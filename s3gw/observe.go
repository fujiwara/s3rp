package s3gw

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/sigv4"
)

// RequestInfo describes a finished request. The gateway does not log: it
// reports what it knows here and leaves the format, the level and the
// destination to the service, which is the one that has to live with them.
//
// Everything a failure can be explained by is present, so an observer is
// enough to write both an access log and a failure log.
type RequestInfo struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	// Host is the Host header as sent. With virtual-hosted-style addressing
	// it is where the bucket is named, and Path alone does not identify
	// what a request refused before reaching an operation was aimed at.
	Host       string `json:"host"`
	RemoteAddr string `json:"remote_addr"`
	// UserAgent is the User-Agent header, cut to maxUserAgentLen bytes. It
	// is for diagnosis only — which SDK and version sent a request the
	// gateway refused — and is whatever the client chose to send: no
	// signature covers it, so nothing may be decided on it.
	UserAgent string `json:"user_agent,omitempty"`
	// RawQuery has the presigned authentication parameters masked. Log this
	// rather than the request's own query string: a presigned URL's signature
	// is a bearer credential until it expires, and recognizing one is this
	// package's job, not the caller's.
	RawQuery string `json:"raw_query"`
	// RequestID is the value returned to the client in x-amz-request-id, so a
	// request a user reports can be found in the log.
	RequestID string `json:"request_id"`
	Status    int    `json:"status"`
	// Code is the S3 error code the client was given, empty on success —
	// and empty as well for a failure discovered after the response had
	// started, where there was no way left to tell the client anything.
	// Err says what happened in both cases.
	Code string `json:"code,omitempty"`
	// Message is the error message the client was given with Code. It is
	// what tells apart refusals that share a code — which header or query
	// parameter a NotImplemented refused — and those carry no Err.
	Message string `json:"message,omitempty"`
	// Tenant and User are the identity the signature proved, known from the
	// moment it verifies — so they are recorded even for a request that is
	// then refused, which is when knowing who asked matters most. They are
	// empty when the signature itself did not verify.
	Tenant string `json:"tenant,omitempty"`
	User   string `json:"user,omitempty"`
	// AccessKeyID is the key the signature was made with, set together with
	// Tenant and User. A user can hold several keys at once (that is how
	// rotation works), so key-level accounting — a last-used timestamp that
	// tells whether an old key is still in use — needs the key, not just the
	// user. Key ids are identifiers, not secrets.
	AccessKeyID string `json:"access_key_id,omitempty"`
	// Route is the S3 operation the request was routed to, set as soon
	// as the signature verifies and before anything else is checked — so a
	// request refused for a header, a query parameter, an unknown bucket or
	// a denied action still says what was refused. It is the route that
	// matched, not what the client meant: an unsupported subresource on a
	// bucket GET is recorded as the listing it fell through to. Empty when
	// the signature did not verify; a browser POST upload is told by its
	// shape and is named before its policy is verified.
	Route string `json:"route,omitempty"`
	// Op is the operation as the hooks saw it, present only once routing and
	// the policies have passed. A request refused before that has none,
	// which is why Route and the identity above are kept separately.
	Op *Op `json:"op,omitzero"`
	// Err is what actually went wrong, when anything did. It is never sent to
	// the client — it may name backend endpoints and buckets — so this is the
	// only place it can be recorded.
	// It is rendered by MarshalJSON as its message: an error marshals to an
	// empty object on its own, which would silently lose the reason.
	Err error `json:"-"`
	// BytesIn and BytesOut count the request and response bodies as seen on
	// the wire, for the whole request including failures.
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`
	// Start is when the request began, and with Duration brackets it. It is
	// here so the record stands on its own: an observer that hands it to
	// something else — a metering queue, a batch — must not have to stamp the
	// time itself at the moment it is called.
	Start time.Time `json:"start"`
	// Duration marshals as a count of nanoseconds, as time.Duration does.
	Duration time.Duration `json:"duration"`
}

// MarshalJSON renders the record, including the reason for a failure, so a
// service that emits its log as JSON can hand this over as it is.
func (i RequestInfo) MarshalJSON() ([]byte, error) {
	type info RequestInfo // shed the method, so this does not recurse
	var msg string
	if i.Err != nil {
		msg = i.Err.Error()
	}
	return json.Marshal(struct {
		info
		// json has no representation of a time.Duration of its own
		Duration int64  `json:"duration"`
		Error    string `json:"error,omitempty"`
	}{info(i), i.Duration.Nanoseconds(), msg},
		json.Deterministic(true),
		// what a client sent need not be UTF-8, and the record of a request
		// must not be lost to that
		jsontext.AllowInvalidUTF8(true))
}

// maxUserAgentLen bounds RequestInfo.UserAgent. The SDKs' own are a few
// hundred bytes at most; the header itself is limited only by the server's
// header size, and it goes into every log line.
const maxUserAgentLen = 512

func clipUserAgent(ua string) string {
	if len(ua) <= maxUserAgentLen {
		return ua
	}
	// not in the middle of a character: the record is emitted as JSON
	return strings.ToValidUTF8(ua[:maxUserAgentLen], "")
}

type infoKey struct{}

// recordOf returns the record being assembled for the request, or nil when
// there is no observer to receive it, or when a handler was invoked outside
// wrapHandler as tests do.
func recordOf(ctx context.Context) *RequestInfo {
	info, _ := ctx.Value(infoKey{}).(*RequestInfo)
	return info
}

// recordOperation records the operation a request was routed to.
func recordOperation(ctx context.Context, name string) {
	if info := recordOf(ctx); info != nil {
		info.Route = name
	}
}

// recordPresentedKey records, for a request whose authentication failed,
// the access key id it claimed — unverified, which is why Tenant and User
// stay empty: the key proves nothing, but which key was tried is what a
// leaked-key hunt or a rotation left-behind needs.
func recordPresentedKey(ctx context.Context, s3e *s3err.Error) {
	info := recordOf(ctx)
	if info == nil {
		return
	}
	if f, ok := errors.AsType[*sigv4.AuthFailure](s3e); ok {
		info.AccessKeyID = f.AccessKeyID
	}
}

// Observer is called once per request, after the response has been written.
type Observer func(ctx context.Context, info *RequestInfo)

// SetObserver installs the observer called at the end of every request.
// Without one the gateway is silent, so a service that installs nothing gets
// no log at all — including for failures, whose cause is not recoverable
// anywhere else.
func (g *Gateway) SetObserver(o Observer) { g.observer = o }

// SetRequestID replaces how the x-amz-request-id of each request is chosen.
// The function is called once per request, before anything else happens,
// with the request as the wrapping handler passed it — so an id derived from
// what that handler put in the context (a trace id, an id from the hop in
// front) ties the id the client is told, the log line and the trace
// together. An empty result falls back to the gateway's own random id.
//
// The gateway does not trust any header for this: a value taken from one
// (a traceparent, an X-Request-Id) is the caller's decision, and must come
// from the hop in front, never from the client — the same rule as the
// client address.
func (g *Gateway) SetRequestID(f func(r *http.Request) string) { g.requestID = f }

// newRequestID is the id a request is told and logged under.
func (g *Gateway) newRequestID(r *http.Request) string {
	if g.requestID != nil {
		if id := g.requestID(r); id != "" {
			return id
		}
	}
	return randomRequestID()
}

// randomRequestID is the gateway's own id: 8 random bytes as hex.
func randomRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
