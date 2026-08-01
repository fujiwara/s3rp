package s3gw

import (
	"context"
	"encoding/json"
	"time"
)

// RequestInfo describes a finished request. The gateway does not log: it
// reports what it knows here and leaves the format, the level and the
// destination to the service, which is the one that has to live with them.
//
// Everything a failure can be explained by is present, so an observer is
// enough to write both an access log and a failure log.
type RequestInfo struct {
	Method     string `json:"method"`
	Path       string `json:"path"`
	RemoteAddr string `json:"remote_addr"`
	// RawQuery has the presigned authentication parameters masked. Log this
	// rather than the request's own query string: a presigned URL's signature
	// is a bearer credential until it expires, and recognizing one is this
	// package's job, not the caller's.
	RawQuery string `json:"raw_query"`
	// RequestID is the value returned to the client in x-amz-request-id, so a
	// request a user reports can be found in the log.
	RequestID string `json:"request_id"`
	Status    int    `json:"status"`
	// Code is the S3 error code the client was given, empty on success.
	Code string `json:"code,omitempty"`
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
		Error string `json:"error,omitempty"`
	}{info(i), msg})
}

// Observer is called once per request, after the response has been written.
type Observer func(ctx context.Context, info *RequestInfo)

// SetObserver installs the observer called at the end of every request.
// Without one the gateway is silent, so a service that installs nothing gets
// no log at all — including for failures, whose cause is not recoverable
// anywhere else.
func (g *Gateway) SetObserver(o Observer) { g.observer = o }
