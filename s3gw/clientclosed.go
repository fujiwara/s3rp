package s3gw

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/fujiwara/s3rp/s3err"
)

// StatusClientClosedRequest is the status recorded for a request the client
// abandoned before it was answered. It is nginx's 499, not an S3 status:
// nobody receives it, so it exists for the record alone.
const StatusClientClosedRequest = 499

// ErrClientClosed is in the cause of a request that failed because the
// client went away, so an observer can tell it from a failure of the
// service with errors.Is.
var ErrClientClosed = errors.New("client closed the request")

// clientClosed rewrites a failure caused by the client abandoning the
// request. Such a request fails in the backend call, with the inbound
// context's cancellation or with the broken read of its body, and those map
// to 504 and 502: a server failure in every log and metric for something
// the client did. net/http cancels the inbound context when the client's
// connection is gone; the failure must also stem from it (the cancellation
// in its chain, or bodyFailed), so a refusal the client was owed anyway
// keeps its own status.
func clientClosed(r *http.Request, s3e *s3err.Error, bodyFailed bool) *s3err.Error {
	if !errors.Is(r.Context().Err(), context.Canceled) {
		return s3e
	}
	if !bodyFailed && !errors.Is(s3e, context.Canceled) {
		return s3e
	}
	var cause error = s3e
	if c := errors.Unwrap(s3e); c != nil {
		cause = c
	}
	return s3err.New(StatusClientClosedRequest, "ClientClosedRequest",
		"The client closed the connection before the request completed.").
		WithCause(fmt.Errorf("%w: %w", ErrClientClosed, cause))
}
