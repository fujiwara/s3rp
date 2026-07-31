package s3rp

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
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

// verifiedRequest carries the results of a successful signature verification.
// Signature and SecretAccessKey are needed later to verify aws-chunked
// (STREAMING-AWS4-HMAC-SHA256-PAYLOAD) chunk signatures.
type verifiedRequest struct {
	AccessKeyID     string
	SecretAccessKey Password
	Signature       string
	SigningTime     time.Time
	Scope           string
	Region          string
	PayloadHash     string
}

// verifyRequest authenticates an incoming request, either by the
// Authorization header or by presigned URL query parameters.
func (app *S3RP) verifyRequest(r *http.Request) (*verifiedRequest, *S3Error) {
	hasQueryAuth := r.URL.Query().Get("X-Amz-Signature") != ""
	hasHeaderAuth := r.Header.Get("Authorization") != ""
	switch {
	case hasQueryAuth && hasHeaderAuth:
		return nil, newS3Error(http.StatusBadRequest, "InvalidArgument",
			"Only one auth mechanism allowed; only the X-Amz-Algorithm query parameter, Signature query string parameter or the Authorization header should be specified")
	case hasQueryAuth:
		return app.verifyPresignedRequest(r)
	case hasHeaderAuth:
		return app.verifyHeaderRequest(r)
	default:
		return nil, errAccessDenied()
	}
}

// verifyHeaderRequest authenticates a request signed via the Authorization
// header by re-signing a clone of it with the secret of the access key and
// comparing the signatures.
func (app *S3RP) verifyHeaderRequest(r *http.Request) (*verifiedRequest, *S3Error) {
	authValue := r.Header.Get("Authorization")
	auth, err := parseAuthorizationHeader(authValue)
	if err != nil {
		return nil, newS3Error(http.StatusBadRequest, "AuthorizationHeaderMalformed",
			fmt.Sprintf("The authorization header is malformed; %s.", err))
	}
	if auth.Service != "s3" {
		return nil, newS3Error(http.StatusBadRequest, "AuthorizationHeaderMalformed",
			"The authorization header is malformed; incorrect service.")
	}
	fk, ok := app.keys[auth.AccessKeyID]
	if !ok {
		return nil, errInvalidAccessKeyID()
	}
	secret := fk.secret

	amzDate := r.Header.Get(amzDateHeader)
	if amzDate == "" {
		return nil, newS3Error(http.StatusBadRequest, "InvalidRequest",
			"Missing required header for this request: x-amz-date")
	}
	t, err := time.Parse(amzDateFormat, amzDate)
	if err != nil {
		return nil, newS3Error(http.StatusBadRequest, "InvalidRequest",
			"Invalid x-amz-date header")
	}
	if t.Format("20060102") != auth.Date {
		return nil, newS3Error(http.StatusBadRequest, "AuthorizationHeaderMalformed",
			"The authorization header is malformed; the date in the credential scope does not match the x-amz-date header.")
	}
	now := app.now()
	if d := now.Sub(t); d > maxClockSkew || d < -maxClockSkew {
		return nil, newS3Error(http.StatusForbidden, "RequestTimeTooSkewed",
			"The difference between the request time and the current time is too large.")
	}

	payloadHash := r.Header.Get(amzContentSha256)
	if payloadHash == "" {
		return nil, newS3Error(http.StatusBadRequest, "InvalidRequest",
			"Missing required header for this request: x-amz-content-sha256")
	}

	clone, err := cloneForSigning(r, auth.SignedHeaders)
	if err != nil {
		return nil, newS3Error(http.StatusBadRequest, "InvalidRequest", err.Error())
	}
	creds := aws.Credentials{
		AccessKeyID:     auth.AccessKeyID,
		SecretAccessKey: secret.String(),
	}
	if err := app.signer.SignHTTP(r.Context(), creds, clone, payloadHash, auth.Service, auth.Region, t); err != nil {
		slog.ErrorContext(r.Context(), "failed to sign request for verification", "error", err)
		return nil, newS3Error(http.StatusInternalServerError, "InternalError", "signing failed")
	}
	signed, err := parseAuthorizationHeader(clone.Header.Get("Authorization"))
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to parse re-signed authorization header", "error", err)
		return nil, newS3Error(http.StatusInternalServerError, "InternalError", "signing failed")
	}
	sigOK := subtle.ConstantTimeCompare([]byte(signed.Signature), []byte(auth.Signature)) == 1
	headersOK := strings.Join(signed.SignedHeaders, ";") == strings.Join(auth.SignedHeaders, ";")
	if !sigOK || !headersOK {
		slog.DebugContext(r.Context(), "signature mismatch",
			"client_signed_headers", strings.Join(auth.SignedHeaders, ";"),
			"resigned_headers", strings.Join(signed.SignedHeaders, ";"),
		)
		return nil, errSignatureDoesNotMatch()
	}
	return &verifiedRequest{
		AccessKeyID:     auth.AccessKeyID,
		SecretAccessKey: secret,
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
// the signatures.
// https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-query-string-auth.html
func (app *S3RP) verifyPresignedRequest(r *http.Request) (*verifiedRequest, *S3Error) {
	query := r.URL.Query()
	queryParamsError := func(msg string) *S3Error {
		return newS3Error(http.StatusBadRequest, "AuthorizationQueryParametersError", msg)
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
	now := app.now()
	if now.After(t.Add(time.Duration(expires) * time.Second)) {
		return nil, newS3Error(http.StatusForbidden, "AccessDenied", "Request has expired")
	}
	if t.After(now.Add(maxClockSkew)) {
		return nil, newS3Error(http.StatusForbidden, "AccessDenied", "Request is not valid yet")
	}
	fk, ok := app.keys[akid]
	if !ok {
		return nil, errInvalidAccessKeyID()
	}
	secret := fk.secret

	clone, err := cloneForSigning(r, signedHeaders)
	if err != nil {
		return nil, newS3Error(http.StatusBadRequest, "InvalidRequest", err.Error())
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
	creds := aws.Credentials{AccessKeyID: akid, SecretAccessKey: secret.String()}
	signedURI, _, err := app.signer.PresignHTTP(r.Context(), creds, clone, payloadHash, service, region, t)
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to presign request for verification", "error", err)
		return nil, newS3Error(http.StatusInternalServerError, "InternalError", "signing failed")
	}
	signedURL, err := url.Parse(signedURI)
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to parse re-presigned URL", "error", err)
		return nil, newS3Error(http.StatusInternalServerError, "InternalError", "signing failed")
	}
	signedQuery := signedURL.Query()
	sigOK := subtle.ConstantTimeCompare(
		[]byte(signedQuery.Get("X-Amz-Signature")), []byte(query.Get("X-Amz-Signature"))) == 1
	headersOK := signedQuery.Get("X-Amz-SignedHeaders") == query.Get("X-Amz-SignedHeaders")
	if !sigOK || !headersOK {
		slog.DebugContext(r.Context(), "presigned signature mismatch",
			"client_signed_headers", query.Get("X-Amz-SignedHeaders"),
			"resigned_headers", signedQuery.Get("X-Amz-SignedHeaders"),
		)
		return nil, errSignatureDoesNotMatch()
	}
	promoteHoistedQueryParams(r)
	return &verifiedRequest{
		AccessKeyID:     akid,
		SecretAccessKey: secret,
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
