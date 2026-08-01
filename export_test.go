package s3rp

import (
	"context"
	"io"
	"net/http"
	"time"
)

var (
	FromSDKError = fromSDKError
	NewS3Error   = newS3Error
	WriteS3Error = writeS3Error
	RedactQuery  = redactQuery
)

func NewPayloadVerifier(r io.Reader, want string, length int64) io.Reader {
	return newPayloadVerifier(r, want, length)
}

type VerifiedRequest = verifiedRequest

func (app *S3RP) VerifyRequest(r *http.Request) (*VerifiedRequest, *S3Error) {
	return app.verifyRequest(r)
}

func (app *S3RP) SetNow(f func() time.Time) {
	app.now = f
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
	return newChunkedReader(body, vr, trailerAlg)
}
