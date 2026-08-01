// Package db holds the shared schema and the write-side operations
// (migration and importing a YAML config). It is used by the admin
// tooling only — the proxy must never import this package or writedb.
package db

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/fujiwara/s3rp"
	"github.com/fujiwara/s3rp/db/writedb"
	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/store"
)

//go:embed schema.sql
var schema string

// Migrate applies the schema. The DDL uses IF NOT EXISTS, so it is
// idempotent on an existing database.
func Migrate(ctx context.Context, sqldb *sql.DB) error {
	for stmt := range strings.SplitSeq(schema, ";") {
		if strings.TrimSpace(stmt) == "" {
			continue
		}
		if _, err := sqldb.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("failed to apply schema: %w", err)
		}
	}
	return nil
}

// backendJSON mirrors store.Backend for storage. store.Password masks
// itself on json.Marshal, so the secret is carried as a plain string here.
type backendJSON struct {
	Endpoint        string `json:"endpoint"`
	Region          string `json:"region"`
	Bucket          string `json:"bucket,omitempty"`
	AccessKeyID     string `json:"access_key_id"`
	SecretAccessKey string `json:"secret_access_key"`
	UsePathStyle    *bool  `json:"use_path_style,omitempty"`
}

// Import loads a validated YAML config (tenants form) into the database.
// The target tables must be empty; re-importing over existing definitions
// fails on the unique constraints.
func Import(ctx context.Context, sqldb *sql.DB, cfg *s3rp.Config) error {
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	q := writedb.New(tx)
	for _, t := range cfg.Tenants {
		tenantID, err := q.CreateTenant(ctx, t.Name)
		if err != nil {
			return fmt.Errorf("tenant %s: %w", t.Name, err)
		}
		for _, u := range t.Users {
			userPolicy, err := marshalUserPolicy(u.Policy)
			if err != nil {
				return fmt.Errorf("user %s/%s: %w", t.Name, u.Name, err)
			}
			userID, err := q.CreateUser(ctx, writedb.CreateUserParams{
				TenantID: tenantID,
				Name:     u.Name,
				Policy:   userPolicy,
			})
			if err != nil {
				return fmt.Errorf("user %s/%s: %w", t.Name, u.Name, err)
			}
			for _, k := range u.Keys {
				if err := q.CreateAccessKey(ctx, writedb.CreateAccessKeyParams{
					AccessKeyID:     k.AccessKeyID,
					UserID:          userID,
					SecretAccessKey: k.SecretAccessKey.String(),
				}); err != nil {
					return fmt.Errorf("access key %s: %w", k.AccessKeyID, err)
				}
			}
		}
		for _, b := range t.Buckets {
			backend, err := marshalBackend(b.Backend)
			if err != nil {
				return fmt.Errorf("bucket %s: %w", b.Name, err)
			}
			cors, err := marshalCORS(b.CORS)
			if err != nil {
				return fmt.Errorf("bucket %s: %w", b.Name, err)
			}
			if err := q.CreateBucket(ctx, writedb.CreateBucketParams{
				TenantID: tenantID,
				Name:     b.Name,
				Backend:  backend,
				Policy:   b.Policy,
				Cors:     cors,
			}); err != nil {
				return fmt.Errorf("bucket %s: %w", b.Name, err)
			}
		}
	}
	return tx.Commit()
}

func marshalBackend(b *store.Backend) (string, error) {
	data, err := json.Marshal(backendJSON{
		Endpoint:        b.Endpoint,
		Region:          b.Region,
		Bucket:          b.Bucket,
		AccessKeyID:     b.AccessKeyID,
		SecretAccessKey: b.SecretAccessKey.String(),
		UsePathStyle:    b.UsePathStyle,
	})
	return string(data), err
}

func marshalCORS(rules []*store.CORSRule) (string, error) {
	if len(rules) == 0 {
		return "", nil
	}
	data, err := json.Marshal(rules)
	return string(data), err
}

func marshalUserPolicy(statements []policy.ActionStatement) (string, error) {
	if len(statements) == 0 {
		return "", nil
	}
	// marshal via pointer: UserPolicy carries a sync.Once (a lock), which
	// must not be copied by value.
	data, err := json.Marshal(&policy.UserPolicy{Statements: statements})
	return string(data), err
}
