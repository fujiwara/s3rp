// Package s3err renders S3 API error responses and maps aws-sdk-go-v2
// errors onto them, preserving the backend's error code and HTTP status. It
// is independent of the proxy so any service speaking the S3 API can reuse it.
package s3err

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
)

// Error is an S3 API error response.
// https://docs.aws.amazon.com/AmazonS3/latest/API/ErrorResponses.html
type Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`

	status int
	// cause is what actually went wrong. It is deliberately unexported and
	// absent from the XML above: it may name backend endpoints, buckets or
	// credentials-adjacent detail, so it belongs in the operator's log and
	// never in a response. Log it once where the request ends, keyed by the
	// request id the client is given, rather than at each failure site.
	cause error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.cause)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

// Unwrap returns the underlying cause, if any, so callers can inspect it with
// errors.Is and errors.As.
func (e *Error) Unwrap() error { return e.cause }

func (e *Error) Status() int {
	return e.status
}

func New(status int, code, message string) *Error {
	return &Error{status: status, Code: code, Message: message}
}

// WithCause records what went wrong behind this error. The cause is never
// rendered to the client.
func (e *Error) WithCause(err error) *Error {
	e.cause = err
	return e
}

// Internal is the error for a failure the client can do nothing about: it
// keeps the cause for the log and tells the client only that something broke.
func Internal(cause error, what string) *Error {
	return &Error{
		status:  http.StatusInternalServerError,
		Code:    "InternalError",
		Message: what,
		cause:   cause,
	}
}

var (
	NotImplemented = func(what string) *Error {
		return New(http.StatusNotImplemented, "NotImplemented", fmt.Sprintf("%s is not implemented", what))
	}
	AccessDenied = func() *Error {
		return New(http.StatusForbidden, "AccessDenied", "Access Denied")
	}
	InvalidAccessKeyID = func() *Error {
		return New(http.StatusForbidden, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.")
	}
	SignatureDoesNotMatch = func() *Error {
		return New(http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided. Check your key and signing method.")
	}
	ContentSHA256Mismatch = func() *Error {
		return New(http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.")
	}
)

// wellKnownErrorStatus maps S3 error codes to HTTP statuses, for errors
// that do not carry an HTTP response (e.g. stubbed clients).
var wellKnownErrorStatus = map[string]int{
	"NoSuchKey":          http.StatusNotFound,
	"NoSuchBucket":       http.StatusNotFound,
	"NotFound":           http.StatusNotFound,
	"AccessDenied":       http.StatusForbidden,
	"PreconditionFailed": http.StatusPreconditionFailed,
	"NotModified":        http.StatusNotModified,
	"InvalidRange":       http.StatusRequestedRangeNotSatisfiable,
}

// FromSDKError converts an error returned by the aws-sdk-go-v2 S3 client
// into an Error, preserving the backend's error code and HTTP status
// where possible.
func FromSDKError(err error, resource string) *Error {
	// an Error raised by the proxy itself (e.g. the chunked reader
	// aborting on a signature or checksum mismatch) passes through
	var own *Error
	if errors.As(err, &own) {
		if own.Resource == "" {
			own.Resource = resource
		}
		return own
	}
	s3err := &Error{
		status:   http.StatusBadGateway,
		Code:     "InternalError",
		Message:  "We encountered an internal error. Please try again.",
		Resource: resource,
		// keep what the SDK reported: a transport failure carries no API
		// error code, so without this the reason would be lost entirely
		cause: err,
	}
	hasStatus := false
	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		s3err.status = respErr.HTTPStatusCode()
		hasStatus = true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		code := apiErr.ErrorCode()
		s3err.Code = code
		s3err.Message = apiErr.ErrorMessage()
		if s3err.Message == "" {
			s3err.Message = code
		}
		if !hasStatus {
			if status, ok := wellKnownErrorStatus[code]; ok {
				s3err.status = status
			}
		}
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		s3err.status = http.StatusGatewayTimeout
		s3err.Code = "InternalError"
		s3err.Message = "backend request timed out"
	}
	return s3err
}

// Write writes an S3 XML error response.
// HEAD responses and 304 must not carry a body.
func Write(w http.ResponseWriter, r *http.Request, e *Error, requestID string) {
	e.RequestID = requestID
	if e.Resource == "" {
		e.Resource = r.URL.Path
	}
	if r.Method == http.MethodHead || e.status == http.StatusNotModified {
		w.WriteHeader(e.status)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(e.status)
	b, err := xml.Marshal(e)
	if err != nil {
		slog.Error("failed to marshal error response", "error", err)
		return
	}
	w.Write([]byte(xml.Header))
	w.Write(b)
}

func NewRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
