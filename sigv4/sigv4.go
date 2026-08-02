// Package sigv4 verifies inbound AWS SigV4 signatures on the server side: it
// re-signs a clone of the request with the client's secret and compares the
// signatures, and decodes the aws-chunked (STREAMING-*) request bodies whose
// per-chunk signatures chain from the request signature.
//
// It is independent of any particular service: callers supply the secret for
// an access key id through a SecretLookup and attach their own identity
// (tenant, user, policies) to the returned Verified.
package sigv4

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"github.com/fujiwara/s3rp/s3err"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
)

const (
	sigV4Algorithm     = "AWS4-HMAC-SHA256"
	amzDateFormat      = "20060102T150405Z"
	amzDateHeader      = "X-Amz-Date"
	amzContentSha256   = "X-Amz-Content-Sha256"
	maxClockSkew       = 15 * time.Minute
	streamingSHA256    = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
	streamingUnsignedT = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	streamingSHA256T   = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
)

type authHeader struct {
	AccessKeyID   string
	Date          string // yyyymmdd of the credential scope
	Region        string
	Service       string
	SignedHeaders []string
	Signature     string
}

func (a *authHeader) scope() string {
	return strings.Join([]string{a.Date, a.Region, a.Service, "aws4_request"}, "/")
}

// parseAuthorizationHeader parses an SigV4 Authorization header:
// AWS4-HMAC-SHA256 Credential=AKID/20230101/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=hex
func parseAuthorizationHeader(v string) (*authHeader, error) {
	algo, rest, ok := strings.Cut(v, " ")
	if !ok || algo != sigV4Algorithm {
		return nil, fmt.Errorf("unsupported algorithm")
	}
	a := &authHeader{}
	for part := range strings.SplitSeq(rest, ",") {
		key, value, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			return nil, fmt.Errorf("malformed component %q", part)
		}
		switch key {
		case "Credential":
			elems := strings.Split(value, "/")
			if len(elems) != 5 {
				return nil, fmt.Errorf("malformed credential %q", value)
			}
			if elems[4] != "aws4_request" {
				return nil, fmt.Errorf("credential must end with aws4_request")
			}
			a.AccessKeyID = elems[0]
			a.Date = elems[1]
			a.Region = elems[2]
			a.Service = elems[3]
		case "SignedHeaders":
			a.SignedHeaders = strings.Split(value, ";")
		case "Signature":
			a.Signature = value
		default:
			return nil, fmt.Errorf("unknown component %q", key)
		}
	}
	if a.AccessKeyID == "" || len(a.SignedHeaders) == 0 || a.Signature == "" {
		return nil, fmt.Errorf("missing component")
	}
	return a, nil
}

// Credential is what a SecretLookup returns for an access key id.
type Credential struct {
	SecretAccessKey string
	// SessionToken is set for a temporary credential and empty for a
	// long-lived key. When set, the request must present exactly this
	// token (header, presigned query, or POST form field); when empty, a
	// request presenting a token is refused — a token on a long-lived key
	// is client confusion, not something to drop silently. Expiry is the
	// store's business: an expired key is simply not returned.
	SessionToken string
}

// SecretLookup returns the credential for an access key id. Return
// ErrUnknownKey when the id does not exist, so that the caller's client sees
// InvalidAccessKeyId rather than an internal error.
type SecretLookup func(ctx context.Context, accessKeyID string) (Credential, error)

// ErrUnknownKey reports that an access key id does not exist.
var ErrUnknownKey = errors.New("unknown access key id")

// Verifier verifies inbound SigV4 requests. The zero value is not usable;
// build one with NewVerifier.
type Verifier struct {
	// Now returns the current time, for expiry and clock-skew checks.
	// Defaults to time.Now.
	Now func() time.Time

	signers *signerCache
	region  string
}

// NewVerifier returns a Verifier ready for concurrent use.
func NewVerifier() *Verifier {
	return &Verifier{signers: newSignerCache(defaultSignerSlots)}
}

// SetSignerCacheSize replaces the per-access-key signer cache with one of n
// slots (default 512). The cache is direct-mapped, so n bounds memory to n
// signers (a few hundred bytes each); size it to the number of access keys
// expected to verify concurrently — a displaced key just re-derives its
// signing key on its next request. Values below 1 are treated as 1. Call
// before serving requests: like setting Now, it is not synchronized with
// concurrent verification, and cached signers are discarded.
func (v *Verifier) SetSignerCacheSize(n int) {
	v.signers = newSignerCache(n)
}

// SetRegion pins the signing region this endpoint accepts: a request whose
// credential scope names any other region is refused with the same error
// Amazon S3 gives (AuthorizationHeaderMalformed / the presigned equivalent,
// naming the expected region). Unset (the default), any region verifies —
// the signature commits to the region either way, so pinning adds no
// integrity; what it buys a multi-region deployment is failing fast on
// clients pointed at the wrong regional endpoint, and restoring the blast
// radius of SigV4's (date, region, service)-scoped derived keys. Call before
// serving requests: like Now, it is not synchronized with concurrent
// verification.
func (v *Verifier) SetRegion(region string) {
	v.region = region
}

func (v *Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func lookupSecret(r *http.Request, lookup SecretLookup, accessKeyID string) (Credential, *s3err.Error) {
	cred, err := lookup(r.Context(), accessKeyID)
	if err != nil {
		if errors.Is(err, ErrUnknownKey) {
			return Credential{}, s3err.InvalidAccessKeyID()
		}
		return Credential{}, s3err.Internal(err, "key lookup failed")
	}
	return cred, nil
}

// validateSessionToken enforces the token rules symmetrically: a temporary
// credential must present exactly its token, a long-lived one must present
// none.
func validateSessionToken(expected, presented string) *s3err.Error {
	if expected == "" && presented == "" {
		return nil
	}
	if expected != "" && presented != "" &&
		subtle.ConstantTimeCompare([]byte(expected), []byte(presented)) == 1 {
		return nil
	}
	return s3err.New(http.StatusBadRequest, "InvalidToken",
		"The provided token is malformed or otherwise invalid.")
}

// IsStreaming reports whether a payload hash denotes an aws-chunked body,
// whose framing must be decoded with NewChunkedReader.
func IsStreaming(payloadHash string) bool {
	switch payloadHash {
	case streamingSHA256, streamingSHA256T, streamingUnsignedT:
		return true
	}
	return false
}

// Verified carries the results of a successful signature verification.
// Signature and SecretAccessKey are needed later to verify aws-chunked
// (STREAMING-AWS4-HMAC-SHA256-PAYLOAD) chunk signatures.
type Verified struct {
	AccessKeyID     string
	SecretAccessKey string
	Signature       string
	SigningTime     time.Time
	Scope           string
	Region          string
	PayloadHash     string
}

// Verify authenticates an incoming request, either by the
// Authorization header or by presigned URL query parameters.
func (v *Verifier) Verify(r *http.Request, lookup SecretLookup) (*Verified, *s3err.Error) {
	hasQueryAuth := r.URL.Query().Get("X-Amz-Signature") != ""
	hasHeaderAuth := r.Header.Get("Authorization") != ""
	switch {
	case hasQueryAuth && hasHeaderAuth:
		return nil, s3err.New(http.StatusBadRequest, "InvalidArgument",
			"Only one auth mechanism allowed; only the X-Amz-Algorithm query parameter, Signature query string parameter or the Authorization header should be specified")
	case hasQueryAuth:
		return v.verifyPresignedRequest(r, lookup)
	case hasHeaderAuth:
		return v.verifyHeaderRequest(r, lookup)
	default:
		return nil, s3err.AccessDenied()
	}
}

// verifyHeaderRequest authenticates a request signed via the Authorization
// header by re-signing a clone of it with the secret of the access key and
// comparing the signatures.
func (v *Verifier) verifyHeaderRequest(r *http.Request, lookup SecretLookup) (*Verified, *s3err.Error) {
	authValue := r.Header.Get("Authorization")
	auth, err := parseAuthorizationHeader(authValue)
	if err != nil {
		return nil, s3err.New(http.StatusBadRequest, "AuthorizationHeaderMalformed",
			fmt.Sprintf("The authorization header is malformed; %s.", err))
	}
	if auth.Service != "s3" {
		return nil, s3err.New(http.StatusBadRequest, "AuthorizationHeaderMalformed",
			"The authorization header is malformed; incorrect service.")
	}
	if v.region != "" && auth.Region != v.region {
		// AWS's exact wording: clients recognize it, and it tells the
		// operator of a misdirected client what to fix
		return nil, s3err.New(http.StatusBadRequest, "AuthorizationHeaderMalformed",
			fmt.Sprintf("The authorization header is malformed; the region '%s' is wrong; expecting '%s'", auth.Region, v.region))
	}
	cred, s3e := lookupSecret(r, lookup, auth.AccessKeyID)
	if s3e != nil {
		return nil, s3e
	}
	if s3e := validateSessionToken(cred.SessionToken, r.Header.Get("X-Amz-Security-Token")); s3e != nil {
		return nil, s3e
	}

	amzDate := r.Header.Get(amzDateHeader)
	if amzDate == "" {
		return nil, s3err.New(http.StatusBadRequest, "InvalidRequest",
			"Missing required header for this request: x-amz-date")
	}
	t, err := time.Parse(amzDateFormat, amzDate)
	if err != nil {
		return nil, s3err.New(http.StatusBadRequest, "InvalidRequest",
			"Invalid x-amz-date header")
	}
	if t.Format("20060102") != auth.Date {
		return nil, s3err.New(http.StatusBadRequest, "AuthorizationHeaderMalformed",
			"The authorization header is malformed; the date in the credential scope does not match the x-amz-date header.")
	}
	now := v.now()
	if d := now.Sub(t); d > maxClockSkew || d < -maxClockSkew {
		return nil, s3err.New(http.StatusForbidden, "RequestTimeTooSkewed",
			"The difference between the request time and the current time is too large.")
	}

	payloadHash := r.Header.Get(amzContentSha256)
	if payloadHash == "" {
		return nil, s3err.New(http.StatusBadRequest, "InvalidRequest",
			"Missing required header for this request: x-amz-content-sha256")
	}

	clone, err := cloneForSigning(r, auth.SignedHeaders)
	if err != nil {
		return nil, s3err.New(http.StatusBadRequest, "InvalidRequest", err.Error())
	}
	creds := aws.Credentials{
		AccessKeyID:     auth.AccessKeyID,
		SecretAccessKey: cred.SecretAccessKey,
		SessionToken:    cred.SessionToken,
	}
	signer := v.signers.get(auth.AccessKeyID)
	if err := signer.SignHTTP(r.Context(), creds, clone, payloadHash, auth.Service, auth.Region, t); err != nil {
		return nil, s3err.Internal(err, "signing failed")
	}
	signed, err := parseAuthorizationHeader(clone.Header.Get("Authorization"))
	if err != nil {
		return nil, s3err.Internal(err, "signing failed")
	}
	sigOK := subtle.ConstantTimeCompare([]byte(signed.Signature), []byte(auth.Signature)) == 1
	headersOK := strings.Join(signed.SignedHeaders, ";") == strings.Join(auth.SignedHeaders, ";")
	if !sigOK || !headersOK {
		// which side differs is the first thing anyone debugging a mismatch
		// wants, and it is safe to record: no secret is involved
		return nil, s3err.SignatureDoesNotMatch().WithCause(fmt.Errorf(
			"signature mismatch: client signed headers %q, re-signed %q",
			strings.Join(auth.SignedHeaders, ";"), strings.Join(signed.SignedHeaders, ";")))
	}
	return &Verified{
		AccessKeyID:     auth.AccessKeyID,
		SecretAccessKey: cred.SecretAccessKey,
		Signature:       auth.Signature,
		SigningTime:     t,
		Scope:           auth.scope(),
		Region:          auth.Region,
		PayloadHash:     payloadHash,
	}, nil
}

const maxPresignExpires = 7 * 24 * time.Hour

// presignAuthParams are the query parameters that carry the SigV4
// authentication of a presigned URL. They are removed before re-signing
// (the signer adds them back itself).
var presignAuthParams = []string{
	"X-Amz-Algorithm",
	"X-Amz-Credential",
	"X-Amz-Date",
	"X-Amz-SignedHeaders",
	"X-Amz-Signature",
	"X-Amz-Security-Token",
}

// verifyPresignedRequest authenticates a request signed via query string
// parameters (a presigned URL) by re-presigning a clone of it and comparing
// the signatures. Specified in "Authenticating Requests: Using Query
// Parameters (AWS Signature Version 4)" in the S3 API reference.
func (v *Verifier) verifyPresignedRequest(r *http.Request, lookup SecretLookup) (*Verified, *s3err.Error) {
	query := r.URL.Query()
	queryParamsError := func(msg string) *s3err.Error {
		return s3err.New(http.StatusBadRequest, "AuthorizationQueryParametersError", msg)
	}
	if algo := query.Get("X-Amz-Algorithm"); algo != sigV4Algorithm {
		return nil, queryParamsError("X-Amz-Algorithm only supports \"AWS4-HMAC-SHA256\"")
	}
	credElems := strings.Split(query.Get("X-Amz-Credential"), "/")
	if len(credElems) != 5 || credElems[4] != "aws4_request" {
		return nil, queryParamsError("Error parsing the X-Amz-Credential parameter; the Credential is mal-formed")
	}
	akid, scopeDate, region, service := credElems[0], credElems[1], credElems[2], credElems[3]
	if service != "s3" {
		return nil, queryParamsError("Error parsing the X-Amz-Credential parameter; incorrect service")
	}
	if v.region != "" && region != v.region {
		return nil, queryParamsError(fmt.Sprintf(
			"Error parsing the X-Amz-Credential parameter; the region '%s' is wrong; expecting '%s'", region, v.region))
	}
	signedHeaders := strings.Split(query.Get("X-Amz-SignedHeaders"), ";")
	if len(signedHeaders) == 0 || signedHeaders[0] == "" {
		return nil, queryParamsError("X-Amz-SignedHeaders is required")
	}
	t, err := time.Parse(amzDateFormat, query.Get("X-Amz-Date"))
	if err != nil {
		return nil, queryParamsError("X-Amz-Date must be in the ISO8601 Long Format")
	}
	if t.Format("20060102") != scopeDate {
		return nil, queryParamsError("Invalid credential date. Date is not the same as X-Amz-Date.")
	}
	expires, err := strconv.ParseInt(query.Get("X-Amz-Expires"), 10, 64)
	if err != nil || expires < 1 {
		return nil, queryParamsError("X-Amz-Expires must be a positive integer")
	}
	if time.Duration(expires)*time.Second > maxPresignExpires {
		return nil, queryParamsError("X-Amz-Expires must be less than a week (in seconds); that is, the given X-Amz-Expires must be less than 604800 seconds")
	}
	now := v.now()
	if now.After(t.Add(time.Duration(expires) * time.Second)) {
		return nil, s3err.New(http.StatusForbidden, "AccessDenied", "Request has expired")
	}
	if t.After(now.Add(maxClockSkew)) {
		return nil, s3err.New(http.StatusForbidden, "AccessDenied", "Request is not valid yet")
	}
	cred, s3e := lookupSecret(r, lookup, akid)
	if s3e != nil {
		return nil, s3e
	}
	if s3e := validateSessionToken(cred.SessionToken, query.Get("X-Amz-Security-Token")); s3e != nil {
		return nil, s3e
	}

	clone, err := cloneForSigning(r, signedHeaders)
	if err != nil {
		return nil, s3err.New(http.StatusBadRequest, "InvalidRequest", err.Error())
	}
	// drop the auth params; the signer adds them back when presigning
	cloneQuery := clone.URL.Query()
	for _, p := range presignAuthParams {
		cloneQuery.Del(p)
	}
	clone.URL.RawQuery = cloneQuery.Encode()

	// presigned S3 requests conventionally use UNSIGNED-PAYLOAD
	payloadHash := r.Header.Get(amzContentSha256)
	if payloadHash == "" {
		payloadHash = "UNSIGNED-PAYLOAD"
	}
	// the token was stripped with the other auth params; the signer re-adds
	// it to the query when the credentials carry one, as the client's did
	creds := aws.Credentials{AccessKeyID: akid, SecretAccessKey: cred.SecretAccessKey, SessionToken: cred.SessionToken}
	signedURI, _, err := v.signers.get(akid).PresignHTTP(r.Context(), creds, clone, payloadHash, service, region, t)
	if err != nil {
		return nil, s3err.Internal(err, "signing failed")
	}
	signedURL, err := url.Parse(signedURI)
	if err != nil {
		return nil, s3err.Internal(err, "signing failed")
	}
	signedQuery := signedURL.Query()
	sigOK := subtle.ConstantTimeCompare(
		[]byte(signedQuery.Get("X-Amz-Signature")), []byte(query.Get("X-Amz-Signature"))) == 1
	headersOK := signedQuery.Get("X-Amz-SignedHeaders") == query.Get("X-Amz-SignedHeaders")
	if !sigOK || !headersOK {
		return nil, s3err.SignatureDoesNotMatch().WithCause(fmt.Errorf(
			"presigned signature mismatch: client signed headers %q, re-signed %q",
			query.Get("X-Amz-SignedHeaders"), signedQuery.Get("X-Amz-SignedHeaders")))
	}
	promoteHoistedQueryParams(r)
	return &Verified{
		AccessKeyID:     akid,
		SecretAccessKey: cred.SecretAccessKey,
		Signature:       query.Get("X-Amz-Signature"),
		SigningTime:     t,
		Scope:           strings.Join([]string{scopeDate, region, service, "aws4_request"}, "/"),
		Region:          region,
		PayloadHash:     payloadHash,
	}, nil
}

// promoteHoistedQueryParams copies x-amz-* query parameters (except the auth
// parameters) into the request headers. SDK presigners hoist headers such as
// x-amz-meta-* and x-amz-storage-class into the query string, and the
// operation handlers read them from headers.
func promoteHoistedQueryParams(r *http.Request) {
	authParams := make(map[string]bool, len(presignAuthParams)+1)
	for _, p := range presignAuthParams {
		authParams[strings.ToLower(p)] = true
	}
	authParams["x-amz-expires"] = true
	for k, vs := range r.URL.Query() {
		lk := strings.ToLower(k)
		if !strings.HasPrefix(lk, "x-amz-") || authParams[lk] || len(vs) == 0 {
			continue
		}
		if r.Header.Get(k) == "" {
			r.Header.Set(k, vs[0])
		}
	}
}

// cloneForSigning builds a request containing only the headers the client
// signed, preserving the raw escaping of the request URI so that the
// canonical request matches the client's exactly.
func cloneForSigning(r *http.Request, signedHeaders []string) (*http.Request, error) {
	u, err := url.ParseRequestURI(r.RequestURI)
	if err != nil {
		// not from a server (e.g. tests); fall back to the parsed URL
		u = r.URL
	}
	clone := &http.Request{
		Method: r.Method,
		URL:    u,
		Host:   r.Host,
		Header: make(http.Header, len(signedHeaders)),
	}
	for _, h := range signedHeaders {
		switch h {
		case "host":
			// taken from clone.Host by the signer
		case "content-length":
			// the signer includes content-length based on the
			// request's ContentLength field, not the header
			clone.ContentLength = r.ContentLength
		default:
			ch := http.CanonicalHeaderKey(h)
			if vs, ok := r.Header[ch]; ok {
				clone.Header[ch] = vs
			} else {
				return nil, fmt.Errorf("signed header %q is not present in the request", h)
			}
		}
	}
	return clone, nil
}
