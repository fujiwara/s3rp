package s3gw_test

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
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

const (
	testAccessKeyID     = "S3RPTESTKEY001"
	testSecretAccessKey = "testsecret001"
	emptyPayloadHash    = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// memStore is the smallest Store the gateway can run on, so these tests do
// not depend on how a service happens to define its tenants.
type memStore struct {
	keys    map[string]*store.Key
	buckets map[string]*store.Bucket
}

func (m memStore) GetKey(_ context.Context, id string) (*store.Key, error) {
	if k, ok := m.keys[id]; ok {
		return k, nil
	}
	return nil, store.ErrNotFound
}

func (m memStore) GetBucket(_ context.Context, tenant, name string) (*store.Bucket, error) {
	if b, ok := m.buckets[name]; ok && b.Tenant == tenant {
		return b, nil
	}
	return nil, store.ErrNotFound
}

func (m memStore) GetBucketByName(_ context.Context, name string) (*store.Bucket, error) {
	if b, ok := m.buckets[name]; ok {
		return b, nil
	}
	return nil, store.ErrNotFound
}

func (m memStore) ListBucketNames(_ context.Context, tenant string) ([]string, error) {
	var names []string
	for n, b := range m.buckets {
		if b.Tenant == tenant {
			names = append(names, n)
		}
	}
	return names, nil
}

func newTestGateway(t *testing.T) *s3gw.Gateway {
	t.Helper()
	pathStyle := true
	return s3gw.New(memStore{
		keys: map[string]*store.Key{
			testAccessKeyID: {
				AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey,
				Tenant: "testtenant", User: "testuser",
			},
		},
		buckets: map[string]*store.Bucket{
			"testbucket": {
				Tenant: "testtenant", Name: "testbucket",
				Backend: &store.Backend{
					Endpoint: "http://backend.invalid", Region: "us-east-1",
					Bucket: "backend-testbucket", AccessKeyID: "bk", SecretAccessKey: "bs",
					UsePathStyle: &pathStyle,
				},
			},
		},
	})
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
			g := newTestGateway(t)
			if !tc.now.IsZero() {
				g.SetNow(func() time.Time { return tc.now })
			} else {
				g.SetNow(func() time.Time { return now })
			}
			vr, s3e := g.VerifyRequest(tc.request(t))
			if tc.wantCode == "" {
				if s3e != nil {
					t.Fatalf("expect success, got %v", s3e)
				}
				if vr.AccessKeyID != testAccessKeyID {
					t.Errorf("expect access key %s, got %s", testAccessKeyID, vr.AccessKeyID)
				}
			} else {
				if s3e == nil {
					t.Fatal("expect error, got success")
				}
				if s3e.Code != tc.wantCode {
					t.Errorf("expect code %s, got %s (%s)", tc.wantCode, s3e.Code, s3e.Message)
				}
			}
		})
	}
}

// TestGatewaySetRegion checks the wiring only; the behavior itself is the
// sigv4 package's and is tested there.
func TestGatewaySetRegion(t *testing.T) {
	g := newTestGateway(t)
	g.SetRegion("eu-west-1")
	// signedRequest signs for us-east-1
	req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
		nil, emptyPayloadHash, time.Now(), testCreds(), nil)
	_, s3e := g.VerifyRequest(req)
	if s3e == nil {
		t.Fatal("expect the pinned region to refuse a us-east-1 signature")
	}
	if s3e.Code != "AuthorizationHeaderMalformed" {
		t.Errorf("expect AuthorizationHeaderMalformed, got %s (%s)", s3e.Code, s3e.Message)
	}
}
