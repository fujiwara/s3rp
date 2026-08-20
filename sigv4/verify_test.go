package sigv4_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

func lookup(_ context.Context, accessKeyID, _ string) (sigv4.Credential, error) {
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

// presignedRequest builds a server-side presigned GET request signed like a
// real S3 client. target must carry X-Amz-Expires; the signer does not add it.
func presignedRequest(t *testing.T, target string) *http.Request {
	t.Helper()
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecret}
	signedURI, _, err := signer.PresignHTTP(context.Background(), creds, req, "UNSIGNED-PAYLOAD", "s3", "us-east-1", testTime)
	if err != nil {
		t.Fatal(err)
	}
	sr := httptest.NewRequest("GET", signedURI, nil)
	sr.Host = req.Host
	return sr
}

// An x-amz-* header outside SignedHeaders is attacker-controllable: the
// signature does not commit to it, so honoring it would let a presigned-URL
// holder attach semantics (storage class, object-lock retention, an SSE mode,
// a copy source, metadata) the grantor never authorized — the Ceph RGW
// CVE-2026-54330 class. The verifier must refuse it with AccessDenied on both
// the header and the presigned auth paths.
func TestVerifyRejectsUnsignedAmzHeader(t *testing.T) {
	t.Run("header auth", func(t *testing.T) {
		r := signedRequest(t, testSecret)
		r.Header.Set("x-amz-storage-class", "GLACIER") // not in SignedHeaders
		_, err := newVerifier().Verify(r, lookup)
		if err == nil {
			t.Fatal("expect the unsigned x-amz header to be refused")
		}
		if err.Code != "AccessDenied" {
			t.Errorf("expect AccessDenied, got %s (%s)", err.Code, err.Message)
		}
	})
	t.Run("presigned auth", func(t *testing.T) {
		r := presignedRequest(t, "http://s3.example.com/bucket/key.txt?X-Amz-Expires=300")
		r.Header.Set("x-amz-acl", "public-read") // not signed; presign hoists to query
		_, err := newVerifier().Verify(r, lookup)
		if err == nil {
			t.Fatal("expect the unsigned x-amz header to be refused")
		}
		if err.Code != "AccessDenied" {
			t.Errorf("expect AccessDenied, got %s (%s)", err.Code, err.Message)
		}
	})
	t.Run("message lists headers sorted", func(t *testing.T) {
		r := signedRequest(t, testSecret)
		r.Header.Set("x-amz-tagging", "k=v")
		r.Header.Set("x-amz-meta-foo", "bar")
		_, err := newVerifier().Verify(r, lookup)
		if err == nil {
			t.Fatal("expect refusal")
		}
		if want := "x-amz-meta-foo, x-amz-tagging"; !strings.Contains(err.Message, want) {
			t.Errorf("expect message to list %q, got %q", want, err.Message)
		}
	})
}

// A standard (non-x-amz) header left unsigned is legitimate: S3 requires only
// x-amz-* headers to be signed, and presigners deliberately leave headers such
// as Content-Type off a presigned PUT's signature. The gate must not touch it.
func TestVerifyAllowsUnsignedStandardHeader(t *testing.T) {
	r := signedRequest(t, testSecret)
	r.Header.Set("Content-Type", "text/plain") // not signed, but not x-amz-*
	if _, err := newVerifier().Verify(r, lookup); err != nil {
		t.Fatalf("an unsigned standard header must be allowed, got %v", err)
	}
}

// Verified.SignedHeaders is what the caller's header accessor decides
// signature coverage by, so it must name exactly what the signature commits
// to: the SignedHeaders list, plus — presigned — the promoted x-amz-* query
// parameters, whose values arrived in the signed query string.
func TestVerifiedSignedHeaders(t *testing.T) {
	t.Run("header auth", func(t *testing.T) {
		got, err := newVerifier().Verify(signedRequest(t, testSecret), lookup)
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range []string{"host", "x-amz-content-sha256", "x-amz-date"} {
			if !got.SignedHeaders[h] {
				t.Errorf("SignedHeaders must contain %s, got %v", h, got.SignedHeaders)
			}
		}
		if got.SignedHeaders["content-type"] {
			t.Error("SignedHeaders must not contain a header the client did not sign")
		}
	})
	t.Run("presigned with hoisted params", func(t *testing.T) {
		r := presignedRequest(t, "http://s3.example.com/bucket/key.txt?X-Amz-Expires=300&x-amz-meta-foo=bar&x-amz-storage-class=GLACIER")
		got, err := newVerifier().Verify(r, lookup)
		if err != nil {
			t.Fatal(err)
		}
		if !got.SignedHeaders["host"] {
			t.Errorf("SignedHeaders must contain the listed signed headers, got %v", got.SignedHeaders)
		}
		for _, h := range []string{"x-amz-meta-foo", "x-amz-storage-class"} {
			if !got.SignedHeaders[h] {
				t.Errorf("SignedHeaders must cover the promoted query param %s, got %v", h, got.SignedHeaders)
			}
			if r.Header.Get(h) == "" {
				t.Errorf("%s must have been promoted into the headers", h)
			}
		}
		if got.SignedHeaders["x-amz-signature"] || got.SignedHeaders["x-amz-expires"] {
			t.Error("auth params must not be reported as signed headers")
		}
	})
}

// a lookup failure that is not ErrUnknownKey must not be reported as a bad
// key, since that would tell a client its credentials are wrong when the
// service is merely broken
func TestVerifyLookupFailure(t *testing.T) {
	failing := func(context.Context, string, string) (sigv4.Credential, error) {
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

// A key may begin with a slash, so a presigned URL's path may begin with
// "//". The verifier used to re-parse the re-signed URI — scheme-less, so
// url.Parse read "//key/..." as an authority — and answered InternalError.
// Found by the botocore cross-check.
func TestVerifyPresignedLeadingSlashKey(t *testing.T) {
	const target = "http://s3.example.com//spaced%20key/a?X-Amz-Expires=300"
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		t.Fatal(err)
	}
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecret}
	signedURI, _, err := signer.PresignHTTP(context.Background(), creds, req, "UNSIGNED-PAYLOAD", "s3", "us-east-1", testTime)
	if err != nil {
		t.Fatal(err)
	}
	sr := httptest.NewRequest("GET", signedURI, nil)
	sr.Host = req.Host
	got, s3e := newVerifier().Verify(sr, lookup)
	if s3e != nil {
		t.Fatalf("presigned URL with a leading-slash key refused: %v", s3e)
	}
	if got.AccessKeyID != testAccessKeyID {
		t.Errorf("unexpected access key %q", got.AccessKeyID)
	}
}
