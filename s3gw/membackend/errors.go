package membackend

import (
	"net/http"
	"strings"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// failure is an S3 error response in the making: what a backend would have
// answered. err renders it as the aws-sdk client reports such a response,
// which is the shape the gateway maps (s3err.FromSDKError) and relays
// headers from.
type failure struct {
	status  int
	code    string
	message string
	header  http.Header
}

func fail(status int, code, message string) *failure {
	return &failure{status: status, code: code, message: message}
}

func malformedXML() *failure {
	return fail(http.StatusBadRequest, "MalformedXML",
		"The XML you provided was not well-formed or did not validate against our published schema")
}

func invalidArgument(message string) *failure {
	return fail(http.StatusBadRequest, "InvalidArgument", message)
}

// with adds a response header, for the ones that describe the object on an
// error response (the ETag of a 304, the delete marker of a 404).
func (f *failure) with(name, value string) *failure {
	if f.header == nil {
		f.header = http.Header{}
	}
	f.header.Set(name, value)
	return f
}

// head is the failure as a HEAD request reports it: there is no body to
// carry a code, so the SDK derives one from the status text.
func (f *failure) head() *failure {
	h := *f
	h.code = strings.ReplaceAll(http.StatusText(f.status), " ", "")
	h.message = http.StatusText(f.status)
	return &h
}

func (f *failure) err() error {
	header := f.header
	if header == nil {
		header = http.Header{}
	}
	return &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{StatusCode: f.status, Header: header}},
			Err:      f.apiError(),
		},
	}
}

// apiError uses the SDK's modeled error type where it has one, since
// callers of the SDK client match on those.
func (f *failure) apiError() error {
	msg := &f.message
	switch f.code {
	case "NoSuchKey":
		return &types.NoSuchKey{Message: msg}
	case "NoSuchBucket":
		return &types.NoSuchBucket{Message: msg}
	case "NoSuchUpload":
		return &types.NoSuchUpload{Message: msg}
	case "NotFound":
		return &types.NotFound{Message: msg}
	case "BucketAlreadyOwnedByYou":
		return &types.BucketAlreadyOwnedByYou{Message: msg}
	case "BucketAlreadyExists":
		return &types.BucketAlreadyExists{Message: msg}
	}
	return &smithy.GenericAPIError{Code: f.code, Message: f.message}
}
