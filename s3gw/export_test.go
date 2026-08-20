package s3gw

import (
	"context"
	"io"
	"net/http"

	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"
)

var RedactQuery = redactQuery

type SignedHeader = signedHeader

func NewSignedHeader(r *http.Request, signed map[string]bool) SignedHeader {
	return newSignedHeader(r, signed)
}

func CheckSSEC(get func(string) string) *s3err.Error { return checkSSEC(get) }

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
