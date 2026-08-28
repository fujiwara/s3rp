package s3gw

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"time"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/store"
)

// Breaker decides whether an attempt against one backend may proceed and
// learns from its outcome. The gateway calls Allow before every attempt the
// SDK makes (retries included) and Report after each attempt it admitted,
// with ok = the backend answered (any status below 500; a 404 proves the
// backend is up). It is never told about attempts the client abandoned
// (context.Canceled): those say nothing about the backend. Implementations
// must be safe for concurrent use.
type Breaker interface {
	Allow() bool
	Report(ok bool)
}

// SetBreaker installs the hook that picks the Breaker guarding each backend
// client the gateway builds; nil means no breaker for that backend. It is
// called once per built client — so per backend identity (endpoint,
// region, credentials, path style), the same key the client cache uses —
// and a client rebuilt after cache eviction asks again and starts with
// whatever it returns. Return a fresh breaker for per-client state, or a
// shared one to aggregate across clients (per endpoint, say): the keying is
// the service's decision, as with SetBandwidthLimit. Set it before serving;
// a cached client keeps the breaker it was built with.
//
// What the breaker buys is fail-fast: a request to a backend that has been
// failing is refused with ServiceUnavailable before the network, instead of
// each one paying the dial or read timeout. Retries are already bounded by
// the SDK's retry token bucket; the breaker bounds the first attempt.
func (g *Gateway) SetBreaker(f func(b *store.Backend) Breaker) { g.breaker = f }

// BreakerOpen is the cause behind a ServiceUnavailable the gateway answered
// without calling the backend: its breaker was open. Observer-only; the
// endpoint never reaches the client.
type BreakerOpen struct {
	Endpoint string `json:"endpoint"`
}

func (e *BreakerOpen) Error() string { return "backend circuit open: " + e.Endpoint }

// RetryableError tells the SDK's retryer not to retry a refused attempt:
// retrying would only ask the same open breaker again.
func (e *BreakerOpen) RetryableError() bool { return false }

// breakerMiddleware wraps the transport send of every attempt with the
// breaker's Allow/Report. It sits in the Deserialize step, which the SDK
// runs once per attempt inside its retry loop, so a retried failure reports
// each attempt and a refusal short-circuits before the network (after
// serialization and signing, which cost microseconds).
func breakerMiddleware(b *store.Backend, br Breaker) func(*middleware.Stack) error {
	return func(stack *middleware.Stack) error {
		return stack.Deserialize.Add(middleware.DeserializeMiddlewareFunc("CircuitBreaker",
			func(ctx context.Context, in middleware.DeserializeInput, next middleware.DeserializeHandler) (
				middleware.DeserializeOutput, middleware.Metadata, error,
			) {
				if !br.Allow() {
					return middleware.DeserializeOutput{}, middleware.Metadata{},
						s3err.New(http.StatusServiceUnavailable, "ServiceUnavailable", "Service is unable to handle request.").
							WithCause(&BreakerOpen{Endpoint: b.Endpoint})
				}
				out, md, err := next.HandleDeserialize(ctx, in)
				if ok, report := classifyAttempt(out.RawResponse, err); report {
					br.Report(ok)
				}
				return out, md, err
			}), middleware.Before)
	}
}

// classifyAttempt turns one attempt's outcome into what the breaker is
// told: ok when the backend answered with anything below 500, not ok on a
// 5xx, a transport failure or a timeout, and nothing at all when the
// client abandoned the request.
func classifyAttempt(raw any, err error) (ok, report bool) {
	if errors.Is(err, context.Canceled) {
		return false, false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return false, true
	}
	// a transport failure still comes with a raw response, an empty one
	// with status 0, so only a real status says the backend answered
	if resp, isHTTP := raw.(*smithyhttp.Response); isHTTP && resp != nil && resp.Response != nil && resp.StatusCode >= 100 {
		return resp.StatusCode < http.StatusInternalServerError, true
	}
	if err == nil {
		return true, true
	}
	// a transport failure carries a ResponseError with no status; anything
	// else that reached here without a response is a failure to reach the
	// backend as well
	if respErr, isResp := errors.AsType[*awshttp.ResponseError](err); isResp {
		if status := respErr.HTTPStatusCode(); status >= 100 {
			return status < http.StatusInternalServerError, true
		}
	}
	return false, true
}

// ConsecutiveFailures is a Breaker that opens after n consecutive failures
// and, once cooldown has passed, admits one probe per cooldown: a
// successful probe closes it, a failed one re-opens it. A probe that is
// never reported (the client abandoned it) is simply followed by the next
// probe a cooldown later, so the breaker cannot wedge half-open. State
// advances on Allow; there are no timers or goroutines.
type ConsecutiveFailures struct {
	n        int
	cooldown time.Duration
	now      func() time.Time

	mu        sync.Mutex
	state     breakerState
	failures  int       // consecutive failures while closed
	since     time.Time // when it opened
	lastProbe time.Time // the half-open probe most recently admitted
}

type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

func (s breakerState) String() string {
	switch s {
	case breakerOpen:
		return "open"
	case breakerHalfOpen:
		return "half-open"
	}
	return "closed"
}

// NewConsecutiveFailures returns a breaker that opens after n consecutive
// failures (n ≥ 1) and probes once per cooldown after that. Size n above
// the SDK's retry attempts (3 by default), or a single request's retries
// can open it.
func NewConsecutiveFailures(n int, cooldown time.Duration) *ConsecutiveFailures {
	if n < 1 {
		n = 1
	}
	return &ConsecutiveFailures{n: n, cooldown: cooldown, now: time.Now}
}

// Allow admits an attempt: always while closed, never while open and
// inside the cooldown, and once per cooldown after that (the probe).
func (b *ConsecutiveFailures) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerClosed:
		return true
	case breakerOpen:
		if b.now().Sub(b.since) < b.cooldown {
			return false
		}
		b.state = breakerHalfOpen
		b.lastProbe = b.now()
		return true
	default: // half-open: one probe per cooldown
		if b.now().Sub(b.lastProbe) < b.cooldown {
			return false
		}
		b.lastProbe = b.now()
		return true
	}
}

// Report records an admitted attempt's outcome.
func (b *ConsecutiveFailures) Report(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ok {
		b.state, b.failures = breakerClosed, 0
		return
	}
	switch b.state {
	case breakerClosed:
		b.failures++
		if b.failures >= b.n {
			b.open()
		}
	case breakerHalfOpen:
		b.open()
	}
}

func (b *ConsecutiveFailures) open() {
	b.state, b.failures, b.since = breakerOpen, 0, b.now()
}

// State reports "closed", "open" or "half-open", for a metric.
func (b *ConsecutiveFailures) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.String()
}
