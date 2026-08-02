package sigv4_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/fujiwara/s3rp/sigv4"
)

// These exercise the package on its own — with a stub secret lookup rather
// than a store — so it stays verifiable independently of the proxy.

const (
	testAccessKeyID = "AKIDEXAMPLE"
	testSecret      = "secret0123456789"
	emptyPayload    = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

var testTime = time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)

func lookup(_ context.Context, accessKeyID string) (sigv4.Credential, error) {
	if accessKeyID != testAccessKeyID {
		return sigv4.Credential{}, sigv4.ErrUnknownKey
	}
	return sigv4.Credential{SecretAccessKey: testSecret}, nil
}

// signedRequest builds a server-side request signed like a real S3 client.
func signedRequest(t *testing.T, secret string) *http.Request {
	t.Helper()
	const url = "http://s3.example.com/bucket/key.txt"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Amz-Content-Sha256", emptyPayload)
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: secret}
	if err := signer.SignHTTP(context.Background(), creds, req, emptyPayload, "s3", "us-east-1", testTime); err != nil {
		t.Fatal(err)
	}
	sr := httptest.NewRequest("GET", url, nil)
	sr.Header = req.Header
	sr.Host = req.Host
	return sr
}

func newVerifier() *sigv4.Verifier {
	v := sigv4.NewVerifier()
	v.Now = func() time.Time { return testTime }
	return v
}

func TestVerifyHeaderAuth(t *testing.T) {
	got, err := newVerifier().Verify(signedRequest(t, testSecret), lookup)
	if err != nil {
		t.Fatalf("expect success, got %v", err)
	}
	if got.AccessKeyID != testAccessKeyID {
		t.Errorf("unexpected access key %q", got.AccessKeyID)
	}
	if got.SecretAccessKey != testSecret {
		t.Error("the secret must be carried through for chunked verification")
	}
	if got.Region != "us-east-1" || got.PayloadHash != emptyPayload {
		t.Errorf("unexpected verified request %+v", got)
	}
	if !got.SigningTime.Equal(testTime) {
		t.Errorf("unexpected signing time %v", got.SigningTime)
	}
}

func TestVerifyRejects(t *testing.T) {
	cases := []struct {
		name    string
		request func(*testing.T) *http.Request
		code    string
	}{
		{
			name:    "wrong secret",
			request: func(t *testing.T) *http.Request { return signedRequest(t, "wrong-secret-value") },
			code:    "SignatureDoesNotMatch",
		},
		{
			name: "unknown access key",
			request: func(t *testing.T) *http.Request {
				r := signedRequest(t, testSecret)
				r.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential=NOSUCHKEY/20260801/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-content-sha256;x-amz-date, Signature=deadbeef")
				return r
			},
			code: "InvalidAccessKeyId",
		},
		{
			name: "no authentication at all",
			request: func(t *testing.T) *http.Request {
				return httptest.NewRequest("GET", "http://s3.example.com/bucket/key.txt", nil)
			},
			code: "AccessDenied",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newVerifier().Verify(tc.request(t), lookup)
			if err == nil {
				t.Fatal("expect an error")
			}
			if err.Code != tc.code {
				t.Errorf("expect %s, got %s (%s)", tc.code, err.Code, err.Message)
			}
		})
	}
}

// a lookup failure that is not ErrUnknownKey must not be reported as a bad
// key, since that would tell a client its credentials are wrong when the
// service is merely broken
func TestVerifyLookupFailure(t *testing.T) {
	failing := func(context.Context, string) (sigv4.Credential, error) {
		return sigv4.Credential{}, errors.New("database is down")
	}
	_, err := newVerifier().Verify(signedRequest(t, testSecret), failing)
	if err == nil {
		t.Fatal("expect an error")
	}
	if err.Code != "InternalError" {
		t.Errorf("expect InternalError, got %s", err.Code)
	}
}

func TestIsStreaming(t *testing.T) {
	for _, s := range []string{
		"STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
		"STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER",
		"STREAMING-UNSIGNED-PAYLOAD-TRAILER",
	} {
		if !sigv4.IsStreaming(s) {
			t.Errorf("%s must be streaming", s)
		}
	}
	for _, s := range []string{"UNSIGNED-PAYLOAD", emptyPayload, ""} {
		if sigv4.IsStreaming(s) {
			t.Errorf("%s must not be streaming", s)
		}
	}
}
