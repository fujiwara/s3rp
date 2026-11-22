package s3gw

import (
	"context"
	"io"
	"net/http"
	"time"

	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"
)

var RedactQuery = redactQuery

type SignedHeader = signedHeader

func NewSignedHeader(r *http.Request, signed map[string]bool) SignedHeader {
	return newSignedHeader(r, signed)
}

func CheckSSEC(hdr SignedHeader) *s3err.Error { return checkSSEC(hdr) }

func NewPayloadVerifier(r io.Reader, want string, length int64) io.Reader {
	return newPayloadVerifier(r, want, length)
}

// VerifiedRequest is the signature side of a verified request; the identity
// the gateway attaches to it is not needed by the tests that build one.
type VerifiedRequest = sigv4.Verified

func (g *Gateway) VerifyRequest(r *http.Request) (*VerifiedRequest, *s3err.Error) {
	vr, err := g.verifyRequest(r)
	if err != nil {
		return nil, err
	}
	return vr.Verified, nil
}

func (g *Gateway) SetNewClient(f func(ctx context.Context, b *store.Backend) (BackendClient, error)) {
	g.newClient = f
}

func (g *Gateway) BackendClientFor(ctx context.Context, b *store.Backend) (BackendClient, error) {
	return g.backendClient(ctx, b)
}

func (g *Gateway) ClientCacheLen() int { return g.clients.Len() }

// SetBreakerClock replaces the bundled breaker's clock.
func SetBreakerClock(b *ConsecutiveFailures, now func() time.Time) { b.now = now }

// ClassifyAttempt exposes the breaker's outcome classification.
var ClassifyAttempt = classifyAttempt

var BackendName = backendName

// KnownAmzHeaders exposes the request-header allowlist to
// TestKnownAmzHeadersCoverSource.
var KnownAmzHeaders = knownAmzHeaders

var IgnoredAmzHeaders = ignoredAmzHeaders

// RouteOperations lists the operation names the route tables dispatch to,
// copy variants included.
func RouteOperations() []string {
	var out []string
	for _, routes := range []map[string][]route{bucketRoutes, objectRoutes} {
		for _, rs := range routes {
			for _, rt := range rs {
				if rt.name != OpUnknown {
					out = append(out, rt.name)
				}
				if rt.copy != "" {
					out = append(out, rt.copy)
				}
			}
		}
	}
	return out
}

// AuthorizationDecision is what the authorization path decides for one
// requester, action and resource, on each of its three ways there.
type AuthorizationDecision struct {
	Visible    bool // resolveBucket resolved the bucket for the requester
	Allowed    bool // authorize let the action through
	PerObject  bool // perObjectAuthorizer let the resource through
	SkipsCheck bool // perObjectAuthorizer said no resource needs testing
}

// DecideAs runs the authorization path as the holder of key would reach it,
// without a request around it.
func (g *Gateway) DecideAs(ctx context.Context, key *store.Key, bucket, action, resource string) AuthorizationDecision {
	vr := &verifiedRequest{Tenant: key.Tenant, User: key.User, UserPolicy: key.Policy}
	b, s3e := g.resolveBucket(ctx, vr, bucket)
	if s3e != nil {
		return AuthorizationDecision{}
	}
	a := g.perObjectAuthorizer(vr, b, action)
	return AuthorizationDecision{
		Visible:    true,
		Allowed:    g.authorize(vr, b, action, resource) == nil,
		PerObject:  !a.denies(resource),
		SkipsCheck: a.allowsEverything(),
	}
}
