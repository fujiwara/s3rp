package s3gw

import (
	"context"
	"io"
	"net/http"
)

// Hooks a service installs to take part in each operation.
//
// The proxy knows how to speak S3 and how to evaluate bucket and user
// policies, but a real service also has to meter what it serves and refuse
// what policies cannot express — an exhausted quota, a suspended tenant, a
// rate limit. Those decisions do not belong in the S3 layer, so they are
// reached through these two seams instead of being wired into the operations.

// Op describes the operation a request is performing: what is being done, by
// whom, to what, and — once it has run — how much data moved.
type Op struct {
	// Method distinguishes operations that share an action, notably
	// HeadObject from GetObject.
	Method string `json:"method"`
	// Action is the s3:* action authorized for this operation. It is empty
	// for the few operations that authorize themselves per object, such as
	// DeleteObjects.
	Action string `json:"action,omitempty"`
	Tenant string `json:"tenant"`
	User   string `json:"user"`
	// Bucket is the front bucket name. The backend name it maps to is
	// deliberately not exposed: nothing outside the proxy should depend on it.
	Bucket string `json:"bucket"`
	Key    string `json:"key,omitempty"`
	// SSE is the server-side encryption the request asks for ("AES256" or
	// "aws:kms", empty when none), and SSEKMSKeyID the KMS key id it
	// names, when it does. The gateway forwards both to the backend
	// untouched; whether this identity may use this key — or whether this
	// bucket's backend supports encryption at all — is the service's
	// decision, made in an Authorizer. Nothing else in the request path
	// knows which tenant owns which key, and a backend that lacks SSE may
	// ignore the request silently rather than refuse it.
	SSE         string `json:"sse,omitempty"`
	SSEKMSKeyID string `json:"sse_kms_key_id,omitempty"`

	// BytesIn and BytesOut count the request and response bodies as seen on
	// the wire, so an upload sent with aws-chunked framing counts the frames
	// too. They are filled in by the time an Interceptor's next returns.
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`

	// BucketMetadata and KeyMetadata carry the store.Bucket.Metadata and
	// store.Key.Metadata the Store attached to the definitions this request
	// resolved, so hooks get the data the store already loaded — a quota, a
	// suspension flag — without a second lookup. The gateway passes them
	// through untouched. They are excluded from JSON on purpose: Op is
	// emitted as-is by observers, and whether this data belongs in a log is
	// the service's decision, not the gateway's.
	BucketMetadata any `json:"-"`
	KeyMetadata    any `json:"-"`
}

// Authorizer is consulted for every operation, after the bucket and user
// policies have allowed it. Returning an error refuses the request: an
// *s3err.Error decides what the client is told, anything else becomes a
// generic internal error. It runs after the policy evaluation because that is
// local and cheap, while this may not be.
type Authorizer interface {
	Authorize(ctx context.Context, op *Op) error
}

// Interceptor wraps the execution of an operation. Call next to run it;
// returning without calling it refuses the request. Metering belongs after
// next returns, when the byte counts on op have been filled in.
type Interceptor func(ctx context.Context, op *Op, next func() error) error

// SetAuthorizer installs the authorizer consulted for every operation.
func (g *Gateway) SetAuthorizer(a Authorizer) { g.authorizer = a }

// Use appends an interceptor. The first one added is the outermost.
func (g *Gateway) Use(i Interceptor) { g.interceptors = append(g.interceptors, i) }

// BandwidthLimiter paces one direction of a request's body. WaitN blocks
// until n bytes may pass, or fails: because ctx was canceled (the client is
// gone), or because n exceeds what the limiter can ever grant in one call.
// *golang.org/x/time/rate.Limiter satisfies this interface; its burst is that
// grant ceiling. The gateway asks in chunks of at most 32 KiB regardless of
// how large the reads and writes it paces are, so any burst of at least
// 32 KiB works; a practical bandwidth limiter wants a larger one (say 256 KiB
// to 1 MiB), since the burst is also how far a stream may run ahead of the
// configured rate.
type BandwidthLimiter interface {
	WaitN(ctx context.Context, n int) error
}

// waitChunk caps how many bytes one WaitN call asks for, so a consumer
// reading with a large buffer cannot exceed a reasonable limiter burst.
const waitChunk = 32 << 10

func waitBandwidth(l BandwidthLimiter, ctx context.Context, n int) error {
	for n > 0 {
		c := min(n, waitChunk)
		if err := l.WaitN(ctx, c); err != nil {
			return err
		}
		n -= c
	}
	return nil
}

// SetBandwidthLimit installs a hook that picks the bandwidth limiters for an
// operation, called once per request after the policies and the Authorizer
// have allowed it. in paces the request body as read off the wire (an
// aws-chunked upload is paced with its framing, matching Op.BytesIn), out
// paces the response body as written; nil means unlimited in that direction.
//
// The hook only selects limiters — sharing is what makes it a limit, and how
// they are shared is the service's decision: one limiter per tenant is that
// tenant's aggregate cap across all its concurrent requests, per user or per
// bucket likewise (key on Op.User rather than an access key: keys rotate, and
// during a rotation two keys are live, which would double a per-key budget).
// The gateway keeps no limiter state, so the service owns the map and its
// eviction, and can carry the configured rates on Bucket.Metadata or
// Key.Metadata to avoid a second store lookup.
func (g *Gateway) SetBandwidthLimit(f func(op *Op) (in, out BandwidthLimiter)) {
	g.bandwidthLimit = f
}

// runOp applies the hooks around one operation.
func (g *Gateway) runOp(ctx context.Context, op *Op, c *opCtx, run func() error) error {
	if g.authorizer != nil {
		if err := g.authorizer.Authorize(ctx, op); err != nil {
			return err
		}
	}
	if g.bandwidthLimit != nil {
		in, out := g.bandwidthLimit(op)
		c.setBandwidthLimiters(ctx, in, out)
	}
	next := func() error {
		err := run()
		// count after the handler has streamed its body, so an interceptor
		// sees the totals once next returns
		op.BytesIn, op.BytesOut = c.transferred()
		return err
	}
	for i := len(g.interceptors) - 1; i >= 0; i-- {
		next = wrapInterceptor(g.interceptors[i], ctx, op, next)
	}
	return next()
}

// wrapInterceptor exists so each iteration captures its own next.
func wrapInterceptor(i Interceptor, ctx context.Context, op *Op, next func() error) func() error {
	return func() error { return i(ctx, op, next) }
}

// transferred reports the bytes moved by this request. The counting wrappers
// are installed by wrapHandler, so a handler invoked outside it simply counts
// nothing rather than failing.
func (c *opCtx) transferred() (in, out int64) {
	if sw, ok := c.w.(*statusWriter); ok {
		out = sw.written
	}
	if cb, ok := c.r.Body.(*countingBody); ok {
		in = cb.n
	}
	return in, out
}

// setBandwidthLimiters arms the counting wrappers installed by wrapHandler
// with the hook's limiters. Like transferred, a handler invoked outside
// wrapHandler has no wrappers and simply goes unpaced rather than failing.
// Only the goroutine serving the request touches the wrappers, so plain
// assignment is enough.
func (c *opCtx) setBandwidthLimiters(ctx context.Context, in, out BandwidthLimiter) {
	if in != nil {
		if cb, ok := c.r.Body.(*countingBody); ok {
			cb.limiter, cb.ctx = in, ctx
		}
	}
	if out != nil {
		if sw, ok := c.w.(*statusWriter); ok {
			sw.limiter, sw.ctx = out, ctx
		}
	}
}

// countingBody counts the bytes read from a request body. Only the goroutine
// serving the request reads it, so a plain counter is enough. When a
// bandwidth limiter is armed it also paces the reads: read first, then wait
// for what was read — the consumer does not get the next chunk until the
// limiter releases this one, which is what backpressures the client's upload.
type countingBody struct {
	io.ReadCloser
	n       int64
	limiter BandwidthLimiter
	ctx     context.Context // the request's context, set with limiter
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	if n > 0 && b.limiter != nil {
		// a pacing failure outranks even io.EOF: the bytes were already
		// consumed, but the request must abort rather than pass unpaced
		if werr := waitBandwidth(b.limiter, b.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

var _ http.ResponseWriter = (*statusWriter)(nil)
