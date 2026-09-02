package s3gw_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

// quota stands in for the kind of value a service loads together with its
// definitions and needs back in its hooks without a second lookup.
type quota struct{ limit int64 }

type recordingAuthorizer struct{ ops []*s3gw.Op }

func (a *recordingAuthorizer) Authorize(_ context.Context, op *s3gw.Op) error {
	a.ops = append(a.ops, op)
	return nil
}

// TestOpCarriesStoreMetadata checks that what a Store attaches to its
// definitions reaches the hooks on Op, untouched: the same values, not
// copies or re-fetches.
func TestOpCarriesStoreMetadata(t *testing.T) {
	bucketMeta := &quota{limit: 42}
	keyMeta := &struct{ suspended bool }{suspended: false}
	pathStyle := true
	gw := s3gw.New(memStore{
		keys: map[string]*store.Key{
			testAccessKeyID: {
				AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey,
				Tenant: "testtenant", User: "testuser",
				Metadata: keyMeta,
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
				Metadata: bucketMeta,
			},
		},
	})

	auth := &recordingAuthorizer{}
	gw.SetAuthorizer(auth)
	var intercepted []*s3gw.Op
	gw.Use(func(ctx context.Context, op *s3gw.Op, next func() error) error {
		intercepted = append(intercepted, op)
		return next()
	})

	res := serve(t, gw, stubGet{body: "hello"})
	if res.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", res.Code, res.Body.String())
	}

	if len(auth.ops) != 1 || len(intercepted) != 1 {
		t.Fatalf("expect one op in each hook, got %d and %d", len(auth.ops), len(intercepted))
	}
	for _, op := range []*s3gw.Op{auth.ops[0], intercepted[0]} {
		if op.BucketMetadata != any(bucketMeta) {
			t.Errorf("expect the bucket metadata the store attached, got %#v", op.BucketMetadata)
		}
		if op.KeyMetadata != any(keyMeta) {
			t.Errorf("expect the key metadata the store attached, got %#v", op.KeyMetadata)
		}
	}
}

// TestOpMetadataStaysOutOfJSON pins the metadata's exclusion from the record
// an observer emits as it stands: whether this data belongs in a log is the
// service's decision, not the gateway's.
func TestOpMetadataStaysOutOfJSON(t *testing.T) {
	op := &s3gw.Op{
		Method: "GET", Actions: []string{"s3:GetObject"},
		Tenant: "testtenant", User: "testuser",
		Bucket: "testbucket", Key: "a.txt",
		BucketMetadata: &quota{limit: 42},
		KeyMetadata:    "billing-tier-secret",
		// the object's own metadata is client-supplied and excluded for the
		// same reason
		Request: &s3gw.OpRequest{
			StorageClass: "PLAN_COLD",
			Metadata:     map[string]string{"plan": "requested-tier-secret"},
		},
		Response: &s3gw.OpResponse{
			StorageClass: "PLAN_COLD",
			Metadata:     map[string]string{"plan": "stored-tier-secret"},
		},
	}
	b, err := json.Marshal(op)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"BucketMetadata", "KeyMetadata", "bucket_metadata", "key_metadata"} {
		if _, ok := got[key]; ok {
			t.Errorf("expect %s to be excluded from JSON: %s", key, b)
		}
	}
	if s := string(b); strings.Contains(s, "tier-secret") || strings.Contains(s, "42") {
		t.Errorf("metadata leaked into JSON: %s", s)
	}
	// the rest of Request/Response is the gateway's own view of the
	// operation and is emitted
	for _, want := range []string{`"request":{"storage_class":"PLAN_COLD"}`, `"response":{"storage_class":"PLAN_COLD"}`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("expect %s in the JSON: %s", want, b)
		}
	}
}
