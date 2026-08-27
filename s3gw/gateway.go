// Package s3gw implements the S3 API of a multi-tenant gateway: signature
// verification, authorization, the operations themselves and the routing that
// reaches them. It re-executes each operation against the bucket's backend
// through the aws-sdk rather than forwarding the request, so that every
// operation is understood and the backend's identity stays hidden.
//
// What it deliberately leaves to the caller: where tenant, bucket and key
// definitions come from (a store.Store), and what a service meters or refuses
// (SetAuthorizer and Use).
package s3gw

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"

	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	lru "github.com/hashicorp/golang-lru/v2"
)

// Byte-size units, so size constants read with their unit instead of as a
// bare shift.
const (
	kib = 1 << 10
	mib = 1 << 20
)

// Gateway serves the S3 API: it verifies the SigV4 signature of each request
// with the front-side access key, authorizes the operation against the bucket
// and user policies, and re-executes it against the bucket's backend.
//
// It knows nothing about where definitions come from — that is the Store —
// nor about what a service charges for or refuses; those are reached through
// SetAuthorizer and Use.
type Gateway struct {
	store    store.Store
	verifier *sigv4.Verifier

	// region is the SetRegion value: the signing region this endpoint
	// accepts and the region it reports to clients. The backend's region is
	// backend identity and is never exposed.
	region string

	// vhostSuffix is the SetVirtualHostSuffix value, normalized to a leading
	// dot; empty means path-style addressing only.
	vhostSuffix string

	authorizer     Authorizer
	interceptors   []Interceptor
	observer       Observer
	requestID      func(r *http.Request) string
	bandwidthLimit func(op *Op) (in, out BandwidthLimiter)

	newClient     func(ctx context.Context, b *store.Backend) (BackendClient, error)
	clientOptions func(b *store.Backend) []func(*s3.Options)
	clients       *lru.Cache[clientCacheKey, BackendClient]

	// client cache counters; the capacity is stored here because the LRU
	// does not report its own bound
	clientHits, clientMisses, clientEvictions atomic.Uint64
	clientCacheCap                            atomic.Int64
}

// defaultClientCacheSize bounds the backend client cache. Each client carries
// its own HTTP connection pool, and with a database store the number of
// distinct backends grows with tenants, so the cache must not be unbounded.
// Adjustable with SetClientCacheSize.
const defaultClientCacheSize = 128

// bucketRT is a bucket resolved for a request: its definition and the
// backend client to use.
type bucketRT struct {
	cfg    *store.Bucket
	client BackendClient
	// target is how the request addressed the bucket, for the URLs handed
	// back to the client
	target target
}

// clientCacheKey identifies a backend client. Clients are bucket-agnostic:
// buckets sharing the same backend endpoint and credentials share a client.
type clientCacheKey struct {
	endpoint     string
	region       string
	accessKeyID  string
	secret       store.Password
	usePathStyle bool
}

func newClientCacheKey(b *store.Backend) clientCacheKey {
	return clientCacheKey{
		endpoint:     b.Endpoint,
		region:       b.Region,
		accessKeyID:  b.AccessKeyID,
		secret:       b.SecretAccessKey,
		usePathStyle: b.UsePathStyle != nil && *b.UsePathStyle,
	}
}

// New returns a Gateway serving the definitions in st.
func New(st store.Store) *Gateway {
	g := &Gateway{
		store:    st,
		verifier: sigv4.NewVerifier(),
	}
	g.clients, _ = lru.NewWithEvict(defaultClientCacheSize,
		func(clientCacheKey, BackendClient) { g.clientEvictions.Add(1) })
	g.clientCacheCap.Store(defaultClientCacheSize)
	g.newClient = func(ctx context.Context, b *store.Backend) (BackendClient, error) {
		return newBackendClient(ctx, b, g.clientOptions)
	}
	return g
}

// backendClient returns the backend client for a backend definition,
// constructing and caching it on first use. An evicted client is simply
// rebuilt on its next request; its idle connections close on their own.
func (g *Gateway) backendClient(ctx context.Context, b *store.Backend) (BackendClient, error) {
	key := newClientCacheKey(b)
	if client, ok := g.clients.Get(key); ok {
		g.clientHits.Add(1)
		return client, nil
	}
	g.clientMisses.Add(1)
	client, err := g.newClient(ctx, b)
	if err != nil {
		return nil, fmt.Errorf("failed to build backend client: %w", err)
	}
	// another goroutine may have built the same client meanwhile; converge on
	// the one already cached so a backend keeps a single connection pool
	if previous, ok, _ := g.clients.PeekOrAdd(key, client); ok {
		return previous, nil
	}
	return client, nil
}
