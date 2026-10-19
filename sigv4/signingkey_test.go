package sigv4_test

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/fujiwara/s3rp/sigv4"
)

// derivingLookup returns a lookup that hands out a SigningKeyFunc for secret
// under version instead of the secret itself, counting derivations.
func derivingLookup(akid, secret, version string, calls *atomic.Int32) sigv4.SecretLookup {
	return func(_ context.Context, accessKeyID, _ string) (sigv4.Credential, error) {
		if accessKeyID != akid {
			return sigv4.Credential{}, sigv4.ErrUnknownKey
		}
		return sigv4.Credential{
			SigningKey: func(_ context.Context, date, region string) ([]byte, error) {
				calls.Add(1)
				return sigv4.DeriveSigningKey([]byte(secret), date, region), nil
			},
			SecretVersion: version,
		}, nil
	}
}

func TestDeriveSigningKey(t *testing.T) {
	want := testSigningKey(testSecret, "20260801", "us-east-1")
	if got := sigv4.DeriveSigningKey([]byte(testSecret), "20260801", "us-east-1"); !bytes.Equal(got, want) {
		t.Errorf("DeriveSigningKey = %x, want %x", got, want)
	}
	mac := hmac.New(sha256.New, []byte("AWS4"+testSecret))
	mac.Write([]byte("20260801"))
	if got := sigv4.SigningKeyFromDateKey(mac.Sum(nil), "us-east-1"); !bytes.Equal(got, want) {
		t.Errorf("SigningKeyFromDateKey = %x, want %x", got, want)
	}
}

// Every mechanism verifies with a derived signing key exactly as with the
// secret, and carries the key — never the secret — on Verified.
func TestVerifyWithDerivedSigningKey(t *testing.T) {
	var calls atomic.Int32
	lk := derivingLookup(testAccessKeyID, testSecret, "v1", &calls)
	want := testSigningKey(testSecret, "20260801", "us-east-1")

	got, s3e := newVerifier().Verify(signedRequest(t, testSecret), lk)
	if s3e != nil {
		t.Fatalf("header auth: %v", s3e)
	}
	if !bytes.Equal(got.SigningKey, want) {
		t.Error("header auth: unexpected signing key on Verified")
	}

	got, s3e = newVerifier().Verify(presignedRequest(t, "http://s3.example.com/bucket/key.txt?X-Amz-Expires=300"), lk)
	if s3e != nil {
		t.Fatalf("presigned: %v", s3e)
	}
	if !bytes.Equal(got.SigningKey, want) {
		t.Error("presigned: unexpected signing key on Verified")
	}

	r := httptest.NewRequest("POST", "http://s3.example.com/sigv4examplebucket", nil)
	got, _, s3e = postVerifier(awsExampleTime).VerifyPost(r, awsExampleFields(),
		derivingLookup(awsExampleAccessKeyID, awsExampleSecret, "v1", &calls))
	if s3e != nil {
		t.Fatalf("POST policy: %v", s3e)
	}
	if !bytes.Equal(got.SigningKey, testSigningKey(awsExampleSecret, "20151229", "us-east-1")) {
		t.Error("POST policy: unexpected signing key on Verified")
	}
}

// The derivation runs once per access key and scope while cached, and again
// when the secret version changes; a rotated secret stops verifying at once.
func TestDerivedSigningKeyCache(t *testing.T) {
	v := newVerifier()
	var calls atomic.Int32
	lk := derivingLookup(testAccessKeyID, testSecret, "v1", &calls)
	for range 3 {
		if _, s3e := v.Verify(signedRequest(t, testSecret), lk); s3e != nil {
			t.Fatal(s3e)
		}
	}
	if n := calls.Load(); n != 1 {
		t.Errorf("derivations = %d, want 1", n)
	}

	rotated := derivingLookup(testAccessKeyID, "rotated-secret", "v2", &calls)
	_, s3e := v.Verify(signedRequest(t, testSecret), rotated)
	if s3e == nil || s3e.Code != "SignatureDoesNotMatch" {
		t.Errorf("old secret after rotation: got %v, want SignatureDoesNotMatch", s3e)
	}
	if _, s3e := v.Verify(signedRequest(t, "rotated-secret"), rotated); s3e != nil {
		t.Errorf("new secret after rotation: %v", s3e)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("derivations = %d, want 2", n)
	}
}

func TestDerivedSigningKeyFailures(t *testing.T) {
	derive := func(key []byte, err error) sigv4.SigningKeyFunc {
		return func(context.Context, string, string) ([]byte, error) { return key, err }
	}
	good := derive(testSigningKey(testSecret, "20260801", "us-east-1"), nil)
	for _, tc := range []struct {
		name string
		cred sigv4.Credential
		code string
	}{
		{"neither secret nor signing key", sigv4.Credential{}, "InternalError"},
		{"both secret and signing key", sigv4.Credential{SecretAccessKey: testSecret, SigningKey: good, SecretVersion: "v1"}, "InternalError"},
		{"signing key without version", sigv4.Credential{SigningKey: good}, "InternalError"},
		{"derivation fails", sigv4.Credential{SigningKey: derive(nil, errors.New("kms down")), SecretVersion: "v1"}, "InternalError"},
		{"key gone", sigv4.Credential{SigningKey: derive(nil, sigv4.ErrUnknownKey), SecretVersion: "v1"}, "InvalidAccessKeyId"},
		{"short key", sigv4.Credential{SigningKey: derive(make([]byte, 16), nil), SecretVersion: "v1"}, "InternalError"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			lk := func(context.Context, string, string) (sigv4.Credential, error) { return tc.cred, nil }
			_, s3e := newVerifier().Verify(signedRequest(t, testSecret), lk)
			if s3e == nil || s3e.Code != tc.code {
				t.Errorf("got %v, want %s", s3e, tc.code)
			}
		})
	}
}

// A failed derivation is not cached: the next request derives again.
func TestDerivedSigningKeyFailureNotCached(t *testing.T) {
	v := newVerifier()
	var calls atomic.Int32
	failing := true
	lk := func(context.Context, string, string) (sigv4.Credential, error) {
		return sigv4.Credential{
			SigningKey: func(_ context.Context, date, region string) ([]byte, error) {
				calls.Add(1)
				if failing {
					return nil, errors.New("kms down")
				}
				return sigv4.DeriveSigningKey([]byte(testSecret), date, region), nil
			},
			SecretVersion: "v1",
		}, nil
	}
	if _, s3e := v.Verify(signedRequest(t, testSecret), lk); s3e == nil {
		t.Fatal("expect the failed derivation to refuse the request")
	}
	failing = false
	if _, s3e := v.Verify(signedRequest(t, testSecret), lk); s3e != nil {
		t.Fatalf("expect the next request to derive again: %v", s3e)
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("derivations = %d, want 2", n)
	}
}
