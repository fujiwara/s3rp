package s3err_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/fujiwara/s3rp/s3err"
)

var fromSDKErrorTestCases = []struct {
	name       string
	err        error
	wantCode   string
	wantStatus int
}{
	{
		name: "NoSuchKey with response status",
		err: &smithy.OperationError{
			ServiceID:     "S3",
			OperationName: "GetObject",
			Err: &awshttp.ResponseError{
				ResponseError: &smithyhttp.ResponseError{
					Response: &smithyhttp.Response{
						Response: &http.Response{StatusCode: http.StatusNotFound},
					},
					Err: &smithy.GenericAPIError{Code: "NoSuchKey", Message: "The specified key does not exist."},
				},
			},
		},
		wantCode:   "NoSuchKey",
		wantStatus: http.StatusNotFound,
	},
	{
		name:       "bare api error well-known code",
		err:        &smithy.GenericAPIError{Code: "NoSuchBucket", Message: "The specified bucket does not exist"},
		wantCode:   "NoSuchBucket",
		wantStatus: http.StatusNotFound,
	},
	{
		name:       "bare api error unknown code",
		err:        &smithy.GenericAPIError{Code: "SomeWeirdError", Message: "weird"},
		wantCode:   "SomeWeirdError",
		wantStatus: http.StatusBadGateway,
	},
	{
		name:       "head NotFound",
		err:        &smithy.GenericAPIError{Code: "NotFound", Message: "Not Found"},
		wantCode:   "NotFound",
		wantStatus: http.StatusNotFound,
	},
	{
		name:       "unknown error",
		err:        http.ErrHandlerTimeout,
		wantCode:   "InternalError",
		wantStatus: http.StatusBadGateway,
	},
}

func TestFromSDKError(t *testing.T) {
	for _, tc := range fromSDKErrorTestCases {
		t.Run(tc.name, func(t *testing.T) {
			e := s3err.FromSDKError(tc.err, "/bucket/key")
			if e.Code != tc.wantCode {
				t.Errorf("expect code %s, got %s", tc.wantCode, e.Code)
			}
			if e.Status() != tc.wantStatus {
				t.Errorf("expect status %d, got %d", tc.wantStatus, e.Status())
			}
		})
	}
}

func TestWriteS3Error(t *testing.T) {
	t.Run("GET has XML body", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/bucket/key", nil)
		e := s3err.New(http.StatusForbidden, "AccessDenied", "Access Denied")
		s3err.Write(w, r, e, "reqid123")
		if w.Code != http.StatusForbidden {
			t.Errorf("expect 403, got %d", w.Code)
		}
		body := w.Body.String()
		for _, want := range []string{"<Error>", "<Code>AccessDenied</Code>", "<RequestId>reqid123</RequestId>", "<Resource>/bucket/key</Resource>"} {
			if !strings.Contains(body, want) {
				t.Errorf("body %q must contain %q", body, want)
			}
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/xml" {
			t.Errorf("expect application/xml, got %s", ct)
		}
	})
	t.Run("HEAD has no body", func(t *testing.T) {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodHead, "/bucket/key", nil)
		e := s3err.New(http.StatusNotFound, "NotFound", "Not Found")
		s3err.Write(w, r, e, "reqid123")
		if w.Code != http.StatusNotFound {
			t.Errorf("expect 404, got %d", w.Code)
		}
		if w.Body.Len() != 0 {
			t.Errorf("expect empty body, got %q", w.Body.String())
		}
	})
}
