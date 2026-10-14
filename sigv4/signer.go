package sigv4

import (
	"hash/maphash"
	"sync"
	"sync/atomic"
)

// defaultSignerSlots is the default number of slots in the signer cache.
// Memory is bounded to this many entries regardless of how many access keys
// exist, which matters because access keys are per user and can grow without
// limit (unlike backends, which are few). Each slot is a few hundred bytes.
const defaultSignerSlots = 512

// signerCache holds the derived SigV4 signing key per access key. Deriving
// one is four HMACs, more than the rest of a verification costs, and the key
// only changes with the date, the region or the secret, so a client's
// requests reuse it for the day.
//
// The cache is direct-mapped: an access key maps to exactly one slot, and a
// collision simply replaces the occupant, so memory stays flat and there is
// no eviction bookkeeping. A displaced key just re-derives on its next
// request. It is deliberately not an LRU: golang-lru takes an exclusive lock
// even on Get, which would serialize verification across the whole proxy.
type signerCache struct {
	seed  maphash.Seed
	slots []signerSlot

	// monotonic counters, atomics so the lock-free shape of the hot path
	// is preserved (the reason this cache exists is avoiding a shared
	// exclusive lock). displacements counts collisions replacing an
	// occupant — this cache's analog of an eviction.
	hits, misses, displacements atomic.Uint64
}

type signerSlot struct {
	mu  sync.RWMutex
	key signingKeyID
	// signingKey is the derived key for key; never mutated once stored,
	// so it is handed out without copying
	signingKey []byte
}

// signingKeyID is everything a signing key is derived from. The secret is
// part of it so that a secret changed under the same access key id never
// verifies against the old one's key.
type signingKeyID struct {
	accessKeyID, secret, date, region string
}

func newSignerCache(size int) *signerCache {
	if size < 1 {
		size = 1
	}
	return &signerCache{seed: maphash.MakeSeed(), slots: make([]signerSlot, size)}
}

// get returns the signing key for a credential scope, deriving it on a miss.
func (c *signerCache) get(id signingKeyID) []byte {
	slot := &c.slots[maphash.String(c.seed, id.accessKeyID)%uint64(len(c.slots))]

	slot.mu.RLock()
	if slot.key == id {
		k := slot.signingKey
		slot.mu.RUnlock()
		c.hits.Add(1)
		return k
	}
	slot.mu.RUnlock()
	c.misses.Add(1)

	k := deriveSigningKey(id.secret, id.date, id.region)
	slot.mu.Lock()
	// a new date or region for the occupant is a refresh, not a displacement
	if slot.key.accessKeyID != "" && slot.key.accessKeyID != id.accessKeyID {
		c.displacements.Add(1)
	}
	slot.key = id
	slot.signingKey = k
	slot.mu.Unlock()
	return k
}

// stats snapshots the cache counters and scans the slots for occupancy. The
// scan takes each slot's read lock briefly; this runs at scrape frequency,
// not on the request path.
func (c *signerCache) stats() CacheStats {
	s := CacheStats{
		Hits:      c.hits.Load(),
		Misses:    c.misses.Load(),
		Evictions: c.displacements.Load(),
		Capacity:  len(c.slots),
	}
	for i := range c.slots {
		slot := &c.slots[i]
		slot.mu.RLock()
		if slot.key.accessKeyID != "" {
			s.Len++
		}
		slot.mu.RUnlock()
	}
	return s
}

// CacheStats is a point-in-time snapshot of the signer cache, for sizing it
// (SetSignerCacheSize). Counters are monotonic for the Verifier's lifetime —
// SetSignerCacheSize replaces the cache but carries them over — and the
// fields are read independently: approximately consistent, for monitoring,
// not accounting. Evictions counts collision displacements; a high rate with Len
// well under Capacity means hot keys colliding on a slot (more slots lower
// the odds), while a high rate with Len near Capacity means the cache is
// simply too small for the active key set.
type CacheStats struct {
	Hits      uint64 `json:"hits"`
	Misses    uint64 `json:"misses"`
	Evictions uint64 `json:"evictions"`
	Len       int    `json:"len"`
	Capacity  int    `json:"capacity"`
}

// SignerCacheStats snapshots the per-access-key signing-key cache.
func (v *Verifier) SignerCacheStats() CacheStats {
	return v.signers.stats()
}
