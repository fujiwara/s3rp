package harness

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"

	"github.com/fujiwara/s3rp/store"
)

// backendConfig identifies the single S3 backend every bucket the harness
// creates lives on.
type backendConfig struct {
	endpoint        string
	accessKeyID     string
	secretAccessKey string
}

// bucketRec is what the harness remembers about a bucket it created.
type bucketRec struct {
	tenant    string
	createdAt time.Time
}

// memStore is a mutable in-memory store.Store. The gateway reads it on
// every request; the CreateBucket/DeleteBucket interceptor mutates it.
// Keys are fixed at construction; buckets change under the mutex.
type memStore struct {
	keys    map[string]*store.Key
	backend backendConfig
	now     func() time.Time

	mu      sync.RWMutex
	buckets map[string]*bucketRec
}

func newMemStore(be backendConfig, keys []*store.Key) *memStore {
	m := &memStore{
		keys:    make(map[string]*store.Key, len(keys)),
		backend: be,
		now:     time.Now,
		buckets: map[string]*bucketRec{},
	}
	for _, k := range keys {
		m.keys[k.AccessKeyID] = k
	}
	return m
}

func (m *memStore) GetKey(ctx context.Context, accessKeyID, sessionToken string) (*store.Key, error) {
	k, ok := m.keys[accessKeyID]
	if !ok {
		return nil, fmt.Errorf("access key %q: %w", accessKeyID, store.ErrNotFound)
	}
	// All harness keys are long-lived; a presented session token is left to
	// the gateway's exact-match refusal.
	return k, nil
}

func (m *memStore) GetBucket(ctx context.Context, tenant, bucket string) (*store.Bucket, error) {
	m.mu.RLock()
	rec, ok := m.buckets[bucket]
	m.mu.RUnlock()
	if !ok || rec.tenant != tenant {
		return nil, fmt.Errorf("bucket %q of tenant %q: %w", bucket, tenant, store.ErrNotFound)
	}
	return m.bucketFor(bucket, rec), nil
}

func (m *memStore) GetBucketByName(ctx context.Context, bucket string) (*store.Bucket, error) {
	m.mu.RLock()
	rec, ok := m.buckets[bucket]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("bucket %q: %w", bucket, store.ErrNotFound)
	}
	return m.bucketFor(bucket, rec), nil
}

func (m *memStore) ListBuckets(ctx context.Context, tenant string) ([]store.BucketEntry, error) {
	m.mu.RLock()
	// never nil, never ErrNotFound: an unknown tenant simply has no buckets
	entries := []store.BucketEntry{}
	for name, rec := range m.buckets {
		if rec.tenant == tenant {
			entries = append(entries, store.BucketEntry{Name: name, CreatedAt: rec.createdAt})
		}
	}
	m.mu.RUnlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// bucketFor builds a fresh store.Bucket so callers never share mutable
// state. The backend bucket name equals the front name (front names are
// globally unique). SetDefaults must be applied, as for any Store.
func (m *memStore) bucketFor(name string, rec *bucketRec) *store.Bucket {
	be := &store.Backend{
		Endpoint:        m.backend.endpoint,
		AccessKeyID:     m.backend.accessKeyID,
		SecretAccessKey: store.Password(m.backend.secretAccessKey),
	}
	be.SetDefaults(name)
	return &store.Bucket{
		Tenant:    rec.tenant,
		Name:      name,
		Backend:   be,
		CreatedAt: rec.createdAt,
	}
}

// claim registers the bucket for the tenant if the name is free. When the
// name is taken it reports the current owner instead. Atomic, so a claim
// either wins or learns who did.
func (m *memStore) claim(name, tenant string) (owner string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rec, exists := m.buckets[name]; exists {
		return rec.tenant, false
	}
	m.buckets[name] = &bucketRec{tenant: tenant, createdAt: m.now()}
	return tenant, true
}

// owner reports which tenant owns the bucket, if it exists.
func (m *memStore) owner(name string) (tenant string, ok bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rec, exists := m.buckets[name]
	if !exists {
		return "", false
	}
	return rec.tenant, true
}

// remove deletes the bucket registration (rollback of a failed claim, or
// after a successful backend delete).
func (m *memStore) remove(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.buckets, name)
}
