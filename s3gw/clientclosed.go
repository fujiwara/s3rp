package s3gw

import (
	"context"
	"errors"
	"fmt"
	"io"
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

// sourceReader remembers whether a read failed, so that a failed io.Copy
// can be put down to its source or its destination.
type sourceReader struct {
	io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	if err != nil && err != io.EOF {
		s.err = err
	}
	return n, err
}

// bodyCopyError is the failure of a response body that stopped midway. The
// client has its status already, so only the observer sees this: without
// it a truncated download is recorded as a clean 200. A canceled inbound
// context means the client left — net/http cancels it when a write to the
// connection fails — and anything else is told apart by which side of the
// copy failed.
func bodyCopyError(r *http.Request, err error, readFailed bool) *s3err.Error {
	const msg = "We encountered an internal error. Please try again."
	switch {
	case errors.Is(r.Context().Err(), context.Canceled):
		return s3err.Internal(fmt.Errorf("%w: sending the object body: %w", ErrClientClosed, err), msg)
	case readFailed:
		return s3err.Internal(fmt.Errorf("reading the object body from the backend: %w", err), msg)
	default:
		return s3err.Internal(fmt.Errorf("sending the object body: %w", err), msg)
	}
}
