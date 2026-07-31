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

// SetBackend replaces the backend client of a bucket for tests.
// bucketRT is shared between app.buckets and the tenant indexes.
func (app *S3RP) SetBackend(bucket string, client BackendClient) {
	app.buckets[bucket].client = client
}

func NewChunkedReader(body io.Reader, vr *VerifiedRequest) io.Reader {
	return newChunkedReader(body, vr)
}
