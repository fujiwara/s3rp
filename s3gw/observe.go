package s3gw

import (
	"context"
	"time"
)

// RequestInfo describes a finished request. The gateway does not log: it
// reports what it knows here and leaves the format, the level and the
// destination to the service, which is the one that has to live with them.
//
// Everything a failure can be explained by is present, so an observer is
// enough to write both an access log and a failure log.
type RequestInfo struct {
	Method     string
	Path       string
	RemoteAddr string
	// RawQuery has the presigned authentication parameters masked. Log this
	// rather than the request's own query string: a presigned URL's signature
	// is a bearer credential until it expires, and recognizing one is this
	// package's job, not the caller's.
	RawQuery string
	// RequestID is the value returned to the client in x-amz-request-id, so a
	// request a user reports can be found in the log.
	RequestID string
	Status    int
	// Code is the S3 error code the client was given, empty on success.
	Code string
	// Err is what actually went wrong, when anything did. It is never sent to
	// the client — it may name backend endpoints and buckets — so this is the
	// only place it can be recorded.
	Err error
	// BytesIn and BytesOut count the request and response bodies as seen on
	// the wire, for the whole request including failures.
	BytesIn  int64
	BytesOut int64
	// Start is when the request began, and with Duration brackets it. It is
	// here so the record stands on its own: an observer that hands it to
	// something else — a metering queue, a batch — must not have to stamp the
	// time itself at the moment it is called.
	Start    time.Time
	Duration time.Duration
}

// Observer is called once per request, after the response has been written.
type Observer func(ctx context.Context, info *RequestInfo)

// SetObserver installs the observer called at the end of every request.
// Without one the gateway is silent, so a service that installs nothing gets
// no log at all — including for failures, whose cause is not recoverable
// anywhere else.
func (g *Gateway) SetObserver(o Observer) { g.observer = o }
