package s3rp

import (
	"context"
	"fmt"

	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/store"
)

// configStore is a store.Store backed by the static YAML config.
type configStore struct {
	keys    map[string]*store.Key
	tenants map[string]map[string]*store.Bucket // tenant name -> bucket name -> bucket
	byName  map[string]*store.Bucket            // bucket name -> bucket (globally unique)
}

// NewConfigStore builds a store.Store from a validated config.
func NewConfigStore(cfg *Config) store.Store {
	s := &configStore{
		keys:    make(map[string]*store.Key),
		tenants: make(map[string]map[string]*store.Bucket, len(cfg.Tenants)),
		byName:  make(map[string]*store.Bucket),
	}
	for _, t := range cfg.Tenants {
		buckets := make(map[string]*store.Bucket, len(t.Buckets))
		for _, b := range t.Buckets {
			bucket := &store.Bucket{
				Tenant:     t.Name,
				Name:       b.Name,
				Backend:    b.Backend,
				PolicyText: b.Policy,
				CORS:       b.CORS,
				CreatedAt:  b.CreatedAt,
			}
			if b.Policy != "" {
				// the config is validated, so this cannot fail
				p, err := policy.Parse(b.Name, b.Policy)
				if err != nil {
					panic(fmt.Sprintf("bucket %s: invalid policy after validation: %v", b.Name, err))
				}
				bucket.Policy = p
			}
			buckets[b.Name] = bucket
			s.byName[b.Name] = bucket
		}
		s.tenants[t.Name] = buckets
		for _, u := range t.Users {
			var up *policy.UserPolicy
			if len(u.Policy) > 0 {
				up = &policy.UserPolicy{Statements: u.Policy}
			}
			for _, k := range u.Keys {
				s.keys[k.AccessKeyID] = &store.Key{
					AccessKeyID:     k.AccessKeyID,
					SecretAccessKey: k.SecretAccessKey,
					Tenant:          t.Name,
					User:            u.Name,
					Policy:          up,
				}
			}
		}
	}
	return s
}

func (s *configStore) GetKey(ctx context.Context, accessKeyID string) (*store.Key, error) {
	if k, ok := s.keys[accessKeyID]; ok {
		return k, nil
	}
	return nil, store.ErrNotFound
}

func (s *configStore) GetBucket(ctx context.Context, tenant, bucket string) (*store.Bucket, error) {
	if b, ok := s.tenants[tenant][bucket]; ok {
		return b, nil
	}
	return nil, store.ErrNotFound
}

func (s *configStore) GetBucketByName(ctx context.Context, bucket string) (*store.Bucket, error) {
	if b, ok := s.byName[bucket]; ok {
		return b, nil
	}
	return nil, store.ErrNotFound
}

func (s *configStore) ListBuckets(ctx context.Context, tenant string) ([]store.BucketEntry, error) {
	buckets, ok := s.tenants[tenant]
	if !ok {
		return nil, store.ErrNotFound
	}
	entries := make([]store.BucketEntry, 0, len(buckets))
	for _, b := range buckets {
		entries = append(entries, store.BucketEntry{Name: b.Name, CreatedAt: b.CreatedAt})
	}
	return entries, nil
}
