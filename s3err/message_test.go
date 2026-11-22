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

// TestCanonicalMessagesVerified pins the messages that were checked against
// Amazon S3 itself: each is asserted literally by a fidelity test of
// aws-emulator-evaluation-suite (commit abd08117, behavioral/s3), whose
// tests were verified against real AWS accounts. The documentation's error
// table words several of them differently, so do not "correct" one from
// the docs.
func TestCanonicalMessagesVerified(t *testing.T) {
	for code, want := range map[string]string{
		"AccessDenied":                          "Access Denied",
		"BucketAlreadyOwnedByYou":               "Your previous request to create the named bucket succeeded and you already own it.",
		"BucketNotEmpty":                        "The bucket you tried to delete is not empty",
		"EntityTooSmall":                        "Your proposed upload is smaller than the minimum allowed size",
		"InvalidBucketName":                     "The specified bucket is not valid.",
		"InvalidObjectState":                    "The operation is not valid for the object's storage class",
		"InvalidPart":                           "One or more of the specified parts could not be found.  The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.",
		"InvalidPartOrder":                      "The list of parts was not in ascending order. Parts must be ordered by part number.",
		"InvalidRange":                          "The requested range is not satisfiable",
		"MalformedXML":                          "The XML you provided was not well-formed or did not validate against our published schema",
		"MethodNotAllowed":                      "The specified method is not allowed against this resource.",
		"MissingContentLength":                  "You must provide the Content-Length HTTP header.",
		"NoSuchBucket":                          "The specified bucket does not exist",
		"NoSuchBucketPolicy":                    "The bucket policy does not exist",
		"NoSuchCORSConfiguration":               "The CORS configuration does not exist",
		"NoSuchKey":                             "The specified key does not exist.",
		"NoSuchLifecycleConfiguration":          "The lifecycle configuration does not exist",
		"NoSuchTagSet":                          "The TagSet does not exist",
		"NoSuchUpload":                          "The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.",
		"NoSuchVersion":                         "The specified version does not exist.",
		"ObjectLockConfigurationNotFound":       "Object Lock configuration does not exist for this bucket",
		"PreconditionFailed":                    "At least one of the pre-conditions you specified did not hold",
		"ReplicationConfigurationNotFoundError": "The replication configuration was not found",
	} {
		e := s3err.FromSDKError(&smithy.GenericAPIError{Code: code, Message: "backend wording"}, "/b/k")
		if e.Message != want {
			t.Errorf("%s: %q, want %q", code, e.Message, want)
		}
	}
}
