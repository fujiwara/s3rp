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
	"sync"

	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"
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

	authorizer   Authorizer
	interceptors []Interceptor

	newClient func(ctx context.Context, b *store.Backend) (BackendClient, error)
	clients   map[clientCacheKey]BackendClient
	clientsMu sync.RWMutex
}

// bucketRT is a bucket resolved for a request: its definition and the
// backend client to use.
type bucketRT struct {
	cfg    *store.Bucket
	client BackendClient
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
	return &Gateway{
		store:     st,
		verifier:  sigv4.NewVerifier(),
		newClient: newBackendClient,
		clients:   make(map[clientCacheKey]BackendClient),
	}
}

// backendClient returns the backend client for a backend definition,
// constructing and caching it on first use.
func (g *Gateway) backendClient(ctx context.Context, b *store.Backend) (BackendClient, error) {
	key := newClientCacheKey(b)
	g.clientsMu.RLock()
	client, ok := g.clients[key]
	g.clientsMu.RUnlock()
	if ok {
		return client, nil
	}
	client, err := g.newClient(ctx, b)
	if err != nil {
		return nil, fmt.Errorf("failed to build backend client: %w", err)
	}
	g.clientsMu.Lock()
	defer g.clientsMu.Unlock()
	if cached, ok := g.clients[key]; ok {
		return cached, nil
	}
	g.clients[key] = client
	return client, nil
}
