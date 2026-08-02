package s3rp_test

import (
	"errors"
	"sort"
	"testing"

	"github.com/fujiwara/s3rp"
	"github.com/fujiwara/s3rp/store"
)

// storeContract asserts the store.Store semantics against the definitions
// of testdata/config.yaml. It runs for every Store implementation.
func storeContract(t *testing.T, st store.Store) {
	ctx := t.Context()

	t.Run("GetKey", func(t *testing.T) {
		key, err := st.GetKey(ctx, "S3RPKEY001")
		if err != nil {
			t.Fatal(err)
		}
		if key.Tenant != "acme" || key.User != "app1" || key.SecretAccessKey.String() != "frontsecret001" {
			t.Errorf("unexpected key %+v", key)
		}
		// app1 has no user policy (default allow all)
		if key.Policy != nil {
			t.Errorf("expect no policy for app1, got %+v", key.Policy)
		}
		// a key of another user of the same tenant, carrying a user policy
		key2, err := st.GetKey(ctx, "S3RPKEY003")
		if err != nil {
			t.Fatal(err)
		}
		if key2.Tenant != "acme" || key2.User != "batch" {
			t.Errorf("unexpected key %+v", key2)
		}
		if key2.Policy == nil || key2.Policy.Allows("s3:PutObject") {
			t.Errorf("batch policy should deny PutObject: %+v", key2.Policy)
		}
		if !key2.Policy.Allows("s3:GetObject") || key2.Policy.Allows("s3:GetObjectAcl") {
			t.Errorf("batch policy Get*/deny GetObjectAcl mismatch: %+v", key2.Policy)
		}
		if _, err := st.GetKey(ctx, "NOSUCHKEY"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expect ErrNotFound, got %v", err)
		}
	})
	t.Run("GetBucket", func(t *testing.T) {
		b, err := st.GetBucket(ctx, "acme", "photos")
		if err != nil {
			t.Fatal(err)
		}
		if b.Backend.Bucket != "photos-prod" {
			t.Errorf("unexpected bucket %+v", b)
		}
		if b.Backend.UsePathStyle == nil || !*b.Backend.UsePathStyle {
			t.Errorf("expect use_path_style true, got %+v", b.Backend)
		}
		// another tenant's bucket is not visible
		if _, err := st.GetBucket(ctx, "acme", "research"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expect ErrNotFound, got %v", err)
		}
		if _, err := st.GetBucket(ctx, "nosuchtenant", "photos"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expect ErrNotFound, got %v", err)
		}
	})
	t.Run("GetBucketByName", func(t *testing.T) {
		b, err := st.GetBucketByName(ctx, "research")
		if err != nil {
			t.Fatal(err)
		}
		if b.Tenant != "umbrella" {
			t.Errorf("unexpected tenant %s", b.Tenant)
		}
		if _, err := st.GetBucketByName(ctx, "nosuchbucket"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expect ErrNotFound, got %v", err)
		}
	})
	t.Run("ListBucketNames", func(t *testing.T) {
		names, err := st.ListBucketNames(ctx, "acme")
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(names)
		if len(names) != 2 || names[0] != "logs" || names[1] != "photos" {
			t.Errorf("unexpected names %v", names)
		}
		if _, err := st.ListBucketNames(ctx, "nosuchtenant"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expect ErrNotFound, got %v", err)
		}
	})
}

func loadTestConfig(t *testing.T) *s3rp.Config {
	t.Helper()
	t.Setenv("TEST_BACKEND_ACCESS_KEY_ID", "backendkey")
	t.Setenv("TEST_BACKEND_SECRET_ACCESS_KEY", "backendsecret")
	cfg, err := s3rp.LoadConfig("testdata/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConfigStore(t *testing.T) {
	storeContract(t, s3rp.NewConfigStore(loadTestConfig(t)))
}
