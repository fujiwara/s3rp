package s3rp

import (
	"context"

	"github.com/fujiwara/s3rp/store"
)

// configStore is a store.Store backed by the static YAML config.
type configStore struct {
	keys    map[string]*store.Key
	tenants map[string]map[string]*store.Bucket // tenant name -> bucket name -> bucket
}

// NewConfigStore builds a store.Store from a validated config.
func NewConfigStore(cfg *Config) store.Store {
	s := &configStore{
		keys:    make(map[string]*store.Key),
		tenants: make(map[string]map[string]*store.Bucket, len(cfg.Tenants)),
	}
	for _, t := range cfg.Tenants {
		buckets := make(map[string]*store.Bucket, len(t.Buckets))
		for _, b := range t.Buckets {
			buckets[b.Name] = &store.Bucket{
				Tenant:  t.Name,
				Name:    b.Name,
				Backend: b.Backend,
			}
		}
		s.tenants[t.Name] = buckets
		for _, k := range t.Keys {
			s.keys[k.AccessKeyID] = &store.Key{
				AccessKeyID:     k.AccessKeyID,
				SecretAccessKey: k.SecretAccessKey,
				Tenant:          t.Name,
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

func (s *configStore) ListBucketNames(ctx context.Context, tenant string) ([]string, error) {
	buckets, ok := s.tenants[tenant]
	if !ok {
		return nil, store.ErrNotFound
	}
	names := make([]string, 0, len(buckets))
	for name := range buckets {
		names = append(names, name)
	}
	return names, nil
}
