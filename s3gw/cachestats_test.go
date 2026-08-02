package s3gw_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
	"github.com/google/go-cmp/cmp"
)

func TestClientCacheStats(t *testing.T) {
	gw := s3gw.New(memStore{})
	gw.SetNewClient(func(_ context.Context, _ *store.Backend) (s3gw.BackendClient, error) {
		return &fakeClient{}, nil
	})
	gw.SetClientCacheSize(2)

	get := func(i int) {
		t.Helper()
		if _, err := gw.BackendClientFor(t.Context(), lruTestBackend(i)); err != nil {
			t.Fatal(err)
		}
	}
	get(0) // miss
	get(0) // hit
	get(1) // miss
	get(2) // miss, evicts backend 0

	want := s3gw.CacheStats{Hits: 1, Misses: 3, Evictions: 1, Len: 2, Capacity: 2}
	if diff := cmp.Diff(want, gw.ClientCacheStats()); diff != "" {
		t.Errorf("unexpected stats (-want +got):\n%s", diff)
	}

	// shrinking evicts the overflow, and the new bound is reported
	gw.SetClientCacheSize(1)
	got := gw.ClientCacheStats()
	if got.Evictions != 2 || got.Len != 1 || got.Capacity != 1 {
		t.Errorf("expect the resize eviction in the stats, got %+v", got)
	}
}

func TestSignerCacheStats(t *testing.T) {
	const otherKeyID, otherSecret = "S3RPTESTKEY002", "othersecret002"
	pathStyle := true
	gw := s3gw.New(memStore{
		keys: map[string]*store.Key{
			testAccessKeyID: {
				AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey,
				Tenant: "testtenant", User: "testuser",
			},
			otherKeyID: {
				AccessKeyID: otherKeyID, SecretAccessKey: otherSecret,
				Tenant: "testtenant", User: "otheruser",
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
	// a single slot forces a displacement as soon as a second key verifies
	gw.SetSignerCacheSize(1)
	if err := gw.SetBackend("testbucket", stubGet{body: "x"}); err != nil {
		t.Fatal(err)
	}

	do := func(akid, secret string) {
		t.Helper()
		req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
			nil, emptyPayloadHash, time.Now(), aws.Credentials{AccessKeyID: akid, SecretAccessKey: secret}, nil)
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
		}
	}

	do(testAccessKeyID, testSecretAccessKey) // miss: first sight of the key
	do(testAccessKeyID, testSecretAccessKey) // hit
	want := s3gw.CacheStats{Hits: 1, Misses: 1, Evictions: 0, Len: 1, Capacity: 1}
	if diff := cmp.Diff(want, gw.SignerCacheStats()); diff != "" {
		t.Errorf("unexpected stats (-want +got):\n%s", diff)
	}

	// a second key lands on the only slot and displaces the first
	do(otherKeyID, otherSecret)
	stats := gw.SignerCacheStats()
	if stats.Misses != 2 || stats.Evictions != 1 || stats.Len != 1 {
		t.Errorf("expect the displacement in the stats, got %+v", stats)
	}
}
