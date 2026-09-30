package s3gw

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"runtime/debug"
	"slices"
	"strings"
	"time"

	"github.com/fujiwara/s3rp/cors"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3op"
	"github.com/fujiwara/s3rp/store"
)

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// redactQuery renders the query string for logging with credential-bearing
// presigned parameters masked. A presigned URL's signature (and session
// token) are bearer credentials until expiry, so they must not land in
// request logs.
func redactQuery(q url.Values) string {
	for k := range q {
		switch strings.ToLower(k) {
		case "x-amz-signature", "x-amz-security-token":
			q[k] = []string{"REDACTED"}
		}
	}
	return q.Encode()
}

// Handler returns the http.Handler of the proxy.
//
// A single catch-all route is used instead of ServeMux patterns because the
// mux cleans paths (collapsing // and resolving dot segments) and redirects,
// which breaks S3 keys and signature verification.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", g.wrapHandler(g.handleRequest))
	return mux
}

type statusWriter struct {
	http.ResponseWriter
	status int
	// started records that the response has begun, so a failure discovered
	// afterwards knows an error document can no longer replace it.
	started bool
	written int64
	limiter BandwidthLimiter
	ctx     context.Context // the request's context, set with limiter
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.started = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	w.started = true // an implicit 200 is a started response too
	if w.limiter == nil {
		n, err := w.ResponseWriter.Write(p)
		w.written += int64(n)
		return n, err
	}
	// wait before each slice, not once for all of p: a single Write can
	// carry a whole WriterTo source, which must neither burst out after
	// one big wait nor be sent at all when pacing fails
	var total int
	for total < len(p) {
		end := min(total+waitChunk, len(p))
		if err := w.limiter.WaitN(w.ctx, end-total); err != nil {
			return total, err
		}
		n, err := w.ResponseWriter.Write(p[total:end])
		w.written += int64(n)
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func (g *Gateway) wrapHandler(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := g.newRequestID(r)
		w.Header().Set("x-amz-request-id", requestID)
		// The record is assembled as the request goes: the identity is known
		// once the signature verifies, the operation only after routing and
		// the policies pass. Without an observer none of that is collected.
		var info *RequestInfo
		if g.observer != nil {
			info = &RequestInfo{
				Method:     r.Method,
				Path:       r.URL.Path,
				RemoteAddr: r.RemoteAddr,
				RequestID:  requestID,
				Start:      start,
			}
			r = r.WithContext(context.WithValue(r.Context(), infoKey{}, info))
		}
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		body := &countingBody{}
		if r.Body != nil {
			body.ReadCloser = r.Body
			r.Body = body
		}

		// report answers the client and hands the record to the observer,
		// exactly once per request whether the handler returned or panicked.
		reported := false
		report := func(err error) {
			if reported {
				return
			}
			reported = true
			var code string
			var cause error
			if err != nil {
				var s3e *s3err.Error
				if !errors.As(err, &s3e) {
					// an error that is not an S3 error at all: the operation
					// layer should not produce these, so the error itself is the
					// only description we have
					s3e = s3err.Internal(err, "We encountered an internal error. Please try again.")
				}
				// the cause never reaches the client, so handing it to the
				// observer is the only way it can be seen
				cause = errors.Unwrap(s3e)
				// a response already under way cannot be replaced by an error
				// document: appending one would leave the client a body that
				// parses as neither. Code stays empty then because it records
				// what the client was told, and it was told nothing.
				if !sw.started {
					code = s3e.Code
					s3err.Write(sw, r, s3e, requestID)
				}
			}
			if info == nil {
				return
			}
			info.RawQuery = redactQuery(r.URL.Query())
			info.Status = sw.status
			info.Code = code
			info.Err = cause
			info.Duration = time.Since(start)
			info.BytesIn, info.BytesOut = body.n, sw.written
			// the observer is the channel failures are reported through, so
			// when it is the thing that failed there is nowhere left to
			// report it but here — and its panic must not cost the client
			// the response that is already written
			defer func() {
				if p := recover(); p != nil {
					slog.WarnContext(r.Context(), "observer panicked",
						"request_id", requestID, "panic", p)
				}
			}()
			g.observer(r.Context(), info)
		}

		defer func() {
			p := recover()
			if p == nil {
				return
			}
			if p == http.ErrAbortHandler {
				// net/http's own signal for a deliberate silent abort: pass
				// it on untouched, including the silence
				panic(p)
			}
			// net/http would recover this too, but by dropping the
			// connection: the client gets no response at all (not even one
			// already written, which is still buffered), the panic goes to
			// its ErrorLog instead of the observer, and the client's retry
			// runs the whole operation again. Answer and record it here
			// instead.
			started := sw.started
			report(&PanicError{Value: p, Stack: debug.Stack()})
			if started {
				// there is a half-sent response the client must not read as
				// a complete one, and no way to take it back
				panic(http.ErrAbortHandler)
			}
		}()
		report(h(sw, r))
	}
}

// splitPath splits an escaped URL path into the bucket name and the
// (unescaped) object key.
func splitPath(escapedPath string) (bucket, key string, err error) {
	p := strings.TrimPrefix(escapedPath, "/")
	rawBucket, rawKey, _ := strings.Cut(p, "/")
	if bucket, err = url.PathUnescape(rawBucket); err != nil {
		return "", "", err
	}
	if key, err = unescapeKey(rawKey); err != nil {
		return "", "", err
	}
	return bucket, key, nil
}

// unescapeKey unescapes each segment of an object key, preserving
// slashes between segments (so that %2F inside a segment survives).
func unescapeKey(rawKey string) (string, error) {
	segments := strings.Split(rawKey, "/")
	for i, s := range segments {
		u, err := url.PathUnescape(s)
		if err != nil {
			return "", err
		}
		segments[i] = u
	}
	return strings.Join(segments, "/"), nil
}

func (g *Gateway) handleRequest(w http.ResponseWriter, r *http.Request) error {
	// browsers send CORS preflights without authentication
	if r.Method == http.MethodOptions {
		return g.handlePreflight(w, r)
	}
	// browser-based POST uploads carry their authentication in the form
	// fields, not in a header or the query
	if r.Method == http.MethodPost && isMultipartForm(r) {
		t, err := g.requestTarget(r)
		if err != nil {
			return s3err.New(http.StatusBadRequest, "InvalidURI", "Couldn't parse the specified URI.").WithCause(err)
		}
		if t.bucket != "" && t.key == "" {
			return g.handlePostObject(w, r, t)
		}
		// a multipart POST to any other path is not a browser upload; let
		// the normal flow reject it
	}
	vr, s3e := g.verifyRequest(r)
	if s3e != nil {
		recordPresentedKey(r.Context(), s3e)
		return s3e
	}
	if info := recordOf(r.Context()); info != nil {
		info.Tenant, info.User = vr.Tenant, vr.User
		info.AccessKeyID = vr.AccessKeyID
	}
	// a header no operation reads is refused, not ignored (headers.go);
	// here, so ListBuckets is covered along with every dispatched operation
	if s3e := newSignedHeader(r, vr.SignedHeaders).checkKnownAmzHeaders(g.checksums); s3e != nil {
		return s3e
	}
	t, err := g.requestTarget(r)
	if err != nil {
		return s3err.New(http.StatusBadRequest, "InvalidURI", "Couldn't parse the specified URI.").WithCause(err)
	}
	bucket, key := t.bucket, t.key

	if bucket == "" {
		if r.Method != http.MethodGet {
			return s3err.New(http.StatusMethodNotAllowed, "MethodNotAllowed",
				"The specified method is not allowed against this resource.")
		}
		// same discipline as every operation: unknown query parameters are
		// refused loudly rather than silently ignored
		if s3e := listBucketsParams.check(r.URL.Query()); s3e != nil {
			return s3e
		}
		return g.listBuckets(w, r, vr)
	}

	b, s3e := g.resolveBucket(r.Context(), vr, bucket)
	if s3e != nil {
		return s3e
	}
	client, err := g.backendClient(r.Context(), b.Backend)
	if err != nil {
		return s3err.Internal(err, "backend client failed")
	}
	rt := &bucketRT{cfg: b, client: client, target: t}
	cors.SetHeaders(w, r, b.CORS)

	query := r.URL.Query()
	if key == "" {
		return g.handleBucketRequest(w, r, rt, vr, query)
	}
	return g.handleObjectRequest(w, r, rt, vr, query, key)
}

// resolveBucket resolves the front bucket a verified request targets: the
// requester's own bucket, or another tenant's whose policy explicitly names
// the requester's qualified principal (cross-tenant access). Every other
// case — no such bucket anywhere, or a policy that never mentions the
// requester — is the same AccessDenied, so bucket names cannot be probed
// across tenants; the gate also keeps requesters the policy never mentions
// from reaching the foreign bucket's backend client and CORS headers. It is
// a visibility gate only: whether a mentioned principal may perform the
// specific operation is authorize's job.
func (g *Gateway) resolveBucket(ctx context.Context, vr *verifiedRequest, bucket string) (*store.Bucket, *s3err.Error) {
	b, err := g.store.GetBucket(ctx, vr.Tenant, bucket)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, s3err.Internal(err, "bucket lookup failed")
	}
	b, err = g.store.GetBucketByName(ctx, bucket)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// the cause distinguishes "no such bucket anywhere" from the
			// policy-silent denial below for the observer; the client gets
			// the same AccessDenied either way (no cross-tenant probing)
			return nil, s3err.AccessDenied().WithCause(err)
		}
		return nil, s3err.Internal(err, "bucket lookup failed")
	}
	if b.Policy == nil || !b.Policy.MentionsPrincipal(vr.principal()) {
		return nil, s3err.AccessDenied().WithCause(visibilityReason(vr.principal(), bucket))
	}
	return b, nil
}

// opCtx carries everything a routed operation needs. key is "" for
// bucket-level operations.
type opCtx struct {
	g     *Gateway
	w     http.ResponseWriter
	r     *http.Request
	rt    *bucketRT
	vr    *verifiedRequest
	hdr   signedHeader
	query url.Values
	key   string
	// copySource is the backend copy source of a copy variant, resolved and
	// authorized by dispatch before the hooks run.
	copySource string
	// op is the operation the hooks were given, set by dispatch. Handlers
	// record the backend's answer on it through setResponse.
	op *Op
}

// signed and attr forward to the request's signedHeader (headers.go), so
// handler bodies read c.signed("x-amz-...") / c.attr("Content-Type") without
// aliasing the accessor. c.signed is also the getter checkSSE/applySSE take.
func (c *opCtx) signed(name string) string { return c.hdr.Signed(name) }
func (c *opCtx) attr(name string) string   { return c.hdr.Attribute(name) }

// objectRequest collects what the client asked for about the object itself,
// or nil when it asked for none of it: on Op that nil is what tells a hook
// "nothing was requested" apart from "requested empty".
//
// withAttrs is the route's own answer to whether this operation carries
// object attributes at all. Only those routes are read, so a read does not
// pay for scanning the headers, and — the reason this covers the SSE fields
// too — an operation that would ignore the header does not report it as a
// request the client made. The gateway still validates the SSE fields on
// every route (dispatch), it just does not hand an inert header to a hook.
func (c *opCtx) objectRequest(withAttrs bool) *OpRequest {
	if !withAttrs {
		return nil
	}
	req := OpRequest{
		SSE:          c.signed(hdrSSE),
		SSEKMSKeyID:  c.signed(hdrSSEKMSKeyID),
		StorageClass: c.signed(hdrStorageClass),
		Metadata:     c.hdr.AmzMeta(),
	}
	if req.empty() {
		return nil
	}
	return &req
}

// setResponse records what the backend reported about the object, for the
// hooks that run after next and for the observer. Like transferred, it is a
// no-op for a handler invoked outside dispatch, and it leaves Op.Response nil
// when the backend reported nothing.
func (c *opCtx) setResponse(res OpResponse) {
	if c.op == nil || res.empty() {
		return
	}
	// an empty map and no map at all mean the same thing here, and the
	// documented rule is that absent metadata is nil — the SDK allocates
	// only when a header is present, but that is its behavior, not a promise
	if len(res.Metadata) == 0 {
		res.Metadata = nil
	}
	c.op.Response = &res
}

// authorize evaluates the bucket policy for the operation's resource
// (the bucket, or bucket/key for object operations).
func (c *opCtx) authorize(action string) *s3err.Error {
	resource := c.rt.cfg.Name
	if c.key != "" {
		resource += "/" + c.key
	}
	return c.g.authorize(c.vr, c.rt.cfg, action, resource)
}

// headerAction is an action a request must additionally be authorized for
// when it carries a header, as on Amazon S3: writing tags with an upload
// needs s3:PutObjectTagging, locking the object s3:PutObjectRetention /
// s3:PutObjectLegalHold, bypassing governance mode
// s3:BypassGovernanceRetention. What S3 models as a permission stays a
// permission here — authorized at the same chokepoint and listed on
// Op.Actions, so a policy Deny or an Authorizer refuses it by name — rather
// than becoming a value on OpRequest, which carries only what S3 has no
// action for.
type headerAction struct {
	present func(signedHeader) bool
	action  string
}

func signedPresent(name string) func(signedHeader) bool {
	return func(h signedHeader) bool { return h.Signed(name) != "" }
}

// headerPresence decides when a header-conditional authorization of the
// s3op catalog applies; every header the catalog names must be listed.
var headerPresence = map[string]func(signedHeader) bool{
	hdrTagging:                   signedPresent(hdrTagging),
	hdrObjectLockMode:            signedPresent(hdrObjectLockMode),
	hdrObjectLockRetainUntil:     signedPresent(hdrObjectLockRetainUntil),
	hdrObjectLockLegalHold:       signedPresent(hdrObjectLockLegalHold),
	hdrBypassGovernanceRetention: bypassGovernanceRetention,
}

// route selects one operation by a query discriminator and describes the
// checks to run before its handler. handle is a method value of *Gateway, so
// the route table reads as a plain "discriminator -> handler" mapping.
type route struct {
	match  func(url.Values) bool // selects this route (nil = always, the fallback)
	params paramSet              // allowed query parameters (nil = skip the check)
	aclHdr bool                  // reject unsupported canned ACL headers first
	attrs  bool                  // the operation carries the object's own attributes (SSE, storage class, user metadata)
	name   string                // S3 operation name recorded on Op, its s3op catalog entry
	copy   string                // operation name when x-amz-copy-source is present (PutObject → CopyObject)
	handle func(*Gateway, *opCtx) error

	// bound from the s3op catalog entry of name by bind
	action     string         // s3:* action to authorize ("" = handler authorizes itself)
	hdrActions []headerAction // actions a header adds to the authorization
}

// bindRoutes fills each route's authorization from the s3op catalog, so the
// actions an operation authorizes are written in one place
// (s3op/operations.json). An inconsistency panics at package init: every
// test run would fail on it, so it never reaches a deployment.
func bindRoutes(routes map[string][]route) map[string][]route {
	for _, rs := range routes {
		for i := range rs {
			rs[i].bind()
		}
	}
	return routes
}

func (rt *route) bind() {
	if rt.name == OpUnknown {
		return
	}
	op := mustLookup(rt.name)
	rt.action, rt.hdrActions = targetAuthorizations(op)
	if rt.copy == "" {
		return
	}
	// the copy variant runs through the same route, so it must authorize
	// the same for its target and add only the read of its source
	cp := mustLookup(rt.copy)
	want := append(onTarget(op), s3op.Authorization{Action: s3op.ActionGetObject, On: s3op.OnCopySource})
	if !slices.Equal(onTarget(cp), onTarget(op)) || len(cp.Authorizations) != len(want) || !slices.Contains(cp.Authorizations, want[len(want)-1]) {
		panic("s3gw: s3op catalog: " + rt.copy + " must authorize what " + rt.name + " does plus s3:GetObject on the copy source")
	}
}

func mustLookup(name string) s3op.Operation {
	op, ok := s3op.Lookup(name)
	if !ok {
		panic("s3gw: operation " + name + " is not in the s3op catalog")
	}
	return op
}

func onTarget(op s3op.Operation) []s3op.Authorization {
	var out []s3op.Authorization
	for _, a := range op.Authorizations {
		if a.On == s3op.OnTarget {
			out = append(out, a)
		}
	}
	return out
}

// targetAuthorizations splits an operation's authorizations on its target
// into the unconditional action and the header-conditional ones.
func targetAuthorizations(op s3op.Operation) (string, []headerAction) {
	var action string
	var hdr []headerAction
	for _, a := range onTarget(op) {
		if a.Header == "" {
			if action != "" {
				panic("s3gw: s3op catalog: " + op.Name + " has more than one unconditional action")
			}
			action = a.Action
			continue
		}
		present, ok := headerPresence[a.Header]
		if !ok {
			panic("s3gw: s3op catalog: " + op.Name + ": no presence check for header " + a.Header)
		}
		hdr = append(hdr, headerAction{present, a.Action})
	}
	return action, hdr
}

// authorizeRoute authorizes the route's actions, the unconditional one and
// those the headers in hdr add, and returns them in authorization order.
func (c *opCtx) authorizeRoute(rt *route, hdr signedHeader) ([]string, *s3err.Error) {
	var actions []string
	if rt.action != "" {
		if err := c.authorize(rt.action); err != nil {
			return nil, err
		}
		actions = append(actions, rt.action)
	}
	for _, ha := range rt.hdrActions {
		if !ha.present(hdr) || slices.Contains(actions, ha.action) {
			continue
		}
		if err := c.authorize(ha.action); err != nil {
			return nil, err
		}
		actions = append(actions, ha.action)
	}
	return actions, nil
}

// dispatch runs the first matching route's checks and handler, in the
// order: parameter check, canned-ACL header, action authorization, header
// actions, copy source, handler.
func (c *opCtx) dispatch(routes []route) error {
	// SSE-C would be silently dropped by every operation, which is a
	// security expectation violated without a word — refuse it up front.
	// SSE values are validated here too, before the hooks, so Op.SSE only
	// ever carries a supported value.
	if err := checkSSEC(c.hdr); err != nil {
		return err
	}
	if err := checkSSE(c.hdr); err != nil {
		return err
	}
	for _, rt := range routes {
		if rt.match != nil && !rt.match(c.query) {
			continue
		}
		if rt.params != nil {
			if err := rt.params.check(c.query); err != nil {
				return err
			}
		}
		if rt.aclHdr {
			if err := checkACLHeader(c.hdr); err != nil {
				return err
			}
		}
		actions, s3e := c.authorizeRoute(&rt, c.hdr)
		if s3e != nil {
			return s3e
		}
		name := rt.name
		if rt.copy != "" && c.signed(hdrCopySource) != "" {
			name = rt.copy
			src, s3e := c.g.resolveCopySource(c)
			if s3e != nil {
				return s3e
			}
			c.copySource = src
			actions = append(actions, s3op.ActionGetObject)
		}
		op := &Op{
			Method:         c.r.Method,
			Operation:      name,
			Actions:        actions,
			Tenant:         c.vr.Tenant,
			User:           c.vr.User,
			Bucket:         c.rt.cfg.Name,
			BucketOwner:    c.rt.cfg.Tenant,
			Key:            c.key,
			Request:        c.objectRequest(rt.attrs),
			BucketMetadata: c.rt.cfg.Metadata,
			KeyMetadata:    c.vr.KeyMetadata,
		}
		c.op = op
		if info := recordOf(c.r.Context()); info != nil {
			info.Op = op
		}
		return c.g.runOp(c.r.Context(), op, c, func() error {
			return rt.handle(c.g, c)
		})
	}
	return s3err.NotImplemented("this operation")
}

func (g *Gateway) handleBucketRequest(w http.ResponseWriter, r *http.Request, rt *bucketRT, vr *verifiedRequest, query url.Values) error {
	routes, ok := bucketRoutes[r.Method]
	if !ok {
		return s3err.NotImplemented("this bucket operation")
	}
	return (&opCtx{g: g, w: w, r: r, rt: rt, vr: vr, hdr: newSignedHeader(r, vr.SignedHeaders), query: query}).dispatch(routes)
}

func (g *Gateway) handleObjectRequest(w http.ResponseWriter, r *http.Request, rt *bucketRT, vr *verifiedRequest, query url.Values, key string) error {
	routes, ok := objectRoutes[r.Method]
	if !ok {
		return s3err.New(http.StatusMethodNotAllowed, "MethodNotAllowed",
			"The specified method is not allowed against this resource.")
	}
	return (&opCtx{g: g, w: w, r: r, rt: rt, vr: vr, hdr: newSignedHeader(r, vr.SignedHeaders), query: query, key: key}).dispatch(routes)
}

func has(key string) func(url.Values) bool {
	return func(q url.Values) bool { return q.Has(key) }
}

func hasListTypeV2(q url.Values) bool { return q.Get(qpListType) == "2" }

func hasUploadPart(q url.Values) bool { return q.Has(qpUploadID) || q.Has(qpPartNumber) }

// notImplemented returns a route for a known S3 operation the gateway
// deliberately does not support: it is recorded on Op under its name so a
// service can count how often it is attempted, and always fails with
// NotImplemented.
func notImplemented(match func(url.Values) bool, name string) route {
	return route{match: match, name: name, handle: func(*Gateway, *opCtx) error { return s3err.NotImplemented(name) }}
}

// unknownOperation is the fallback route for a request matching no known
// operation, recorded as OpUnknown.
func unknownOperation(what string) route {
	return route{name: OpUnknown, handle: func(*Gateway, *opCtx) error { return s3err.NotImplemented(what) }}
}

// noQuery matches a request with no query parameters at all.
func noQuery(q url.Values) bool { return len(q) == 0 }

func rejectACL(*Gateway, *opCtx) error { return errACLNotSupported() }

// putObjectOrCopy and uploadPartOrCopy pick the copy variant when the
// request carries an x-amz-copy-source header.
func (g *Gateway) putObjectOrCopy(c *opCtx) error {
	if c.copySource != "" {
		return g.copyObject(c)
	}
	return g.putObject(c)
}

func (g *Gateway) uploadPartOrCopy(c *opCtx) error {
	if c.copySource != "" {
		return g.uploadPartCopy(c)
	}
	return g.uploadPart(c)
}

var bucketRoutes = bindRoutes(map[string][]route{
	http.MethodGet: {
		{match: has(subUploads), params: listMultipartUploadsParams, name: s3op.OpListMultipartUploads, handle: (*Gateway).listMultipartUploads},
		{match: has(subLocation), params: locationOnlyParams, name: s3op.OpGetBucketLocation, handle: (*Gateway).getBucketLocation},
		{match: has(subACL), params: aclOnlyParams, name: s3op.OpGetBucketAcl, handle: (*Gateway).getBucketACL},
		{match: has(subPolicy), params: policyOnlyParams, name: s3op.OpGetBucketPolicy, handle: (*Gateway).getBucketPolicy},
		{match: has(subCORS), params: corsOnlyParams, name: s3op.OpGetBucketCors, handle: (*Gateway).getBucketCors},
		{match: has(subObjectLock), params: objectLockOnlyParams, name: s3op.OpGetObjectLockConfiguration, handle: (*Gateway).getObjectLockConfiguration},
		{match: has(subVersioning), params: versioningOnlyParams, name: s3op.OpGetBucketVersioning, handle: (*Gateway).getBucketVersioning},
		{match: has(subEncryption), params: encryptionOnlyParams, name: s3op.OpGetBucketEncryption, handle: (*Gateway).getBucketEncryption},
		{match: has(subPolicyStatus), params: policyStatusOnlyParams, name: s3op.OpGetBucketPolicyStatus, handle: (*Gateway).getBucketPolicyStatus},
		{match: has(subOwnership), params: ownershipOnlyParams, name: s3op.OpGetBucketOwnershipControls, handle: (*Gateway).getBucketOwnershipControls},
		{match: has(subPublicAccess), params: publicAccessOnlyParams, name: s3op.OpGetPublicAccessBlock, handle: (*Gateway).getPublicAccessBlock},
		{match: has(subVersions), params: listObjectVersionsParams, name: s3op.OpListObjectVersions, handle: (*Gateway).listObjectVersions},
		{match: hasListTypeV2, params: listObjectsV2Params, name: s3op.OpListObjectsV2, handle: (*Gateway).listObjectsV2},
		{params: listObjectsV1Params, name: s3op.OpListObjects, handle: (*Gateway).listObjectsV1},
	},
	http.MethodHead: {
		{name: s3op.OpHeadBucket, handle: (*Gateway).headBucket},
	},
	http.MethodPut: {
		{match: has(subACL), name: s3op.OpPutBucketAcl, handle: rejectACL},
		// bucket configuration is written where the bucket is created — the
		// control plane / store — never via the S3 API: policies and CORS
		// rules live in the store, and versioning / Object Lock defaults
		// would let a data-plane key overwrite what the bucket's creator
		// configured (the reads stay proxied)
		notImplemented(has(subVersioning), s3op.OpPutBucketVersioning),
		notImplemented(has(subObjectLock), s3op.OpPutObjectLockConfiguration),
		notImplemented(has(subPolicy), s3op.OpPutBucketPolicy),
		notImplemented(has(subCORS), s3op.OpPutBucketCors),
		notImplemented(has(subEncryption), s3op.OpPutBucketEncryption),
		notImplemented(has(subOwnership), s3op.OpPutBucketOwnershipControls),
		notImplemented(has(subPublicAccess), s3op.OpPutPublicAccessBlock),
		// buckets are created and deleted by the control plane
		notImplemented(noQuery, s3op.OpCreateBucket),
		unknownOperation("this bucket operation"),
	},
	http.MethodDelete: {
		notImplemented(has(subPolicy), s3op.OpDeleteBucketPolicy),
		notImplemented(has(subCORS), s3op.OpDeleteBucketCors),
		notImplemented(has(subEncryption), s3op.OpDeleteBucketEncryption),
		notImplemented(has(subOwnership), s3op.OpDeleteBucketOwnershipControls),
		notImplemented(has(subPublicAccess), s3op.OpDeletePublicAccessBlock),
		notImplemented(noQuery, s3op.OpDeleteBucket),
		unknownOperation("this bucket operation"),
	},
	http.MethodPost: {
		// its actions are authorized per object inside deleteObjects
		{match: has(subDelete), params: deleteOnlyParams, name: s3op.OpDeleteObjects, handle: (*Gateway).deleteObjects},
		unknownOperation("this bucket operation"),
	},
})

var objectRoutes = bindRoutes(map[string][]route{
	http.MethodGet: {
		{match: has(qpUploadID), params: listPartsParams, name: s3op.OpListParts, handle: (*Gateway).listParts},
		{match: has(subTagging), params: taggingParams, name: s3op.OpGetObjectTagging, handle: (*Gateway).getObjectTagging},
		{match: has(subACL), params: aclParams, name: s3op.OpGetObjectAcl, handle: (*Gateway).getObjectACL},
		{match: has(subRetention), params: retentionParams, name: s3op.OpGetObjectRetention, handle: (*Gateway).getObjectRetention},
		{match: has(subLegalHold), params: legalHoldParams, name: s3op.OpGetObjectLegalHold, handle: (*Gateway).getObjectLegalHold},
		{match: has(subAttributes), params: attributesParams, name: s3op.OpGetObjectAttributes, handle: (*Gateway).getObjectAttributes},
		{params: getObjectParams, name: s3op.OpGetObject, handle: (*Gateway).getObject},
	},
	http.MethodHead: {
		{params: headObjectParams, name: s3op.OpHeadObject, handle: (*Gateway).headObject},
	},
	http.MethodPut: {
		{match: has(subTagging), params: taggingParams, name: s3op.OpPutObjectTagging, handle: (*Gateway).putObjectTagging},
		{match: has(subACL), name: s3op.OpPutObjectAcl, handle: rejectACL},
		{match: has(subRetention), params: retentionParams, name: s3op.OpPutObjectRetention, handle: (*Gateway).putObjectRetention},
		{match: has(subLegalHold), params: legalHoldParams, name: s3op.OpPutObjectLegalHold, handle: (*Gateway).putObjectLegalHold},
		{match: hasUploadPart, params: uploadPartParams, name: s3op.OpUploadPart, copy: s3op.OpUploadPartCopy, handle: (*Gateway).uploadPartOrCopy},
		{params: noParams, aclHdr: true, attrs: true, name: s3op.OpPutObject, copy: s3op.OpCopyObject, handle: (*Gateway).putObjectOrCopy},
	},
	http.MethodDelete: {
		{match: has(qpUploadID), params: uploadIDOnlyParams, name: s3op.OpAbortMultipartUpload, handle: (*Gateway).abortMultipartUpload},
		{match: has(subTagging), params: taggingParams, name: s3op.OpDeleteObjectTagging, handle: (*Gateway).deleteObjectTagging},
		{params: versionIDOnlyParams, name: s3op.OpDeleteObject, handle: (*Gateway).deleteObject},
	},
	http.MethodPost: {
		{match: has(subUploads), params: uploadsOnlyParams, aclHdr: true, attrs: true, name: s3op.OpCreateMultipartUpload, handle: (*Gateway).createMultipartUpload},
		{match: has(qpUploadID), params: uploadIDOnlyParams, name: s3op.OpCompleteMultipartUpload, handle: (*Gateway).completeMultipartUpload},
		unknownOperation("this operation"),
	},
})

// paramSet is the set of query parameters an operation accepts.
type paramSet map[string]bool

func newParamSet(names ...string) paramSet {
	p := make(paramSet, len(names))
	for _, n := range names {
		p[n] = true
	}
	return p
}

// check returns a NotImplemented error for the first query parameter not in
// the set. Unknown subresources are rejected loudly (501) rather than
// silently ignored, so that clients using unsupported operations fail
// clearly.
func (p paramSet) check(query url.Values) *s3err.Error {
	for k := range query {
		if k == "x-id" {
			// an aws-sdk internal operation hint, not a subresource
			continue
		}
		if strings.HasPrefix(strings.ToLower(k), "x-amz-") {
			// presigned auth params and hoisted headers, not subresources
			continue
		}
		if !p[k] {
			return s3err.NotImplemented("query parameter " + k)
		}
	}
	return nil
}

// Query parameters that select an operation. Using named constants (rather
// than repeating the literal in the dispatch switch and its paramSet) makes
// a typo a compile error instead of a silent misroute, and keeps each key
// defined in one place.
const (
	// subresource discriminators
	subACL          = "acl"
	subCORS         = "cors"
	subDelete       = "delete"
	subEncryption   = "encryption"
	subLegalHold    = "legal-hold"
	subLocation     = "location"
	subObjectLock   = "object-lock"
	subOwnership    = "ownershipControls"
	subPolicy       = "policy"
	subPolicyStatus = "policyStatus"
	subPublicAccess = "publicAccessBlock"
	subAttributes   = "attributes"
	subRetention    = "retention"
	subTagging      = "tagging"
	subUploads      = "uploads"
	subVersioning   = "versioning"
	subVersions     = "versions"
	// other operation-selecting parameters
	qpUploadID   = "uploadId"
	qpPartNumber = "partNumber"
	qpListType   = "list-type"
	qpVersionID  = "versionId"
)

var (
	noParams               = newParamSet()
	aclOnlyParams          = newParamSet(subACL)
	policyOnlyParams       = newParamSet(subPolicy)
	corsOnlyParams         = newParamSet(subCORS)
	objectLockOnlyParams   = newParamSet(subObjectLock)
	retentionParams        = newParamSet(subRetention, qpVersionID)
	legalHoldParams        = newParamSet(subLegalHold, qpVersionID)
	aclParams              = newParamSet(subACL, qpVersionID)
	locationOnlyParams     = newParamSet(subLocation)
	deleteOnlyParams       = newParamSet(subDelete)
	uploadsOnlyParams      = newParamSet(subUploads)
	uploadIDOnlyParams     = newParamSet(qpUploadID)
	versionIDOnlyParams    = newParamSet(qpVersionID)
	versioningOnlyParams   = newParamSet(subVersioning)
	encryptionOnlyParams   = newParamSet(subEncryption)
	policyStatusOnlyParams = newParamSet(subPolicyStatus)
	ownershipOnlyParams    = newParamSet(subOwnership)
	publicAccessOnlyParams = newParamSet(subPublicAccess)
	taggingParams          = newParamSet(subTagging, qpVersionID)
	uploadPartParams       = newParamSet(qpUploadID, qpPartNumber)
	listPartsParams        = newParamSet(qpUploadID, "max-parts", "part-number-marker")
	listObjectsV1Params    = newParamSet(
		"prefix", "delimiter", "marker", "max-keys", "encoding-type")
	listObjectsV2Params = newParamSet(
		qpListType, "prefix", "delimiter", "max-keys", "continuation-token",
		"start-after", "fetch-owner", "encoding-type")
	listObjectVersionsParams = newParamSet(
		subVersions, "prefix", "delimiter", "key-marker", "version-id-marker",
		"max-keys", "encoding-type")
	listMultipartUploadsParams = newParamSet(
		subUploads, "prefix", "delimiter", "key-marker", "upload-id-marker",
		"max-uploads", "encoding-type")
	getObjectParams = newParamSet(
		qpVersionID, qpPartNumber, "response-content-type", "response-content-disposition",
		"response-cache-control", "response-content-encoding",
		"response-content-language", "response-expires")
	headObjectParams  = newParamSet(qpVersionID, qpPartNumber)
	listBucketsParams = newParamSet("max-buckets", "continuation-token")
	attributesParams  = newParamSet(subAttributes, qpVersionID)
)
