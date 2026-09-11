package s3gw

import (
	"context"
	"io"
	"net/http"
	"slices"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
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
//
// The fields fall into three groups by the moment they are known: the
// identity of the operation, which is settled before any hook runs; Request,
// what the client asked for, also settled before the hooks but never more
// than a claim; and Response, what the backend reported, which does not
// exist until the operation has run. Request and Response are pointers so
// that "not asked for" and "asked for nothing" are the same nil, and so an
// Authorizer cannot mistake an unfilled Response for an empty one.
// OpUnknown is the Op.Operation of a request that matched no known S3
// operation.
const OpUnknown = "Unknown"

type Op struct {
	// Method distinguishes operations that share an action, notably
	// HeadObject from GetObject.
	Method string `json:"method"`
	// Operation is the S3 API operation name (PutObject, DeleteObjects,
	// DeleteBucket, ...). Unlike Action it is set for operations the
	// gateway refuses outright, so a service can count which unsupported
	// operations its users attempt; a request matching no known operation
	// is OpUnknown.
	Operation string `json:"operation"`
	// Actions lists the s3:* actions the request was authorized for before
	// the hooks ran, the operation's own first. A header adds one, as on
	// Amazon S3: x-amz-tagging on an upload needs s3:PutObjectTagging, the
	// x-amz-object-lock-* headers s3:PutObjectRetention /
	// s3:PutObjectLegalHold, x-amz-bypass-governance-retention
	// s3:BypassGovernanceRetention; a copy reads its source under
	// s3:GetObject. This is how a service refuses tags or a lock on an
	// upload — a policy Deny on the action, or an Authorizer matching it
	// here — without the values being reported on Request: what S3 models
	// as a permission stays a permission. Empty for the few operations that
	// authorize themselves per object, such as DeleteObjects.
	Actions []string `json:"actions,omitempty"`
	Tenant  string   `json:"tenant"`
	User    string   `json:"user"`
	// Bucket is the front bucket name. The backend name it maps to is
	// deliberately not exposed: nothing outside the proxy should depend on it.
	Bucket string `json:"bucket"`
	Key    string `json:"key,omitempty"`

	// Request is what the client asked for about the object itself, set
	// before the Authorizer runs so it can refuse on it. Nil when the
	// request asked for none of it — and nil as well for an operation that
	// carries no object attributes at all, so a header an operation would
	// ignore is never reported as something the client asked for.
	Request *OpRequest `json:"request,omitempty"`
	// Response is what the backend reported about the object. It is nil
	// until the operation has run — always nil in an Authorizer, and still
	// nil for an operation that failed — and an Interceptor sees it once
	// next returns, as with the byte counts. Operations that report none of
	// it (UploadPart, DeleteObject, the listings) leave it nil.
	Response *OpResponse `json:"response,omitempty"`

	// BytesIn and BytesOut count the request and response bodies as seen on
	// the wire, so an upload sent with aws-chunked framing counts the frames
	// too. They are filled in by the time an Interceptor's next returns.
	BytesIn  int64 `json:"bytes_in"`
	BytesOut int64 `json:"bytes_out"`

	// Denials summarizes the per-key refusals of an operation that
	// authorizes per object (DeleteObjects) and therefore succeeds as a
	// whole: one entry per deciding statement with the number of keys it
	// refused. Nil when every key went through. Set by the time next
	// returns, like Response. A refusal of the whole request is not here —
	// it is the *DenyReason cause of the AccessDenied the observer gets.
	Denials []Denial `json:"denials,omitempty"`

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

// OpRequest is what a request asked for about the object it writes or reads.
// Every value here is the client's, and beyond what the gateway validates
// itself (dispatch checks the SSE fields) none of it is a statement about
// what the backend did — that is Response. It carries only attributes S3
// has no action for; tags and Object Lock on an upload are permissions
// there and are authorized as such (Op.Actions), which keeps this from
// growing a field per header.
type OpRequest struct {
	// SSE is the server-side encryption the request asks for ("AES256" or
	// "aws:kms", empty when none), and SSEKMSKeyID the KMS key id it
	// names, when it does. The gateway forwards both to the backend
	// untouched; whether this identity may use this key — or whether this
	// bucket's backend supports encryption at all — is the service's
	// decision, made in an Authorizer. Nothing else in the request path
	// knows which tenant owns which key, and a backend that lacks SSE may
	// ignore the request silently rather than refuse it, which is what
	// comparing this with Response.SSE reveals.
	SSE         string `json:"sse,omitempty"`
	SSEKMSKeyID string `json:"sse_kms_key_id,omitempty"`
	// StorageClass is the class the request names. Backends disagree on an
	// undefined one — Ceph RGW refuses the write with InvalidArgument,
	// versitygw stores the object as STANDARD without a word — and the S3
	// API returns no class on a write, so this is what was asked for and
	// there is no gateway-side way to learn what it became. An Authorizer
	// is the place to refuse a class this tenant may not use, and with a
	// StorageClassMapper installed this is only ever a request: the mapper
	// decides what the backend is told.
	StorageClass string `json:"storage_class,omitempty"`
	// Metadata is the user metadata the request carries (the x-amz-meta-*
	// names, prefix removed), which is where a service's own per-object
	// attribute travels. Only writes carry it, and a copy that keeps the
	// source's metadata carries none of its own.
	//
	// It is nil when the request carried none, including when the rest of
	// OpRequest is filled: a non-nil OpRequest means at least one of its
	// fields is set, not all of them. Reading a nil map is safe; a hook
	// must not write to it.
	//
	// Excluded from JSON for the same reason as the store metadata on Op:
	// it is client-supplied, and whether it belongs in a log is the
	// service's decision.
	Metadata map[string]string `json:"-"`
}

// empty reports whether the request asked for nothing about the object, in
// which case Op.Request stays nil rather than pointing at a blank struct.
func (r *OpRequest) empty() bool {
	return r.SSE == "" && r.SSEKMSKeyID == "" && r.StorageClass == "" && len(r.Metadata) == 0
}

// OpResponse is what the backend reported about the object, which is the
// only side of the operation that is a fact rather than a request.
type OpResponse struct {
	// SSE and SSEKMSKeyID are the encryption the backend says it applied.
	// A write that asked for SSE and comes back without it was served by a
	// backend that ignored the request rather than refusing it.
	SSE         string `json:"sse,omitempty"`
	SSEKMSKeyID string `json:"sse_kms_key_id,omitempty"`
	// StorageClass is the class the object is actually in, so it accounts
	// for a lifecycle transition the gateway never saw and is what storage
	// or retrieval billing must be based on. Reads only — no S3 write
	// returns a storage class. Empty means the backend named none, which
	// is what S3 does for an object left in the default class; it does not
	// mean the class is unknown. This is the backend's own name for it,
	// before a StorageClassMapper decides what the client is shown.
	StorageClass string `json:"storage_class,omitempty"`
	// Metadata is the object's stored user metadata (prefix removed), on
	// reads only, and nil when the object has none — same rule as the
	// request's, down to it being nil while the rest of OpResponse is
	// filled. Excluded from JSON like the request's, and for the same
	// reason.
	Metadata map[string]string `json:"-"`
	// ETag and VersionID identify the object version this operation read or
	// created, for a service that has to be able to say afterwards which
	// one it served.
	ETag      string `json:"etag,omitempty"`
	VersionID string `json:"version_id,omitempty"`
}

// empty reports whether the backend said nothing worth recording, in which
// case Op.Response stays nil rather than pointing at a blank struct.
func (r *OpResponse) empty() bool {
	return r.SSE == "" && r.SSEKMSKeyID == "" && r.StorageClass == "" &&
		r.ETag == "" && r.VersionID == "" && len(r.Metadata) == 0
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

// BandwidthLimiter paces one direction of a request's body.
// *golang.org/x/time/rate.Limiter satisfies it; the gateway calls WaitN in
// chunks of at most waitChunk, so any burst of at least 32 KiB works.
type BandwidthLimiter interface {
	WaitN(ctx context.Context, n int) error
}

// waitChunk caps one WaitN request: read and write sizes are
// consumer-controlled and must not exceed a limiter's burst.
const waitChunk = 32 * kib

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

// SetBandwidthLimit installs a hook that picks the limiters pacing an
// operation's request and response bodies — the same wire bytes
// Op.BytesIn/BytesOut count; nil means unlimited. The gateway keeps no
// limiter state: sharing one limiter across requests is what makes it an
// aggregate cap, and the keying is the service's decision (see
// docs/building-a-service.md).
func (g *Gateway) SetBandwidthLimit(f func(op *Op) (in, out BandwidthLimiter)) {
	g.bandwidthLimit = f
}

// SizeUnknown is the size handed to a backend storage class hook when the
// operation's final object size is not known at the time the class must be
// chosen: CreateMultipartUpload (the class is fixed before any part is
// uploaded), CopyObject (the source's size would cost a HeadObject) and a
// POST upload (the file part is read after the class is decided).
const SizeUnknown int64 = -1

// StorageClassMapper puts the service between the storage class a client
// sees and the one the backend uses, in both directions at once. It exists
// because a client naming the class would name the backend's classes —
// physical pools, tiers — and expose how the backend is laid out; with a
// mapper installed, which class a write lands in is a rule of the service
// (size, tenant plan, bucket) and what a read reports is the service's
// vocabulary. The two directions are one interface on purpose: choosing
// backend classes on writes while reporting them as they are on reads would
// show clients class names they never asked for and S3 does not define, so
// a mapper that means to pass one direction through says so explicitly:
// ToClient returning backendClass, and ToBackend returning the requested
// class — guarding the pointer, since Op.Request is nil for a write that
// asked for nothing:
//
//	if op.Request == nil {
//		return ""
//	}
//	return op.Request.StorageClass
//
// Without a mapper both directions pass through unchanged.
type StorageClassMapper interface {
	// ToBackend returns the class every write sends the backend —
	// PutObject, POST upload, CopyObject and CreateMultipartUpload — in
	// place of whatever the client asked for. It is called after the
	// policies, the Authorizer and the interceptors have admitted the
	// operation, with the op and the object's size in bytes (SizeUnknown
	// when the operation does not know it yet). "" sends none, which is the
	// backend's default class. The client's x-amz-storage-class never
	// reaches the backend on its own: it stays on Op.Request.StorageClass
	// for the Authorizer to refuse, or for this method to return when it
	// means to honor it.
	ToBackend(op *Op, size int64) string
	// ToClient returns what the client is shown wherever the S3 API
	// reports a class: the x-amz-storage-class header of
	// GetObject/HeadObject, and the StorageClass element of ListObjects,
	// ListObjectVersions, GetObjectAttributes, ListMultipartUploads and
	// ListParts. It is called once per reported value (per object in a
	// listing, so keep it cheap) with the class the backend named — ""
	// included, which is what S3 says for the default class. "" omits the
	// header or element. Op.Response.StorageClass carries the backend's own
	// value, before this method, so billing sees the real class while the
	// client sees the front's.
	ToClient(op *Op, backendClass string) string
}

// SetStorageClassMapper installs the storage class mapping. nil (the
// default) forwards the client's class to the backend and the backend's
// class to the client as they are.
func (g *Gateway) SetStorageClassMapper(m StorageClassMapper) { g.storageClass = m }

// backendStorageClass is what a write sends the backend: the mapper's
// choice when one is installed, the client's signed request otherwise.
func (c *opCtx) backendStorageClass(requested string, size int64) types.StorageClass {
	if c.g.storageClass != nil {
		return types.StorageClass(c.g.storageClass.ToBackend(c.op, size))
	}
	return types.StorageClass(requested)
}

// clientStorageClass is what the client is shown for a class the backend
// reported.
func (c *opCtx) clientStorageClass(backendClass string) string {
	if c.g.storageClass != nil {
		return c.g.storageClass.ToClient(c.op, backendClass)
	}
	return backendClass
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
	for _, v := range slices.Backward(g.interceptors) {
		next = wrapInterceptor(v, ctx, op, next)
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

// setBandwidthLimiters arms the wrappers installed by wrapHandler; outside
// it there are none and, like transferred, this is deliberately a no-op.
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
// serving the request reads it, so a plain counter is enough.
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
		// a pacing failure outranks io.EOF: the request must abort rather
		// than pass the already-consumed bytes unpaced
		if werr := waitBandwidth(b.limiter, b.ctx, n); werr != nil {
			return n, werr
		}
	}
	return n, err
}

var _ http.ResponseWriter = (*statusWriter)(nil)
