package sigv4

import (
	"hash/maphash"
	"sync"
	"sync/atomic"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// defaultSignerSlots is the default number of slots in the signer cache.
// Memory is bounded to this many signers regardless of how many access keys
// exist, which matters because access keys are per user and can grow without
// limit (unlike backends, which are few). Each slot is a few hundred bytes.
const defaultSignerSlots = 512

// signerCache hands out a SigV4 signer per access key.
//
// The SDK's own derived-key cache (inside v4.Signer) holds a single entry keyed
// by service and region, and validates it against the access key id of the
// request being signed. That fits an SDK client with one set of credentials,
// but a multi-tenant proxy signs with a different key on nearly every request:
// sharing one signer makes that cache miss every time, and each miss takes its
// write lock, serializing signature verification across the whole proxy.
//
// Giving each access key its own signer turns those misses into hits (the
// signing key is derived once per key per day, not per request) and removes the
// shared lock. The cache is direct-mapped: an access key maps to exactly one
// slot, and a collision simply replaces the occupant, so memory stays flat and
// there is no eviction bookkeeping. A displaced key just re-derives on its next
// request, which is the behavior we had before this cache existed.
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
	mu          sync.RWMutex
	accessKeyID string
	signer      *v4.Signer
}

func newSignerCache(size int) *signerCache {
	if size < 1 {
		size = 1
	}
	return &signerCache{seed: maphash.MakeSeed(), slots: make([]signerSlot, size)}
}

// get returns the signer for an access key id, creating it on first use.
func (c *signerCache) get(accessKeyID string) *v4.Signer {
	slot := &c.slots[maphash.String(c.seed, accessKeyID)%uint64(len(c.slots))]

	slot.mu.RLock()
	if slot.accessKeyID == accessKeyID {
		signer := slot.signer
		slot.mu.RUnlock()
		c.hits.Add(1)
		return signer
	}
	slot.mu.RUnlock()
	c.misses.Add(1)

	signer := newSigV4Signer()
	slot.mu.Lock()
	// another goroutine may have installed the same key meanwhile; reuse it so
	// concurrent requests for one key converge on a single signer (and its
	// derived-key cache)
	if slot.accessKeyID == accessKeyID {
		signer = slot.signer
	} else {
		if slot.accessKeyID != "" {
			c.displacements.Add(1)
		}
		slot.accessKeyID = accessKeyID
		slot.signer = signer
	}
	slot.mu.Unlock()
	return signer
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
		if slot.accessKeyID != "" {
			s.Len++
		}
		slot.mu.RUnlock()
	}
	return s
}

// CacheStats is a point-in-time snapshot of the signer cache, for sizing it
// (SetSignerCacheSize). Counters are monotonic since the cache was created —
// SetSignerCacheSize replaces the cache and resets them — and the fields are
// read independently: approximately consistent, for monitoring, not
// accounting. Evictions counts collision displacements; a high rate with Len
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

// SignerCacheStats snapshots the per-access-key signer cache.
func (v *Verifier) SignerCacheStats() CacheStats {
	return v.signers.stats()
}

func newSigV4Signer() *v4.Signer {
	return v4.NewSigner(func(o *v4.SignerOptions) {
		o.DisableURIPathEscaping = true // S3 mode
	})
}
