package s3gw

import (
	"context"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/store"
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

// SetClientOptions installs a hook that contributes s3.Options to every
// backend client the gateway builds — a custom Retryer, an instrumented
// HTTPClient (e.g. an otelhttp transport), timeouts. It receives the backend
// definition so the options can differ per backend, and its options are
// applied after the gateway's own, so they can adjust or knowingly override
// them — note the defaults documented on newBackendClient are load-bearing
// for non-AWS backends. Call before serving requests: clients are cached per
// backend, and a client built earlier keeps the options it was built with.
// The hook must be deterministic for a given backend, since a cached client
// is reused without consulting it again.
func (g *Gateway) SetClientOptions(fn func(b *store.Backend) []func(*s3.Options)) {
	g.clientOptions = fn
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

// SetRegion pins the signing region this endpoint accepts; unset, any region
// verifies. See sigv4.Verifier.SetRegion.
func (g *Gateway) SetRegion(region string) { g.verifier.SetRegion(region) }

// SetNow replaces the clock used for signature expiry and clock-skew checks.
func (g *Gateway) SetNow(f func() time.Time) { g.verifier.Now = f }
