package s3gw_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// TestSSEHeaderValidation covers what the SDK cannot be made to send:
// malformed SSE values on the wire.
func TestSSEHeaderValidation(t *testing.T) {
	cases := []struct {
		name    string
		headers map[string]string
		code    string
	}{
		{
			name:    "unsupported encryption method",
			headers: map[string]string{"x-amz-server-side-encryption": "aws:kms:dsse"},
			code:    "InvalidArgument",
		},
		{
			name:    "kms key id without aws:kms",
			headers: map[string]string{"x-amz-server-side-encryption-aws-kms-key-id": "some-key"},
			code:    "InvalidArgument",
		},
		{
			name: "kms key id with AES256",
			headers: map[string]string{
				"x-amz-server-side-encryption":                "AES256",
				"x-amz-server-side-encryption-aws-kms-key-id": "some-key",
			},
			code: "InvalidArgument",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newTestGateway(t)
			stub := &stubPost{}
			if err := gw.SetBackend("testbucket", stub); err != nil {
				t.Fatal(err)
			}
			req := signedRequest(t, "PUT", "http://s3.example.com/testbucket/a.txt",
				[]byte("x"), "UNSIGNED-PAYLOAD", time.Now(), testCreds(), func(r *http.Request) {
					for k, v := range tc.headers {
						r.Header.Set(k, v)
					}
				})
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.code) {
				t.Errorf("expect %s, got %d: %s", tc.code, w.Code, w.Body.String())
			}
			if stub.putIn != nil {
				t.Error("a refused request must not reach the backend")
			}
		})
	}
}

// TestPostObjectSSE checks the POST form path forwards the SSE fields.
func TestPostObjectSSE(t *testing.T) {
	gw := newTestGateway(t)
	stub := &stubPost{
		putOut: &s3.PutObjectOutput{
			ETag:                 aws.String(`"post-etag"`),
			ServerSideEncryption: types.ServerSideEncryptionAwsKms,
			SSEKMSKeyId:          aws.String("tenant-key-1"),
		},
	}
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	form := &postForm{
		conditions: []string{
			`{"key": "a.txt"}`,
			`{"x-amz-server-side-encryption": "aws:kms"}`,
			`{"x-amz-server-side-encryption-aws-kms-key-id": "tenant-key-1"}`,
		},
		fields: [][2]string{
			{"key", "a.txt"},
			{"x-amz-server-side-encryption", "aws:kms"},
			{"x-amz-server-side-encryption-aws-kms-key-id", "tenant-key-1"},
		},
		filename: "x", content: "secret",
	}
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, form.request(t))
	if w.Code != http.StatusNoContent {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}
	if stub.putIn.ServerSideEncryption != types.ServerSideEncryptionAwsKms ||
		aws.ToString(stub.putIn.SSEKMSKeyId) != "tenant-key-1" {
		t.Errorf("expect the SSE fields at the backend, got %q %v",
			stub.putIn.ServerSideEncryption, stub.putIn.SSEKMSKeyId)
	}
	if got := w.Header().Get("x-amz-server-side-encryption"); got != "aws:kms" {
		t.Errorf("expect the encryption result header, got %q", got)
	}
	if got := w.Header().Get("x-amz-server-side-encryption-aws-kms-key-id"); got != "tenant-key-1" {
		t.Errorf("expect the key id header, got %q", got)
	}
}
