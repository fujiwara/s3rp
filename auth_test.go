package s3rp_test

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/fujiwara/s3rp"
)

const (
	testAccessKeyID     = "S3RPTESTKEY001"
	testSecretAccessKey = "testsecret001"
	emptyPayloadHash    = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func newTestApp(t *testing.T) *s3rp.S3RP {
	t.Helper()
	cfg := &s3rp.Config{
		Tenants: []*s3rp.TenantConfig{
			{
				Name: "testtenant",
				Keys: []*s3rp.KeyConfig{
					{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey},
				},
				Buckets: []*s3rp.BucketConfig{
					{
						Name: "testbucket",
						Backend: &s3rp.BackendConfig{
							Endpoint:        "http://backend.invalid",
							Bucket:          "backend-testbucket",
							AccessKeyID:     "backendkey",
							SecretAccessKey: "backendsecret",
						},
					},
				},
			},
		},
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := s3rp.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

// signedRequest builds a server-side request signed like a real S3 client.
func signedRequest(t *testing.T, method, target string, body []byte, payloadHash string, signTime time.Time, creds aws.Credentials, mod func(r *http.Request)) *http.Request {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, target, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if mod != nil {
		mod(req)
	}
	signer := v4.NewSigner(func(o *v4.SignerOptions) {
		o.DisableURIPathEscaping = true
	})
	if err := signer.SignHTTP(context.Background(), creds, req, payloadHash, "s3", "us-east-1", signTime); err != nil {
		t.Fatal(err)
	}
	// convert to a server-side request
	sr := httptest.NewRequest(method, target, bytes.NewReader(body))
	sr.Header = req.Header
	sr.Host = req.Host
	return sr
}

func testCreds() aws.Credentials {
	return aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey}
}

func TestVerifyRequest(t *testing.T) {
	now := time.Now()

	testCases := []struct {
		name     string
		request  func(t *testing.T) *http.Request
		now      time.Time
		wantCode string // "" means success
	}{
		{
			name: "valid GET",
			request: func(t *testing.T) *http.Request {
				return signedRequest(t, http.MethodGet, "http://proxy.example.com/testbucket/key.txt", nil, emptyPayloadHash, now, testCreds(), nil)
			},
		},
		{
			name: "valid GET with query",
			request: func(t *testing.T) *http.Request {
				return signedRequest(t, http.MethodGet, "http://proxy.example.com/testbucket?list-type=2&prefix=foo%2Fbar", nil, emptyPayloadHash, now, testCreds(), nil)
			},
		},
		{
			name: "valid GET with escaped key",
			request: func(t *testing.T) *http.Request {
				return signedRequest(t, http.MethodGet, "http://proxy.example.com/testbucket/dir/hello%20world%2Bx", nil, emptyPayloadHash, now, testCreds(), nil)
			},
		},
		{
			name: "valid PUT with content-length",
			request: func(t *testing.T) *http.Request {
				return signedRequest(t, http.MethodPut, "http://proxy.example.com/testbucket/key.txt", []byte("hello"), "UNSIGNED-PAYLOAD", now, testCreds(), func(r *http.Request) {
					r.Header.Set("Content-Type", "text/plain")
					r.Header.Set("x-amz-meta-foo", "bar")
				})
			},
		},
		{
			name: "wrong secret",
			request: func(t *testing.T) *http.Request {
				creds := aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: "wrongsecret"}
				return signedRequest(t, http.MethodGet, "http://proxy.example.com/testbucket/key.txt", nil, emptyPayloadHash, now, creds, nil)
			},
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "unknown access key",
			request: func(t *testing.T) *http.Request {
				creds := aws.Credentials{AccessKeyID: "UNKNOWNKEY", SecretAccessKey: testSecretAccessKey}
				return signedRequest(t, http.MethodGet, "http://proxy.example.com/testbucket/key.txt", nil, emptyPayloadHash, now, creds, nil)
			},
			wantCode: "InvalidAccessKeyId",
		},
		{
			name: "tampered signed header",
			request: func(t *testing.T) *http.Request {
				r := signedRequest(t, http.MethodGet, "http://proxy.example.com/testbucket/key.txt", nil, emptyPayloadHash, now, testCreds(), func(r *http.Request) {
					r.Header.Set("x-amz-meta-foo", "original")
				})
				r.Header.Set("x-amz-meta-foo", "tampered")
				return r
			},
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "tampered path",
			request: func(t *testing.T) *http.Request {
				r := signedRequest(t, http.MethodGet, "http://proxy.example.com/testbucket/key.txt", nil, emptyPayloadHash, now, testCreds(), nil)
				r.URL.Path = "/testbucket/other.txt"
				r.RequestURI = "/testbucket/other.txt"
				return r
			},
			wantCode: "SignatureDoesNotMatch",
		},
		{
			name: "clock skew",
			request: func(t *testing.T) *http.Request {
				return signedRequest(t, http.MethodGet, "http://proxy.example.com/testbucket/key.txt", nil, emptyPayloadHash, now, testCreds(), nil)
			},
			now:      now.Add(20 * time.Minute),
			wantCode: "RequestTimeTooSkewed",
		},
		{
			name: "presigned with malformed credential",
			request: func(t *testing.T) *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://proxy.example.com/testbucket/key.txt?X-Amz-Signature=deadbeef&X-Amz-Algorithm=AWS4-HMAC-SHA256", nil)
			},
			wantCode: "AuthorizationQueryParametersError",
		},
		{
			name: "both header and query auth",
			request: func(t *testing.T) *http.Request {
				r := signedRequest(t, http.MethodGet, "http://proxy.example.com/testbucket/key.txt", nil, emptyPayloadHash, now, testCreds(), nil)
				q := r.URL.Query()
				q.Set("X-Amz-Signature", "deadbeef")
				r.URL.RawQuery = q.Encode()
				return r
			},
			wantCode: "InvalidArgument",
		},
		{
			name: "no authorization header",
			request: func(t *testing.T) *http.Request {
				return httptest.NewRequest(http.MethodGet, "http://proxy.example.com/testbucket/key.txt", nil)
			},
			wantCode: "AccessDenied",
		},
		{
			name: "malformed authorization header",
			request: func(t *testing.T) *http.Request {
				r := httptest.NewRequest(http.MethodGet, "http://proxy.example.com/testbucket/key.txt", nil)
				r.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
				return r
			},
			wantCode: "AuthorizationHeaderMalformed",
		},
		{
			name: "missing x-amz-content-sha256",
			request: func(t *testing.T) *http.Request {
				r := signedRequest(t, http.MethodGet, "http://proxy.example.com/testbucket/key.txt", nil, emptyPayloadHash, now, testCreds(), nil)
				r.Header.Del("X-Amz-Content-Sha256")
				return r
			},
			wantCode: "InvalidRequest",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			app := newTestApp(t)
			if !tc.now.IsZero() {
				app.SetNow(func() time.Time { return tc.now })
			} else {
				app.SetNow(func() time.Time { return now })
			}
			vr, s3err := app.VerifyRequest(tc.request(t))
			if tc.wantCode == "" {
				if s3err != nil {
					t.Fatalf("expect success, got %v", s3err)
				}
				if vr.AccessKeyID != testAccessKeyID {
					t.Errorf("expect access key %s, got %s", testAccessKeyID, vr.AccessKeyID)
				}
			} else {
				if s3err == nil {
					t.Fatal("expect error, got success")
				}
				if s3err.Code != tc.wantCode {
					t.Errorf("expect code %s, got %s (%s)", tc.wantCode, s3err.Code, s3err.Message)
				}
			}
		})
	}
}
