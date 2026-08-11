package harness_test

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/fujiwara/s3rp/s3tests/harness"
	"github.com/fujiwara/s3rp/store"
	"github.com/google/go-cmp/cmp"
)

func newStore() *harness.MemStore {
	return harness.NewMemStore("http://127.0.0.1:7480", "backendkey", "backendsecret", harness.DefaultKeys())
}

func TestStoreGetKey(t *testing.T) {
	st := newStore()
	k, err := st.GetKey(t.Context(), harness.MainAccessKeyID, "")
	if err != nil {
		t.Fatal(err)
	}
	if k.Tenant != "main" || k.User != "tester" {
		t.Errorf("unexpected key identity: %s/%s", k.Tenant, k.User)
	}
	if _, err := st.GetKey(t.Context(), "NOSUCHKEY0000000000X", ""); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound, got %v", err)
	}
}

func TestStoreBucketLifecycle(t *testing.T) {
	st := newStore()
	if owner, ok := st.Claim("bkt-a", "main"); !ok || owner != "main" {
		t.Fatalf("claim failed: %s %v", owner, ok)
	}
	// claiming a taken name reports the current owner
	if owner, ok := st.Claim("bkt-a", "alt"); ok || owner != "main" {
		t.Errorf("expected claim refusal with owner main, got %s %v", owner, ok)
	}

	b, err := st.GetBucket(t.Context(), "main", "bkt-a")
	if err != nil {
		t.Fatal(err)
	}
	if b.Tenant != "main" || b.Name != "bkt-a" {
		t.Errorf("unexpected bucket: %+v", b)
	}
	// the backend must come fully resolved (SetDefaults applied)
	be := b.Backend
	if be.Bucket != "bkt-a" || be.Region != store.DefaultRegion || be.UsePathStyle == nil || !*be.UsePathStyle {
		t.Errorf("backend not defaulted: %+v", be)
	}
	if b.CreatedAt.IsZero() {
		t.Error("CreatedAt not set")
	}

	// tenant isolation on GetBucket; GetBucketByName crosses tenants
	if _, err := st.GetBucket(t.Context(), "alt", "bkt-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound for other tenant, got %v", err)
	}
	if _, err := st.GetBucketByName(t.Context(), "bkt-a"); err != nil {
		t.Errorf("GetBucketByName: %v", err)
	}

	st.Remove("bkt-a")
	if _, err := st.GetBucketByName(t.Context(), "bkt-a"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("expected ErrNotFound after remove, got %v", err)
	}
}

func TestStoreListBuckets(t *testing.T) {
	st := newStore()
	// an unknown tenant lists as empty, never as an error (an error would
	// surface as InternalError 500 from the gateway)
	got, err := st.ListBuckets(t.Context(), "nobody")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("expected empty non-nil slice, got %#v", got)
	}

	st.Claim("bkt-b", "main")
	st.Claim("bkt-a", "main")
	st.Claim("bkt-c", "alt")
	got, err = st.ListBuckets(t.Context(), "main")
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(got))
	for i, e := range got {
		names[i] = e.Name
		if e.CreatedAt.IsZero() {
			t.Errorf("entry %s has zero CreatedAt", e.Name)
		}
	}
	if diff := cmp.Diff([]string{"bkt-a", "bkt-b"}, names); diff != "" {
		t.Errorf("unexpected listing (-want +got):\n%s", diff)
	}
}

func TestStoreConcurrency(t *testing.T) {
	st := newStore()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			name := fmt.Sprintf("bkt-%d", i)
			st.Claim(name, "main")
			st.GetBucketByName(t.Context(), name)
			st.ListBuckets(t.Context(), "main")
			st.Remove(name)
		})
	}
	wg.Wait()
}
