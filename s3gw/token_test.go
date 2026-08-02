package s3gw_test

import (
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
