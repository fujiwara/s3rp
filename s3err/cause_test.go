package s3err_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/s3err"
)

// The cause exists for the operator's log, so the two properties that matter
// are that it survives to be logged and that it never reaches the client.

var errBackendDown = errors.New("dial tcp 10.0.0.1:9000: connection refused")

func TestCauseIsRecoverable(t *testing.T) {
	cases := []struct {
		name string
		err  *s3err.Error
	}{
		{"Internal", s3err.Internal(errBackendDown, "backend client failed")},
		{"WithCause", s3err.AccessDenied().WithCause(errBackendDown)},
		{"FromSDKError keeps a transport failure", s3err.FromSDKError(errBackendDown, "/b/k")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if !errors.Is(tc.err, errBackendDown) {
				t.Errorf("expect the cause to be reachable with errors.Is, got %v", errors.Unwrap(tc.err))
			}
			if !strings.Contains(tc.err.Error(), "connection refused") {
				t.Errorf("expect the cause in Error(), got %q", tc.err.Error())
			}
		})
	}
}

func TestCauseIsNotSentToTheClient(t *testing.T) {
	// a cause naming internal infrastructure must not leak
	e := s3err.Internal(errors.New("backend s3-internal.example.com bucket tenant-42-raw refused"),
		"We encountered an internal error. Please try again.")
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://s3.example.com/bucket/key", nil)
	s3err.Write(w, r, e, "reqid123")

	body := w.Body.String()
	for _, secret := range []string{"s3-internal.example.com", "tenant-42-raw", "refused"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks %q: %s", secret, body)
		}
	}
	// what the client does get: the code, the generic message and the request
	// id that ties their report to the log line carrying the cause
	for _, want := range []string{"InternalError", "reqid123"} {
		if !strings.Contains(body, want) {
			t.Errorf("expect %q in the response: %s", want, body)
		}
	}
	if w.Code != http.StatusInternalServerError {
		t.Errorf("expect 500, got %d", w.Code)
	}
}

func TestFromSDKErrorPassesOwnErrorThrough(t *testing.T) {
	// an error raised by the proxy itself keeps its own cause when it travels
	// back out through the SDK call site
	own := s3err.New(http.StatusBadRequest, "BadDigest", "checksum mismatch").WithCause(errBackendDown)
	got := s3err.FromSDKError(own, "/b/k")
	if got.Code != "BadDigest" {
		t.Errorf("expect the original code, got %s", got.Code)
	}
	if !errors.Is(got, errBackendDown) {
		t.Error("expect the original cause to survive")
	}
}
