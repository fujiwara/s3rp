package localbackend_test

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/s3gw/localbackend"
	"github.com/fujiwara/s3rp/store"
)

const (
	testTenant          = "acme"
	testAccessKeyID     = "LOCALBACKENDTESTKEY1"
	testSecretAccessKey = "localbackendtestsecret"
)

// testStore is the definition store of a service in miniature: one tenant,
// one key, and the buckets the test's stand-in control plane registered.
type testStore struct {
	mu      sync.RWMutex
	buckets map[string]bool
}

func (s *testStore) GetKey(_ context.Context, accessKeyID, _ string) (*store.Key, error) {
	if accessKeyID != testAccessKeyID {
		return nil, store.ErrNotFound
	}
	return &store.Key{
		AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey,
		Tenant: testTenant, User: "tester",
	}, nil
}

func (s *testStore) GetBucket(ctx context.Context, tenant, bucket string) (*store.Bucket, error) {
	if tenant != testTenant {
		return nil, store.ErrNotFound
	}
	return s.GetBucketByName(ctx, bucket)
}

func (s *testStore) GetBucketByName(_ context.Context, bucket string) (*store.Bucket, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.buckets[bucket] {
		return nil, store.ErrNotFound
	}
	// the endpoint is never dialed; the backend bucket carries a prefix so
	// that a front name reaching the backend would not resolve
	be := &store.Backend{Endpoint: "http://localbackend.invalid", Bucket: backendName(bucket)}
	be.SetDefaults(bucket)
	return &store.Bucket{Tenant: testTenant, Name: bucket, Backend: be}, nil
}

func (s *testStore) ListBuckets(context.Context, string) ([]store.BucketEntry, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entries := []store.BucketEntry{}
	for name := range s.buckets {
		entries = append(entries, store.BucketEntry{Name: name})
	}
	return entries, nil
}

func backendName(front string) string { return "be-" + front }

// fixture is a gateway served over TLS — so the SDK client behaves as it
// does toward a real service, trailing checksums included — with every
// bucket on one in-process backend.
type fixture struct {
	t       *testing.T
	client  *s3.Client
	backend *localbackend.Backend
	store   *testStore
}

// eachBackend runs a test against a Backend held in memory and one kept in
// a directory: they are the same backend and must behave the same.
func eachBackend(t *testing.T, test func(*testing.T, *localbackend.Backend)) {
	t.Run("memory", func(t *testing.T) { test(t, localbackend.New()) })
	t.Run("directory", func(t *testing.T) {
		dir := t.TempDir()
		be, err := localbackend.Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := be.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
			// whatever the test left must load again
			again, err := localbackend.Open(dir)
			if err != nil {
				t.Errorf("reopening what the test left: %v", err)
				return
			}
			again.Close()
		})
		test(t, be)
	})
}

func newFixture(t *testing.T, be *localbackend.Backend) *fixture {
	t.Helper()
	st := &testStore{buckets: map[string]bool{}}
	gw := s3gw.New(st)
	gw.SetBackendFactory(func(context.Context, *store.Backend) (s3gw.BackendClient, error) {
		return be, nil
	})
	ts := httptest.NewTLSServer(gw.Handler())
	t.Cleanup(ts.Close)
	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, "")),
		awsconfig.WithHTTPClient(ts.Client()),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts.URL)
		o.UsePathStyle = true
		// a read of an object stored without a checksum is not news here
		o.DisableLogOutputChecksumValidationSkipped = true
	})
	return &fixture{t: t, client: client, backend: be, store: st}
}

// createBucket does what a control plane does: creates the bucket on the
// backend and registers its definition.
func (f *fixture) createBucket(name string, objectLock bool) {
	f.t.Helper()
	if _, err := f.backend.CreateBucket(f.t.Context(), &s3.CreateBucketInput{
		Bucket:                     aws.String(backendName(name)),
		ObjectLockEnabledForBucket: aws.Bool(objectLock),
	}); err != nil {
		f.t.Fatal(err)
	}
	f.store.mu.Lock()
	f.store.buckets[name] = true
	f.store.mu.Unlock()
}

// wantError checks an error a client got from the gateway for its S3 code
// and HTTP status.
func wantError(t *testing.T, err error, status int, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expect %d %s, got success", status, code)
	}
	var gotCode string
	if apiErr, ok := errors.AsType[smithy.APIError](err); ok {
		gotCode = apiErr.ErrorCode()
	}
	gotStatus := 0
	if respErr, ok := errors.AsType[*awshttp.ResponseError](err); ok {
		gotStatus = respErr.HTTPStatusCode()
	}
	if gotStatus != status || gotCode != code {
		t.Fatalf("expect %d %s, got %d %s (%v)", status, code, gotStatus, gotCode, err)
	}
}
