package sigv4_test

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/fujiwara/s3rp/sigv4"
)

// Temporary credentials: the SDK signs the session token into the request
// (header, presigned query, or POST form field), and the verifier must
// require exactly the stored token — and refuse a token on a long-lived key.

const testToken = "FwoGZXIvYXdzEXAMPLETOKEN=="

func tokenLookup(_ context.Context, akid string) (sigv4.Credential, error) {
	if akid != testAccessKeyID {
		return sigv4.Credential{}, sigv4.ErrUnknownKey
	}
	return sigv4.Credential{SecretAccessKey: testSecret, SessionToken: testToken}, nil
}

// signedRequestCreds signs a header-auth GET with arbitrary credentials; the
// SDK signer adds and signs X-Amz-Security-Token itself when the credentials
// carry a session token, exactly as a real client does.
func signedRequestCreds(t *testing.T, creds aws.Credentials) *http.Request {
	t.Helper()
	const url = "http://s3.example.com/bucket/key.txt"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Amz-Content-Sha256", emptyPayload)
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if err := signer.SignHTTP(context.Background(), creds, req, emptyPayload, "s3", "us-east-1", testTime); err != nil {
		t.Fatal(err)
	}
	sr := httptest.NewRequest("GET", url, nil)
	sr.Header = req.Header
	sr.Host = req.Host
	return sr
}

// presignedRequestCreds builds a presigned GET with arbitrary credentials;
// the presigner puts X-Amz-Security-Token into the query itself.
func presignedRequestCreds(t *testing.T, creds aws.Credentials) *http.Request {
	t.Helper()
	const url = "http://s3.example.com/bucket/key.txt?X-Amz-Expires=300"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	signedURI, _, err := signer.PresignHTTP(context.Background(), creds, req, "UNSIGNED-PAYLOAD", "s3", "us-east-1", testTime)
	if err != nil {
		t.Fatal(err)
	}
	sr := httptest.NewRequest("GET", signedURI, nil)
	sr.Host = req.Host
	return sr
}

func TestVerifySessionToken(t *testing.T) {
	tempCreds := aws.Credentials{
		AccessKeyID: testAccessKeyID, SecretAccessKey: testSecret, SessionToken: testToken,
	}
	permCreds := aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecret}

	t.Run("header auth", func(t *testing.T) {
		got, s3e := newVerifier().Verify(signedRequestCreds(t, tempCreds), tokenLookup)
		if s3e != nil {
			t.Fatalf("expect a temporary credential to verify, got %v", s3e)
		}
		if got.AccessKeyID != testAccessKeyID {
			t.Errorf("unexpected access key %q", got.AccessKeyID)
		}
	})
	t.Run("presigned", func(t *testing.T) {
		if _, s3e := newVerifier().Verify(presignedRequestCreds(t, tempCreds), tokenLookup); s3e != nil {
			t.Fatalf("expect a temporary presigned URL to verify, got %v", s3e)
		}
	})
	t.Run("wrong token", func(t *testing.T) {
		bad := tempCreds
		bad.SessionToken = "FORGEDTOKEN"
		_, s3e := newVerifier().Verify(signedRequestCreds(t, bad), tokenLookup)
		if s3e == nil || s3e.Code != "InvalidToken" {
			t.Errorf("expect InvalidToken, got %v", s3e)
		}
	})
	t.Run("temporary key without a token", func(t *testing.T) {
		_, s3e := newVerifier().Verify(signedRequestCreds(t, permCreds), tokenLookup)
		if s3e == nil || s3e.Code != "InvalidToken" {
			t.Errorf("expect InvalidToken, got %v", s3e)
		}
	})
	t.Run("long-lived key presenting a token", func(t *testing.T) {
		// lookup (not tokenLookup) knows this key as long-lived
		_, s3e := newVerifier().Verify(signedRequestCreds(t, tempCreds), lookup)
		if s3e == nil || s3e.Code != "InvalidToken" {
			t.Errorf("expect InvalidToken, got %v", s3e)
		}
	})
	t.Run("presigned without its token", func(t *testing.T) {
		_, s3e := newVerifier().Verify(presignedRequestCreds(t, permCreds), tokenLookup)
		if s3e == nil || s3e.Code != "InvalidToken" {
			t.Errorf("expect InvalidToken, got %v", s3e)
		}
	})
}

func TestVerifyPostSessionToken(t *testing.T) {
	policy := `{"expiration":"2026-08-02T00:00:00Z","conditions":[` +
		`{"bucket":"testbucket"},{"key":"a.txt"},` +
		`{"x-amz-credential":"` + testAccessKeyID + `/20260801/us-east-1/s3/aws4_request"},` +
		`{"x-amz-algorithm":"AWS4-HMAC-SHA256"},{"x-amz-date":"20260801T120000Z"},` +
		`{"x-amz-security-token":"` + testToken + `"}]}`
	b64 := base64.StdEncoding.EncodeToString([]byte(policy))
	fields := func(token string) map[string]string {
		return map[string]string{
			"bucket":               "testbucket",
			"key":                  "a.txt",
			"x-amz-credential":     testAccessKeyID + "/20260801/us-east-1/s3/aws4_request",
			"x-amz-algorithm":      "AWS4-HMAC-SHA256",
			"x-amz-date":           "20260801T120000Z",
			"x-amz-security-token": token,
			"policy":               b64,
			"x-amz-signature":      signPostPolicy(testSecret, "20260801", "us-east-1", b64),
		}
	}
	r := httptest.NewRequest("POST", "http://s3.example.com/testbucket", nil)

	if _, _, s3e := postVerifier(testTime).VerifyPost(r, fields(testToken), tokenLookup); s3e != nil {
		t.Fatalf("expect a temporary POST upload to verify, got %v", s3e)
	}
	if _, _, s3e := postVerifier(testTime).VerifyPost(r, fields("FORGEDTOKEN"), tokenLookup); s3e == nil || s3e.Code != "InvalidToken" {
		t.Errorf("expect InvalidToken for a wrong token, got %v", s3e)
	}
}
