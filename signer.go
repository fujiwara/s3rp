package s3rp

import (
	"hash/maphash"
	"sync"

	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
)

// signerShards is the fixed number of slots in the signer cache. Memory is
// bounded to this many signers regardless of how many access keys exist, which
// matters because access keys are per user and can grow without limit (unlike
// backends, which are few). Each slot is a few hundred bytes.
const signerShards = 512

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
	seed   maphash.Seed
	shards [signerShards]signerSlot
}

type signerSlot struct {
	mu          sync.RWMutex
	accessKeyID string
	signer      *v4.Signer
}

func newSignerCache() *signerCache {
	return &signerCache{seed: maphash.MakeSeed()}
}

// get returns the signer for an access key id, creating it on first use.
func (c *signerCache) get(accessKeyID string) *v4.Signer {
	slot := &c.shards[maphash.String(c.seed, accessKeyID)%signerShards]

	slot.mu.RLock()
	if slot.accessKeyID == accessKeyID {
		signer := slot.signer
		slot.mu.RUnlock()
		return signer
	}
	slot.mu.RUnlock()

	signer := newSigV4Signer()
	slot.mu.Lock()
	// another goroutine may have installed the same key meanwhile; reuse it so
	// concurrent requests for one key converge on a single signer (and its
	// derived-key cache)
	if slot.accessKeyID == accessKeyID {
		signer = slot.signer
	} else {
		slot.accessKeyID = accessKeyID
		slot.signer = signer
	}
	slot.mu.Unlock()
	return signer
}

func newSigV4Signer() *v4.Signer {
	return v4.NewSigner(func(o *v4.SignerOptions) {
		o.DisableURIPathEscaping = true // S3 mode
	})
}
