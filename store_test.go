package s3rp_test

import (
	"errors"
	"sort"
	"testing"

	"github.com/fujiwara/s3rp"
	"github.com/fujiwara/s3rp/store"
)

func TestConfigStore(t *testing.T) {
	t.Setenv("TEST_BACKEND_ACCESS_KEY_ID", "backendkey")
	t.Setenv("TEST_BACKEND_SECRET_ACCESS_KEY", "backendsecret")
	cfg, err := s3rp.LoadConfig("testdata/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	st := s3rp.NewConfigStore(cfg)
	ctx := t.Context()

	t.Run("GetKey", func(t *testing.T) {
		key, err := st.GetKey(ctx, "S3RPKEY001")
		if err != nil {
			t.Fatal(err)
		}
		if key.Tenant != "acme" || key.User != "app1" || key.SecretAccessKey.String() != "frontsecret001" {
			t.Errorf("unexpected key %+v", key)
		}
		// a key of another user of the same tenant
		key2, err := st.GetKey(ctx, "S3RPKEY003")
		if err != nil {
			t.Fatal(err)
		}
		if key2.Tenant != "acme" || key2.User != "batch" {
			t.Errorf("unexpected key %+v", key2)
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
		// another tenant's bucket is not visible
		if _, err := st.GetBucket(ctx, "acme", "research"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expect ErrNotFound, got %v", err)
		}
		if _, err := st.GetBucket(ctx, "nosuchtenant", "photos"); !errors.Is(err, store.ErrNotFound) {
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
