package s3rp

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

// S3Error is an S3 API error response.
// https://docs.aws.amazon.com/AmazonS3/latest/API/ErrorResponses.html
type S3Error struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource,omitempty"`
	RequestID string   `xml:"RequestId,omitempty"`

	status int
}

func (e *S3Error) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *S3Error) Status() int {
	return e.status
}

func newS3Error(status int, code, message string) *S3Error {
	return &S3Error{status: status, Code: code, Message: message}
}

var (
	errNotImplemented = func(what string) *S3Error {
		return newS3Error(http.StatusNotImplemented, "NotImplemented", fmt.Sprintf("%s is not implemented", what))
	}
	errAccessDenied = func() *S3Error {
		return newS3Error(http.StatusForbidden, "AccessDenied", "Access Denied")
	}
	errInvalidAccessKeyID = func() *S3Error {
		return newS3Error(http.StatusForbidden, "InvalidAccessKeyId", "The AWS Access Key Id you provided does not exist in our records.")
	}
	errSignatureDoesNotMatch = func() *S3Error {
		return newS3Error(http.StatusForbidden, "SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided. Check your key and signing method.")
	}
	errContentSHA256Mismatch = func() *S3Error {
		return newS3Error(http.StatusBadRequest, "XAmzContentSHA256Mismatch", "The provided 'x-amz-content-sha256' header does not match what was computed.")
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

// fromSDKError converts an error returned by the aws-sdk-go-v2 S3 client
// into an S3Error, preserving the backend's error code and HTTP status
// where possible.
func fromSDKError(err error, resource string) *S3Error {
	// an S3Error raised by the proxy itself (e.g. the chunked reader
	// aborting on a signature or checksum mismatch) passes through
	var own *S3Error
	if errors.As(err, &own) {
		if own.Resource == "" {
			own.Resource = resource
		}
		return own
	}
	s3err := &S3Error{
		status:   http.StatusBadGateway,
		Code:     "InternalError",
		Message:  "We encountered an internal error. Please try again.",
		Resource: resource,
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

// writeS3Error writes an S3 XML error response.
// HEAD responses and 304 must not carry a body.
func writeS3Error(w http.ResponseWriter, r *http.Request, e *S3Error, requestID string) {
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

func newRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}
