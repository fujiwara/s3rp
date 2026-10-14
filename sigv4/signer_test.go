package sigv4

import (
	"bytes"
	"testing"
)

func testKeyID(akid string) signingKeyID {
	return signingKeyID{accessKeyID: akid, secret: "secret-" + akid, date: "20261014", region: "us-east-1"}
}

func TestSignerCacheSingleSlot(t *testing.T) {
	c := newSignerCache(1)
	if len(c.slots) != 1 {
		t.Fatalf("slots = %d, want 1", len(c.slots))
	}
	// with a single slot every key collides: each key must still get its
	// own signing key, a repeated key is a hit, a displaced key re-derives
	k1 := testKeyID("KEY1")
	c.get(k1)
	c.get(k1)
	c.get(testKeyID("KEY2"))
	got := c.get(k1)
	if want := deriveSigningKey(k1.secret, k1.date, k1.region); !bytes.Equal(got, want) {
		t.Error("displaced key must re-derive its own signing key")
	}
	s := c.stats()
	if s.Hits != 1 || s.Misses != 3 || s.Evictions != 2 {
		t.Errorf("stats = %+v, want 1 hit, 3 misses, 2 evictions", s)
	}
}

// TestSignerCacheKeyedByScopeAndSecret: a cached key must never answer for a
// different date, region or secret under the same access key id — a secret
// rotated in place must stop verifying immediately — and such a refresh is
// not a displacement.
func TestSignerCacheKeyedByScopeAndSecret(t *testing.T) {
	c := newSignerCache(8)
	base := testKeyID("KEY1")
	c.get(base)
	for _, id := range []signingKeyID{
		{accessKeyID: "KEY1", secret: "rotated", date: base.date, region: base.region},
		{accessKeyID: "KEY1", secret: base.secret, date: "20261015", region: base.region},
		{accessKeyID: "KEY1", secret: base.secret, date: base.date, region: "ap-northeast-1"},
	} {
		if got, want := c.get(id), deriveSigningKey(id.secret, id.date, id.region); !bytes.Equal(got, want) {
			t.Errorf("get(%+v) returned a key derived from something else", id)
		}
	}
	if s := c.stats(); s.Hits != 0 || s.Evictions != 0 {
		t.Errorf("stats = %+v, want no hits and no evictions", s)
	}
}

func TestSignerCacheSizeClamp(t *testing.T) {
	for _, n := range []int{0, -5} {
		if got := len(newSignerCache(n).slots); got != 1 {
			t.Errorf("newSignerCache(%d) slots = %d, want 1", n, got)
		}
	}
}

func TestSetSignerCacheSize(t *testing.T) {
	v := NewVerifier()
	if got := len(v.signers.slots); got != defaultSignerSlots {
		t.Fatalf("default slots = %d, want %d", got, defaultSignerSlots)
	}
	v.SetSignerCacheSize(64)
	if got := len(v.signers.slots); got != 64 {
		t.Errorf("slots after SetSignerCacheSize(64) = %d, want 64", got)
	}
}

// TestSetSignerCacheSizeKeepsCounters: the counters back metrics counters,
// which must never go backwards.
func TestSetSignerCacheSizeKeepsCounters(t *testing.T) {
	v := NewVerifier()
	v.SetSignerCacheSize(1)
	v.signers.get(testKeyID("a")) // miss
	v.signers.get(testKeyID("a")) // hit
	v.signers.get(testKeyID("b")) // miss, displaces a
	v.SetSignerCacheSize(8)
	got := v.SignerCacheStats()
	if got.Hits != 1 || got.Misses != 2 || got.Evictions != 1 || got.Len != 0 || got.Capacity != 8 {
		t.Errorf("stats after resize = %+v, want counters kept and an empty cache of 8", got)
	}
}
