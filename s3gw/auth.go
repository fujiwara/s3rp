package s3gw

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"

	"github.com/fujiwara/s3rp/s3err"

	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"
)

// verifiedRequest is a request whose signature has been verified, together
// with the identity it authenticates. The signature facts live in
// sigv4.Verified — embedded so that call sites keep reading vr.PayloadHash
// and friends — while the tenant, user and its policy are this service's
// domain and are attached here.
type verifiedRequest struct {
	*sigv4.Verified
	Tenant     string
	User       string
	UserPolicy *policy.UserPolicy
	// KeyMetadata is store.Key.Metadata, carried along so dispatch can hand
	// it to the hooks on Op.
	KeyMetadata any
	// SourceIP is the client's source address, parsed from r.RemoteAddr —
	// what bucket-policy IP conditions evaluate. A deployment behind a
	// proxy must rewrite RemoteAddr to the real client address in a handler
	// wrapped around Gateway.Handler(); the gateway itself never interprets
	// X-Forwarded-For. Zero when RemoteAddr does not parse, which fails
	// conditions closed.
	SourceIP netip.Addr
}

// principal is the identity string bucket policies are evaluated with:
// always the qualified "tenant/user" form, regardless of which tenant's
// bucket the request targets.
func (vr *verifiedRequest) principal() string {
	return vr.Tenant + "/" + vr.User
}

// requestContext is the request-constant condition input for bucket-policy
// evaluation.
func (vr *verifiedRequest) requestContext() policy.RequestContext {
	return policy.RequestContext{SourceIP: vr.SourceIP}
}

// remoteIP parses the client address out of r.RemoteAddr, which is normally
// "ip:port" but may be a bare IP when a wrapping handler rewrote it. A
// value that parses as neither yields a zero Addr.
func remoteIP(r *http.Request) netip.Addr {
	if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
		return ap.Addr()
	}
	if a, err := netip.ParseAddr(r.RemoteAddr); err == nil {
		return a
	}
	return netip.Addr{}
}

// secretLookup returns the SecretLookup backing signature verification. The
// store lookup that supplies the secret also yields the identity, so the
// resolved key is captured into *key instead of being looked up twice. The
// presented session token is handed through to the store, which decides what
// to make of it (see store.Store.GetKey).
func (g *Gateway) secretLookup(key **store.Key) sigv4.SecretLookup {
	return func(ctx context.Context, accessKeyID, sessionToken string) (sigv4.Credential, error) {
		k, err := g.store.GetKey(ctx, accessKeyID, sessionToken)
		if err != nil {
			switch {
			case errors.Is(err, store.ErrNotFound):
				return sigv4.Credential{}, sigv4.ErrUnknownKey
			case errors.Is(err, store.ErrInvalidToken):
				// keep the store's error: it becomes the cause the observer
				// sees, and "mac mismatch" vs "revoked" is what one wants
				// to know there
				return sigv4.Credential{}, fmt.Errorf("%w: %w", sigv4.ErrInvalidToken, err)
			}
			return sigv4.Credential{}, err
		}
		*key = k
		return sigv4.Credential{
			SecretAccessKey: k.SecretAccessKey.String(),
			SessionToken:    k.SessionToken,
		}, nil
	}
}

// verifyRequest authenticates an incoming request, either by the
// Authorization header or by presigned URL query parameters, and resolves the
// identity behind the access key.
func (g *Gateway) verifyRequest(r *http.Request) (*verifiedRequest, *s3err.Error) {
	var key *store.Key
	verified, s3e := g.verifier.Verify(r, g.secretLookup(&key))
	if s3e != nil {
		return nil, s3e
	}
	return &verifiedRequest{
		Verified:    verified,
		Tenant:      key.Tenant,
		User:        key.User,
		UserPolicy:  key.Policy,
		KeyMetadata: key.Metadata,
		SourceIP:    remoteIP(r),
	}, nil
}

// verifyPostRequest authenticates a browser-based POST upload from its form
// fields and resolves the identity behind the access key, exactly as
// verifyRequest does for header and presigned authentication.
func (g *Gateway) verifyPostRequest(r *http.Request, fields map[string]string) (*verifiedRequest, *sigv4.PostPolicy, *s3err.Error) {
	var key *store.Key
	verified, pp, s3e := g.verifier.VerifyPost(r, fields, g.secretLookup(&key))
	if s3e != nil {
		return nil, nil, s3e
	}
	return &verifiedRequest{
		Verified:    verified,
		Tenant:      key.Tenant,
		User:        key.User,
		UserPolicy:  key.Policy,
		KeyMetadata: key.Metadata,
		SourceIP:    remoteIP(r),
	}, pp, nil
}
