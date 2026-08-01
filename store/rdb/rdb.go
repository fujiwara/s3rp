// Package rdb is a read-only store.Store implementation backed by a
// relational database (sqlite for the PoC), using the sqlc-generated
// queries in the readdb package. The proxy must never hold write access:
// Open forces mode=ro, and this package only links the read queries —
// writes live in db/writedb, used exclusively by the admin tooling.
package rdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/store"
	"github.com/fujiwara/s3rp/store/rdb/readdb"
	_ "modernc.org/sqlite"
)

// Store is a read-only store.Store backed by sqlite.
type Store struct {
	db *sql.DB
	q  *readdb.Queries
}

var _ store.Store = (*Store)(nil)

// Open opens the sqlite database read-only. A mode= parameter in the DSN
// is rejected: the proxy must not open the database for writing.
func Open(dsn string) (*Store, error) {
	if strings.Contains(dsn, "mode=") {
		return nil, fmt.Errorf("dsn must not specify mode= (the proxy always opens the database read-only)")
	}
	if !strings.HasPrefix(dsn, "file:") {
		dsn = "file:" + dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	dsn += sep + "mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to open database: %w", err)
	}
	return &Store{db: db, q: readdb.New(db)}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) GetKey(ctx context.Context, accessKeyID string) (*store.Key, error) {
	row, err := s.q.GetKey(ctx, accessKeyID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("failed to get key: %w", err)
	}
	key := &store.Key{
		AccessKeyID:     row.AccessKeyID,
		SecretAccessKey: store.Password(row.SecretAccessKey),
		Tenant:          row.TenantName,
		User:            row.UserName,
	}
	if row.UserPolicy != "" {
		if len(row.UserPolicy) > policy.MaxPolicyBytes {
			return nil, fmt.Errorf("user %s: policy is %d bytes, at most %d are allowed", row.UserName, len(row.UserPolicy), policy.MaxPolicyBytes)
		}
		var up policy.UserPolicy
		if err := json.Unmarshal([]byte(row.UserPolicy), &up); err != nil {
			return nil, fmt.Errorf("user %s: malformed policy: %w", row.UserName, err)
		}
		if err := policy.ValidateUserPolicy(&up); err != nil {
			return nil, fmt.Errorf("user %s: invalid policy: %w", row.UserName, err)
		}
		key.Policy = &up
	}
	return key, nil
}

func (s *Store) GetBucket(ctx context.Context, tenant, bucket string) (*store.Bucket, error) {
	row, err := s.q.GetBucket(ctx, readdb.GetBucketParams{Name: tenant, Name_2: bucket})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("failed to get bucket: %w", err)
	}
	return bucketFromRow(row.TenantName, row.Name, row.Backend, row.Policy, row.Cors)
}

func (s *Store) GetBucketByName(ctx context.Context, bucket string) (*store.Bucket, error) {
	row, err := s.q.GetBucketByName(ctx, bucket)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, fmt.Errorf("failed to get bucket: %w", err)
	}
	return bucketFromRow(row.TenantName, row.Name, row.Backend, row.Policy, row.Cors)
}

func (s *Store) ListBucketNames(ctx context.Context, tenant string) ([]string, error) {
	names, err := s.q.ListBucketNames(ctx, tenant)
	if err != nil {
		return nil, fmt.Errorf("failed to list buckets: %w", err)
	}
	if len(names) == 0 {
		exists, err := s.q.TenantExists(ctx, tenant)
		if err != nil {
			return nil, fmt.Errorf("failed to check tenant: %w", err)
		}
		if !exists {
			return nil, store.ErrNotFound
		}
	}
	return names, nil
}

func bucketFromRow(tenant, name, backendJSON, policyText, corsJSON string) (*store.Bucket, error) {
	b := &store.Bucket{
		Tenant:     tenant,
		Name:       name,
		Backend:    &store.Backend{},
		PolicyText: policyText,
	}
	if err := json.Unmarshal([]byte(backendJSON), b.Backend); err != nil {
		return nil, fmt.Errorf("bucket %s: malformed backend definition: %w", name, err)
	}
	// the optional columns may be absent in a row written by another tool, so
	// resolve them here rather than handing a half-populated backend to the
	// client builder
	b.Backend.SetDefaults(name)
	if policyText != "" {
		p, err := policy.Parse(policyText)
		if err != nil {
			return nil, fmt.Errorf("bucket %s: malformed policy: %w", name, err)
		}
		b.Policy = p
	}
	if corsJSON != "" {
		if err := json.Unmarshal([]byte(corsJSON), &b.CORS); err != nil {
			return nil, fmt.Errorf("bucket %s: malformed cors rules: %w", name, err)
		}
	}
	return b, nil
}
