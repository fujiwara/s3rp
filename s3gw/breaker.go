package s3gw

import (
	"context"
	"errors"
	"net/http"
	"net/url"
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
// backend never reaches the client.
type BreakerOpen struct {
	// Backend names the backend as backendName does: scheme and host of
	// its endpoint, or "aws:<region>" for Amazon S3 — never a path, query
	// or userinfo an endpoint URL might carry.
	Backend string `json:"backend"`
}

// backendName is how a backend is named in an observer cause: enough to
// tell backends apart, nothing an endpoint URL could smuggle into a log.
func backendName(b *store.Backend) string {
	if b.Endpoint == "" {
		return "aws:" + b.Region
	}
	u, err := url.Parse(b.Endpoint)
	if err != nil || u.Host == "" {
		return "backend:" + b.Region
	}
	return u.Scheme + "://" + u.Host
}

func (e *BreakerOpen) Error() string { return "backend circuit open: " + e.Backend }

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
							WithCause(&BreakerOpen{Backend: backendName(b)})
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
//
// What it does in each state is breakerTransitions, and nothing else: Allow
// and Report only name the event and carry out the step the table gives.
type ConsecutiveFailures struct {
	n        int
	cooldown time.Duration
	now      func() time.Time

	mu       sync.Mutex
	state    breakerState
	failures int // consecutive failures while closed
	// mark is what the cooldown is measured from: when it opened, or the
	// half-open probe most recently admitted
	mark time.Time
}

type breakerState int

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
	numBreakerStates
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

// breakerEvent is what can happen to a breaker: one of the two calls, with
// the fact the call's outcome depends on.
type breakerEvent int

const (
	// Allow, inside the cooldown since mark and after it
	breakerAllowEarly breakerEvent = iota
	breakerAllowDue
	// Report(true)
	breakerSuccess
	// Report(false), and the one that makes it n in a row
	breakerFailure
	breakerFailureTripping
	numBreakerEvents
)

func (e breakerEvent) String() string {
	return [...]string{"allow-early", "allow-due", "success", "failure", "failure-tripping"}[e]
}

// breakerStep is what an event does in a state.
type breakerStep struct {
	to breakerState
	// admit is Allow's answer; false for a Report
	admit bool
	// count adds to the consecutive failures; any other step that changes
	// the state, and every success, resets them
	count bool
	// stamp sets mark to now: the breaker opened, or a probe went out
	stamp bool
}

// breakerTransitions is the breaker: every state, every event. It is held
// to tla/Breaker.tla, which is checked for what the breaker promises —
// nothing admitted inside a cooldown but the one probe, and no state it
// cannot leave (TestBreakerTable).
var breakerTransitions = [numBreakerStates][numBreakerEvents]breakerStep{
	breakerClosed: {
		breakerAllowEarly:      {to: breakerClosed, admit: true},
		breakerAllowDue:        {to: breakerClosed, admit: true},
		breakerSuccess:         {to: breakerClosed},
		breakerFailure:         {to: breakerClosed, count: true},
		breakerFailureTripping: {to: breakerOpen, stamp: true},
	},
	breakerOpen: {
		breakerAllowEarly: {to: breakerOpen},
		// the probe
		breakerAllowDue: {to: breakerHalfOpen, admit: true, stamp: true},
		// an attempt admitted before it opened: the backend answered
		breakerSuccess: {to: breakerClosed},
		// and one that failed like those that opened it, which is not news
		breakerFailure:         {to: breakerOpen},
		breakerFailureTripping: {to: breakerOpen},
	},
	breakerHalfOpen: {
		breakerAllowEarly: {to: breakerHalfOpen},
		// the probe was not reported: the next one
		breakerAllowDue:        {to: breakerHalfOpen, admit: true, stamp: true},
		breakerSuccess:         {to: breakerClosed},
		breakerFailure:         {to: breakerOpen, stamp: true},
		breakerFailureTripping: {to: breakerOpen, stamp: true},
	},
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
	if b.state == breakerClosed {
		// the cooldown means nothing here, so the clock is not read
		return b.step(breakerAllowEarly, time.Time{})
	}
	now := b.now()
	if now.Sub(b.mark) < b.cooldown {
		return b.step(breakerAllowEarly, now)
	}
	return b.step(breakerAllowDue, now)
}

// Report records an admitted attempt's outcome.
func (b *ConsecutiveFailures) Report(ok bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch {
	case ok:
		b.step(breakerSuccess, time.Time{})
	case b.failures+1 >= b.n:
		b.step(breakerFailureTripping, b.now())
	default:
		b.step(breakerFailure, b.now())
	}
}

// step carries out what the table gives for e in the current state. now may
// be the zero time for an event that stamps in no state.
func (b *ConsecutiveFailures) step(e breakerEvent, now time.Time) bool {
	s := breakerTransitions[b.state][e]
	switch {
	case s.count:
		b.failures++
	case s.to != b.state || e == breakerSuccess:
		b.failures = 0
	}
	if s.stamp {
		b.mark = now
	}
	b.state = s.to
	return s.admit
}

// State reports "closed", "open" or "half-open", for a metric.
func (b *ConsecutiveFailures) State() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state.String()
}
