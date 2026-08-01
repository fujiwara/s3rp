package s3gw

import (
	"io"
	"net/http"

	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/sigv4"
)

var RedactQuery = redactQuery

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
