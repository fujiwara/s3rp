package s3rp

import (
	"io"
	"net/http"
	"time"
)

var (
	FromSDKError = fromSDKError
	NewS3Error   = newS3Error
	WriteS3Error = writeS3Error
)

type VerifiedRequest = verifiedRequest

func (app *S3RP) VerifyRequest(r *http.Request) (*VerifiedRequest, *S3Error) {
	return app.verifyRequest(r)
}

func (app *S3RP) SetNow(f func() time.Time) {
	app.now = f
}

// SetBackend replaces the backend client of a bucket for tests by
// pre-warming the client cache for the bucket's backend.
func (app *S3RP) SetBackend(bucket string, client BackendClient) {
	cs := app.store.(*configStore)
	for _, buckets := range cs.tenants {
		if b, ok := buckets[bucket]; ok {
			app.clientsMu.Lock()
			app.clients[newClientCacheKey(b.Backend)] = client
			app.clientsMu.Unlock()
		}
	}
}

func NewChunkedReader(body io.Reader, vr *VerifiedRequest) io.Reader {
	return newChunkedReader(body, vr)
}
