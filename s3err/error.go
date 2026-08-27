// Package s3err renders S3 API error responses and maps aws-sdk-go-v2
// errors onto them, preserving the backend's error code and HTTP status. It
// is independent of the proxy so any service speaking the S3 API can reuse it.
package s3err

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
)

// Error is an S3 API error response, as specified in "Error responses" in
// the S3 API reference.
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
	"NoSuchKey":    http.StatusNotFound,
	"NoSuchBucket": http.StatusNotFound,
	"NotFound":     http.StatusNotFound,
	"ServerSideEncryptionConfigurationNotFoundError": http.StatusNotFound,
	"AccessDenied":       http.StatusForbidden,
	"PreconditionFailed": http.StatusPreconditionFailed,
	"NotModified":        http.StatusNotModified,
	"InvalidRange":       http.StatusRequestedRangeNotSatisfiable,
}

// canonicalMessage gives each S3 error code the message Amazon S3 uses for
// it. A backend's own message is never passed on to the client: it is
// written by software the tenant does not know about and may name what the
// proxy exists to hide — Amazon S3's authorization failures, for one, quote
// the caller's IAM ARN and the real bucket ARN, which here are the
// operator's. The backend's wording is kept as the cause, so it reaches the
// operator's log and only there. Canonical messages also make the surface
// uniform: for one missing key, versitygw says "The specified key does not
// exist." and Ceph RGW says "NoSuchKey".
var canonicalMessage = map[string]string{
	"AccessDenied":                    "Access Denied",
	"AccountProblem":                  "There is a problem with your account that prevents the operation from completing successfully.",
	"BadDigest":                       "The Content-MD5 or checksum value that you specified did not match what the server received.",
	"BucketAlreadyExists":             "The requested bucket name is not available.",
	"BucketAlreadyOwnedByYou":         "The bucket that you tried to create already exists, and you own it.",
	"BucketNotEmpty":                  "The bucket that you tried to delete is not empty.",
	"EntityTooLarge":                  "Your proposed upload exceeds the maximum allowed size.",
	"EntityTooSmall":                  "Your proposed upload is smaller than the minimum allowed size.",
	"InternalError":                   "We encountered an internal error. Please try again.",
	"InvalidArgument":                 "Invalid Argument",
	"InvalidBucketName":               "The specified bucket is not valid.",
	"InvalidDigest":                   "The Content-MD5 or checksum value that you specified is not valid.",
	"InvalidObjectState":              "The operation is not valid for the current state of the object.",
	"InvalidPart":                     "One or more of the specified parts could not be found.",
	"InvalidPartOrder":                "The list of parts was not in ascending order. Parts list must be specified in order by part number.",
	"InvalidRange":                    "The requested range is not satisfiable.",
	"InvalidRequest":                  "Invalid Request",
	"InvalidToken":                    "The provided token is malformed or otherwise invalid.",
	"MalformedXML":                    "The XML that you provided was not well formed or did not validate against our published schema.",
	"MethodNotAllowed":                "The specified method is not allowed against this resource.",
	"MissingContentLength":            "You must provide the Content-Length HTTP header.",
	"NoSuchBucket":                    "The specified bucket does not exist.",
	"NoSuchBucketPolicy":              "The bucket policy does not exist.",
	"NoSuchCORSConfiguration":         "The CORS configuration does not exist.",
	"NoSuchKey":                       "The specified key does not exist.",
	"NoSuchLifecycleConfiguration":    "The lifecycle configuration does not exist.",
	"NoSuchUpload":                    "The specified multipart upload does not exist. The upload ID might be invalid, or the multipart upload might have been aborted or completed.",
	"NoSuchVersion":                   "The version ID specified in the request does not match an existing version.",
	"NotFound":                        "Not Found",
	"NotImplemented":                  "A header that you provided implies functionality that is not implemented.",
	"ObjectLockConfigurationNotFound": "Object Lock configuration does not exist for this bucket.",
	"ServerSideEncryptionConfigurationNotFoundError": "The server side encryption configuration was not found",
	"PreconditionFailed":                             "At least one of the preconditions that you specified did not hold.",
	"RequestTimeout":                                 "Your socket connection to the server was not read from or written to within the timeout period.",
	"ServiceUnavailable":                             "Service is unable to handle request.",
	"SlowDown":                                       "Please reduce your request rate.",
	"TooManyBuckets":                                 "You have attempted to create more buckets than allowed.",
}

// messageFor returns what the client is told for an error code. An
// unrecognized code (a backend extension, or a newer S3 error than this
// table) is reported as the code itself, which is what the code already
// falls back to when a backend sends no message at all.
func messageFor(code string) string {
	if msg, ok := canonicalMessage[code]; ok {
		return msg
	}
	return code
}

// FromSDKError converts an error returned by the aws-sdk-go-v2 S3 client
// into an Error, preserving the backend's error code and HTTP status
// where possible.
func FromSDKError(err error, resource string) *Error {
	// an Error raised by the proxy itself (e.g. the chunked reader
	// aborting on a signature or checksum mismatch) passes through
	if own, ok := errors.AsType[*Error](err); ok {
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
	if respErr, ok := errors.AsType[*awshttp.ResponseError](err); ok {
		// a transport failure (dial error, connection reset) still carries
		// a ResponseError, with a zero status; adopting it would make the
		// response unwritable (WriteHeader panics below 100)
		if status := respErr.HTTPStatusCode(); status >= 100 {
			s3err.status = status
			hasStatus = true
		}
	}
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		code := apiErr.ErrorCode()
		s3err.Code = code
		// the backend's own wording stays in the cause (set above), which
		// only the observer sees; the client gets the canonical message
		s3err.Message = messageFor(code)
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
