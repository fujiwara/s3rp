package s3gw_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

// fakeClient is a distinct BackendClient per build, so the tests can tell a
// cached client from a rebuilt one. Its methods are never called.
type fakeClient struct {
	s3gw.BackendClient
	build int
}

func lruTestBackend(i int) *store.Backend {
	pathStyle := true
	return &store.Backend{
		Endpoint: fmt.Sprintf("http://backend%02d.invalid", i), Region: "us-east-1",
		Bucket: "b", AccessKeyID: "bk", SecretAccessKey: "bs", UsePathStyle: &pathStyle,
	}
}

func TestClientCacheLRU(t *testing.T) {
	gw := s3gw.New(memStore{})
	builds := 0
	gw.SetNewClient(func(_ context.Context, _ *store.Backend) (s3gw.BackendClient, error) {
		builds++
		return &fakeClient{build: builds}, nil
	})
	gw.SetClientCacheSize(2)

	get := func(i int) s3gw.BackendClient {
		t.Helper()
		c, err := gw.BackendClientFor(t.Context(), lruTestBackend(i))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	c0 := get(0)
	if get(0) != c0 {
		t.Error("same backend must reuse the cached client")
	}
	if builds != 1 {
		t.Errorf("builds = %d, want 1", builds)
	}

	get(1) // cache holds backends 0 and 1
	get(2) // evicts backend 0
	if builds != 3 {
		t.Errorf("builds = %d, want 3", builds)
	}
	if n := gw.ClientCacheLen(); n != 2 {
		t.Errorf("cache len = %d, want 2", n)
	}
	if get(0) == c0 {
		t.Error("evicted backend must get a rebuilt client")
	}
	if builds != 4 {
		t.Errorf("builds = %d, want 4", builds)
	}
}

func TestSetClientCacheSizeEvicts(t *testing.T) {
	gw := s3gw.New(memStore{})
	gw.SetNewClient(func(_ context.Context, _ *store.Backend) (s3gw.BackendClient, error) {
		return &fakeClient{}, nil
	})
	for i := range 4 {
		if _, err := gw.BackendClientFor(t.Context(), lruTestBackend(i)); err != nil {
			t.Fatal(err)
		}
	}
	if n := gw.ClientCacheLen(); n != 4 {
		t.Fatalf("cache len = %d, want 4", n)
	}
	gw.SetClientCacheSize(2)
	if n := gw.ClientCacheLen(); n != 2 {
		t.Errorf("cache len after shrink = %d, want 2", n)
	}
	gw.SetClientCacheSize(0) // below 1 is treated as 1
	if n := gw.ClientCacheLen(); n != 1 {
		t.Errorf("cache len after size 0 = %d, want 1", n)
	}
}
