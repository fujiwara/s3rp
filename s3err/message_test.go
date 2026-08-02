package s3err_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp/s3err"
)

// The backend's own error wording never reaches the client: it is written by
// software the tenant does not know about and may name what the proxy exists
// to hide.
func TestBackendMessageNeverReachesTheClient(t *testing.T) {
	// what Amazon S3 answers when the *operator's* backend credentials are
	// not authorized — it quotes the operator's IAM principal and the real
	// backend bucket
	leaky := &smithy.GenericAPIError{
		Code: "AccessDenied",
		Message: "User: arn:aws:iam::123456789012:user/s3rp-backend is not authorized to perform: " +
			"s3:PutObject on resource: arn:aws:s3:::acme-photos-a1b2/key.txt because no identity-based policy allows it",
	}
	e := s3err.FromSDKError(leaky, "/photos/key.txt")

	if e.Code != "AccessDenied" {
		t.Errorf("expect the backend's code to be preserved, got %s", e.Code)
	}
	if e.Message != "Access Denied" {
		t.Errorf("expect the canonical message, got %q", e.Message)
	}
	for _, secret := range []string{"arn:aws", "123456789012", "s3rp-backend", "acme-photos-a1b2"} {
		if strings.Contains(e.Message, secret) {
			t.Errorf("message leaks %q: %s", secret, e.Message)
		}
	}
	// and it must not reach the wire either
	w := httptest.NewRecorder()
	r := httptest.NewRequest("PUT", "http://s3.example.com/photos/key.txt", nil)
	s3err.Write(w, r, e, "req-1")
	body := w.Body.String()
	for _, secret := range []string{"arn:aws", "123456789012", "s3rp-backend", "acme-photos-a1b2"} {
		if strings.Contains(body, secret) {
			t.Errorf("response body leaks %q: %s", secret, body)
		}
	}

	// the operator still gets the full story: the backend error is the cause
	if !errors.Is(e, error(leaky)) {
		t.Error("expect the backend error to be kept as the cause for the observer")
	}
	if !strings.Contains(e.Error(), "acme-photos-a1b2") {
		t.Errorf("expect the cause to carry the detail for the log, got %v", e)
	}
}

// Backends word the same condition differently (versitygw: "The specified
// key does not exist.", Ceph RGW: "NoSuchKey"); the client sees one wording.
func TestCanonicalMessagesAreUniform(t *testing.T) {
	for _, backendWording := range []string{
		"The specified key does not exist.",
		"NoSuchKey",
		"UnknownError",
		"", // some backends send no message at all
	} {
		e := s3err.FromSDKError(&smithy.GenericAPIError{Code: "NoSuchKey", Message: backendWording}, "/b/k")
		if e.Message != "The specified key does not exist." {
			t.Errorf("backend wording %q produced %q", backendWording, e.Message)
		}
	}
}

// An error code this table does not know is reported as the code itself —
// the same fallback the package already used for an empty message.
func TestUnknownCodeFallsBackToTheCode(t *testing.T) {
	e := s3err.FromSDKError(&smithy.GenericAPIError{
		Code: "SomeBackendSpecificError", Message: "internal detail at 10.0.0.5:9000",
	}, "/b/k")
	if e.Code != "SomeBackendSpecificError" || e.Message != "SomeBackendSpecificError" {
		t.Errorf("unexpected code/message: %s / %q", e.Code, e.Message)
	}
	if strings.Contains(e.Message, "10.0.0.5") {
		t.Errorf("message leaks backend detail: %q", e.Message)
	}
}

// Errors the proxy raises itself keep their own message: they are written
// here, for the tenant, and often carry the specifics the tenant needs.
func TestOwnErrorsKeepTheirMessage(t *testing.T) {
	own := s3err.New(http.StatusBadRequest, "InvalidArgument", "the region 'us-east-1' is wrong; expecting 'ap-northeast-1'")
	e := s3err.FromSDKError(own, "/b/k")
	if e.Message != "the region 'us-east-1' is wrong; expecting 'ap-northeast-1'" {
		t.Errorf("expect the proxy's own message to pass through, got %q", e.Message)
	}
}
