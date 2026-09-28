package sigv4

import "testing"

func TestSignerCacheSingleSlot(t *testing.T) {
	c := newSignerCache(1)
	if len(c.slots) != 1 {
		t.Fatalf("slots = %d, want 1", len(c.slots))
	}
	// with a single slot every key collides: each key must still get a
	// signer, a repeated key reuses the occupant, a displaced key gets a
	// fresh one
	s1 := c.get("KEY1")
	if s1 == nil {
		t.Fatal("nil signer")
	}
	if c.get("KEY1") != s1 {
		t.Error("same key must reuse the cached signer")
	}
	if c.get("KEY2") == nil {
		t.Fatal("nil signer for colliding key")
	}
	if c.get("KEY1") == s1 {
		t.Error("displaced key must get a fresh signer")
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
	v.signers.get("a") // miss
	v.signers.get("a") // hit
	v.signers.get("b") // miss, displaces a
	v.SetSignerCacheSize(8)
	got := v.SignerCacheStats()
	if got.Hits != 1 || got.Misses != 2 || got.Evictions != 1 || got.Len != 0 || got.Capacity != 8 {
		t.Errorf("stats after resize = %+v, want counters kept and an empty cache of 8", got)
	}
}
