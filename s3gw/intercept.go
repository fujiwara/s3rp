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

	// BytesIn and BytesOut count the request and response bodies as seen on
	// the wire, so an upload sent with aws-chunked framing counts the frames
	// too. They are filled in by the time an Interceptor's next returns.
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`
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

// runOp applies the hooks around one operation.
func (g *Gateway) runOp(ctx context.Context, op *Op, c *opCtx, run func() error) error {
	if g.authorizer != nil {
		if err := g.authorizer.Authorize(ctx, op); err != nil {
			return err
		}
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

// countingBody counts the bytes read from a request body. Only the goroutine
// serving the request reads it, so a plain counter is enough.
type countingBody struct {
	io.ReadCloser
	n int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

var _ http.ResponseWriter = (*statusWriter)(nil)
