package s3gw_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

// TestGatewaySessionToken checks the wiring: a store.Key carrying a session
// token verifies only when the request presents it. The rules themselves are
// tested in sigv4.
func TestGatewaySessionToken(t *testing.T) {
	const token = "FwoGZXIvYXdzEXAMPLETOKEN=="
	pathStyle := true
	gw := s3gw.New(memStore{
		keys: map[string]*store.Key{
			testAccessKeyID: {
				AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey,
				Tenant: "testtenant", User: "testuser",
				SessionToken: token,
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
	if err := gw.SetBackend("testbucket", stubGet{body: "x"}); err != nil {
		t.Fatal(err)
	}

	do := func(creds aws.Credentials) *httptest.ResponseRecorder {
		t.Helper()
		req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
			nil, emptyPayloadHash, time.Now(), creds, nil)
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, req)
		return w
	}

	with := do(aws.Credentials{
		AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey, SessionToken: token,
	})
	if with.Code != http.StatusOK {
		t.Errorf("expect the temporary credential to work, got %d: %s", with.Code, with.Body.String())
	}
	without := do(testCreds())
	if without.Code != http.StatusBadRequest || !strings.Contains(without.Body.String(), "InvalidToken") {
		t.Errorf("expect InvalidToken without the token, got %d: %s", without.Code, without.Body.String())
	}
}

// statelessStore stands in for a store that issues self-contained tokens
// and persists no per-credential state: GetKey authenticates the presented
// token itself (a fixed prefix stands in for verifying a MAC), derives the
// key from it, and echoes the token back as Key.SessionToken so the
// gateway's exact-match passes. A token failing the check is refused with
// store.ErrInvalidToken.
type statelessStore struct{ memStore }

const validTokenPrefix = "valid:"

func (s statelessStore) GetKey(_ context.Context, id, token string) (*store.Key, error) {
	if !strings.HasPrefix(token, validTokenPrefix) {
		return nil, fmt.Errorf("%w: mac mismatch", store.ErrInvalidToken)
	}
	return &store.Key{
		AccessKeyID: id, SecretAccessKey: testSecretAccessKey,
		Tenant: "testtenant", User: "testuser",
		SessionToken: token,
	}, nil
}

// TestGatewayStatelessToken checks that the presented token reaches the
// store, so a store may validate tokens itself instead of persisting
// temporary keys — revocation then being its own problem.
func TestGatewayStatelessToken(t *testing.T) {
	pathStyle := true
	gw := s3gw.New(statelessStore{memStore{
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
	}})
	if err := gw.SetBackend("testbucket", stubGet{body: "x"}); err != nil {
		t.Fatal(err)
	}
	var observed *s3gw.RequestInfo
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { observed = info })

	do := func(token string) *httptest.ResponseRecorder {
		t.Helper()
		req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
			nil, emptyPayloadHash, time.Now(), aws.Credentials{
				AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey, SessionToken: token,
			}, nil)
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, req)
		return w
	}

	// any token the store's check accepts works, with no key persisted
	ok := do(validTokenPrefix + "issued-out-of-band")
	if ok.Code != http.StatusOK {
		t.Errorf("expect a store-validated token to work, got %d: %s", ok.Code, ok.Body.String())
	}
	forged := do("forged")
	if forged.Code != http.StatusBadRequest || !strings.Contains(forged.Body.String(), "InvalidToken") {
		t.Errorf("expect InvalidToken for a token the store refuses, got %d: %s", forged.Code, forged.Body.String())
	}
	// the store's reason survives as the observed cause (and never reaches
	// the client: the XML above carries only the InvalidToken message)
	if observed == nil || observed.Err == nil || !strings.Contains(observed.Err.Error(), "mac mismatch") {
		t.Errorf("expect the store's error to reach the observer, got %v", observed)
	}
	if strings.Contains(forged.Body.String(), "mac mismatch") {
		t.Errorf("the cause must not reach the client: %s", forged.Body.String())
	}
}
