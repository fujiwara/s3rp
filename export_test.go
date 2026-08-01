package s3rp

import (
	"context"
	"github.com/fujiwara/s3rp/s3err"
	"io"
	"net/http"
	"time"

	"github.com/fujiwara/s3rp/sigv4"
)

var RedactQuery = redactQuery

func NewPayloadVerifier(r io.Reader, want string, length int64) io.Reader {
	return newPayloadVerifier(r, want, length)
}

// VerifiedRequest is the signature side of a verified request; the identity
// this service attaches to it is not needed by the tests that build one.
type VerifiedRequest = sigv4.Verified

func (app *S3RP) VerifyRequest(r *http.Request) (*VerifiedRequest, *s3err.Error) {
	vr, err := app.verifyRequest(r)
	if err != nil {
		return nil, err
	}
	return vr.Verified, nil
}

func (app *S3RP) SetNow(f func() time.Time) {
	app.verifier.Now = f
}

// SetBackend replaces the backend client of a bucket for tests by
// pre-warming the client cache for the bucket's backend. Works with any
// Store implementation.
func (app *S3RP) SetBackend(bucket string, client BackendClient) {
	b, err := app.store.GetBucketByName(context.Background(), bucket)
	if err != nil {
		panic(err)
	}
	app.clientsMu.Lock()
	app.clients[newClientCacheKey(b.Backend)] = client
	app.clientsMu.Unlock()
}

func NewChunkedReader(body io.Reader, vr *VerifiedRequest, trailerAlg string) io.Reader {
	return sigv4.NewChunkedReader(body, vr, trailerAlg)
}
