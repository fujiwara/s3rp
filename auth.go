package s3rp

import (
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
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

// verifyRequest authenticates an incoming request by re-signing a clone of it
// with the secret of the access key in the Authorization header and comparing
// the signatures.
func (app *S3RP) verifyRequest(r *http.Request) (*verifiedRequest, *S3Error) {
	if r.URL.Query().Get("X-Amz-Signature") != "" {
		return nil, errNotImplemented("presigned URL")
	}
	authValue := r.Header.Get("Authorization")
	if authValue == "" {
		return nil, errAccessDenied()
	}
	auth, err := parseAuthorizationHeader(authValue)
	if err != nil {
		return nil, newS3Error(http.StatusBadRequest, "AuthorizationHeaderMalformed",
			fmt.Sprintf("The authorization header is malformed; %s.", err))
	}
	if auth.Service != "s3" {
		return nil, newS3Error(http.StatusBadRequest, "AuthorizationHeaderMalformed",
			"The authorization header is malformed; incorrect service.")
	}
	secret, ok := app.keys[auth.AccessKeyID]
	if !ok {
		return nil, errInvalidAccessKeyID()
	}

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
