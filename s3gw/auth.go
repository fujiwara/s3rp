package s3gw

import (
	"context"
	"errors"
	"net/http"

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
}

// verifyRequest authenticates an incoming request, either by the
// Authorization header or by presigned URL query parameters, and resolves the
// identity behind the access key.
func (g *Gateway) verifyRequest(r *http.Request) (*verifiedRequest, *s3err.Error) {
	// the store lookup that supplies the secret also yields the identity, so
	// capture it here instead of looking the key up twice
	var key *store.Key
	verified, s3e := g.verifier.Verify(r, func(ctx context.Context, accessKeyID string) (sigv4.Credential, error) {
		k, err := g.store.GetKey(ctx, accessKeyID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return sigv4.Credential{}, sigv4.ErrUnknownKey
			}
			return sigv4.Credential{}, err
		}
		key = k
		return sigv4.Credential{
			SecretAccessKey: k.SecretAccessKey.String(),
			SessionToken:    k.SessionToken,
		}, nil
	})
	if s3e != nil {
		return nil, s3e
	}
	return &verifiedRequest{
		Verified:    verified,
		Tenant:      key.Tenant,
		User:        key.User,
		UserPolicy:  key.Policy,
		KeyMetadata: key.Metadata,
	}, nil
}

// verifyPostRequest authenticates a browser-based POST upload from its form
// fields and resolves the identity behind the access key, exactly as
// verifyRequest does for header and presigned authentication.
func (g *Gateway) verifyPostRequest(r *http.Request, fields map[string]string) (*verifiedRequest, *sigv4.PostPolicy, *s3err.Error) {
	var key *store.Key
	verified, pp, s3e := g.verifier.VerifyPost(r, fields, func(ctx context.Context, accessKeyID string) (sigv4.Credential, error) {
		k, err := g.store.GetKey(ctx, accessKeyID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return sigv4.Credential{}, sigv4.ErrUnknownKey
			}
			return sigv4.Credential{}, err
		}
		key = k
		return sigv4.Credential{
			SecretAccessKey: k.SecretAccessKey.String(),
			SessionToken:    k.SessionToken,
		}, nil
	})
	if s3e != nil {
		return nil, nil, s3e
	}
	return &verifiedRequest{
		Verified:    verified,
		Tenant:      key.Tenant,
		User:        key.User,
		UserPolicy:  key.Policy,
		KeyMetadata: key.Metadata,
	}, pp, nil
}
