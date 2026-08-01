package s3gw

import (
	"context"
	"time"
)

// SetBackend replaces the backend client used for a bucket, by pre-warming
// the client cache for that bucket's backend. It is how a caller substitutes
// a backend — a fake in tests, or a specially configured client — without
// going through the definition store.
func (g *Gateway) SetBackend(bucket string, client BackendClient) error {
	b, err := g.store.GetBucketByName(context.Background(), bucket)
	if err != nil {
		return err
	}
	g.clients.Add(newClientCacheKey(b.Backend), client)
	return nil
}

// SetClientCacheSize resizes the backend client cache (default 128 entries,
// LRU). Size it to the number of distinct backends expected to be active at
// once; an evicted client is rebuilt on its next request. Values below 1 are
// treated as 1.
func (g *Gateway) SetClientCacheSize(n int) {
	if n < 1 {
		n = 1
	}
	g.clients.Resize(n)
}

// SetSignerCacheSize resizes the per-access-key signer cache used by
// signature verification. See sigv4.Verifier.SetSignerCacheSize.
func (g *Gateway) SetSignerCacheSize(n int) { g.verifier.SetSignerCacheSize(n) }

// SetNow replaces the clock used for signature expiry and clock-skew checks.
func (g *Gateway) SetNow(f func() time.Time) { g.verifier.Now = f }
