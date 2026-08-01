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
	g.clientsMu.Lock()
	defer g.clientsMu.Unlock()
	g.clients[newClientCacheKey(b.Backend)] = client
	return nil
}

// SetNow replaces the clock used for signature expiry and clock-skew checks.
func (g *Gateway) SetNow(f func() time.Time) { g.verifier.Now = f }
