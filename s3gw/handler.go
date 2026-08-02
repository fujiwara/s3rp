package s3gw

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/fujiwara/s3rp/cors"
	"github.com/fujiwara/s3rp/s3err"
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
	status  int
	written int64
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.written += int64(n)
	return n, err
}

func (g *Gateway) wrapHandler(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := s3err.NewRequestID()
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
		err := h(sw, r)
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
			code = s3e.Code
			// the cause never reaches the client, so handing it to the
			// observer is the only way it can be seen
			cause = errors.Unwrap(s3e)
			s3err.Write(sw, r, s3e, requestID)
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
		g.observer(r.Context(), info)
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
	vr, s3e := g.verifyRequest(r)
	if s3e != nil {
		return s3e
	}
	if info := recordOf(r.Context()); info != nil {
		info.Tenant, info.User = vr.Tenant, vr.User
	}
	bucket, key, err := splitPath(r.URL.EscapedPath())
	if err != nil {
		return s3err.New(http.StatusBadRequest, "InvalidURI", "Couldn't parse the specified URI.")
	}

	if bucket == "" {
		if r.Method != http.MethodGet {
			return s3err.New(http.StatusMethodNotAllowed, "MethodNotAllowed",
				"The specified method is not allowed against this resource.")
		}
		return g.listBuckets(w, r, vr)
	}

	b, err := g.store.GetBucket(r.Context(), vr.Tenant, bucket)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return s3err.AccessDenied()
		}
		return s3err.Internal(err, "bucket lookup failed")
	}
	client, err := g.backendClient(r.Context(), b.Backend)
	if err != nil {
		return s3err.Internal(err, "backend client failed")
	}
	rt := &bucketRT{cfg: b, client: client}
	cors.SetHeaders(w, r, b.CORS)

	query := r.URL.Query()
	if key == "" {
		return g.handleBucketRequest(w, r, rt, vr, query)
	}
	return g.handleObjectRequest(w, r, rt, vr, query, key)
}

// opCtx carries everything a routed operation needs. key is "" for
// bucket-level operations.
type opCtx struct {
	g     *Gateway
	w     http.ResponseWriter
	r     *http.Request
	rt    *bucketRT
	vr    *verifiedRequest
	query url.Values
	key   string
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

// route selects one operation by a query discriminator and describes the
// checks to run before its handler. handle is a method value of *Gateway, so
// the route table reads as a plain "discriminator -> handler" mapping.
type route struct {
	match  func(url.Values) bool // selects this route (nil = always, the fallback)
	params paramSet              // allowed query parameters (nil = skip the check)
	aclHdr bool                  // reject unsupported canned ACL headers first
	bypass bool                  // also require s3:BypassGovernanceRetention when the bypass header is set
	action string                // s3:* action to authorize ("" = handler authorizes itself)
	handle func(*Gateway, *opCtx) error
}

// dispatch runs the first matching route's checks and handler, in the
// order: parameter check, canned-ACL header, bypass authorization,
// action authorization, handler.
func (c *opCtx) dispatch(routes []route) error {
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
			if err := checkACLHeader(c.r); err != nil {
				return err
			}
		}
		if rt.bypass && bypassGovernanceRetention(c.r) {
			if err := c.authorize("s3:BypassGovernanceRetention"); err != nil {
				return err
			}
		}
		if rt.action != "" {
			if err := c.authorize(rt.action); err != nil {
				return err
			}
		}
		op := &Op{
			Method:         c.r.Method,
			Action:         rt.action,
			Tenant:         c.vr.Tenant,
			User:           c.vr.User,
			Bucket:         c.rt.cfg.Name,
			Key:            c.key,
			BucketMetadata: c.rt.cfg.Metadata,
			KeyMetadata:    c.vr.KeyMetadata,
		}
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
	return (&opCtx{g: g, w: w, r: r, rt: rt, vr: vr, query: query}).dispatch(routes)
}

func (g *Gateway) handleObjectRequest(w http.ResponseWriter, r *http.Request, rt *bucketRT, vr *verifiedRequest, query url.Values, key string) error {
	routes, ok := objectRoutes[r.Method]
	if !ok {
		return s3err.New(http.StatusMethodNotAllowed, "MethodNotAllowed",
			"The specified method is not allowed against this resource.")
	}
	return (&opCtx{g: g, w: w, r: r, rt: rt, vr: vr, query: query, key: key}).dispatch(routes)
}

func has(key string) func(url.Values) bool {
	return func(q url.Values) bool { return q.Has(key) }
}

func hasListTypeV2(q url.Values) bool { return q.Get(qpListType) == "2" }

func hasUploadPart(q url.Values) bool { return q.Has(qpUploadID) || q.Has(qpPartNumber) }

// notImplemented returns a route whose handler always fails with the given
// NotImplemented message (used for operations defined only in the store).
func notImplemented(match func(url.Values) bool, what string) route {
	return route{match: match, handle: func(*Gateway, *opCtx) error { return s3err.NotImplemented(what) }}
}

func rejectACL(*Gateway, *opCtx) error { return errACLNotSupported() }

// putObjectOrCopy and uploadPartOrCopy pick the copy variant when the
// request carries an x-amz-copy-source header.
func (g *Gateway) putObjectOrCopy(c *opCtx) error {
	if c.r.Header.Get("x-amz-copy-source") != "" {
		return g.copyObject(c)
	}
	return g.putObject(c)
}

func (g *Gateway) uploadPartOrCopy(c *opCtx) error {
	if c.r.Header.Get("x-amz-copy-source") != "" {
		return g.uploadPartCopy(c)
	}
	return g.uploadPart(c)
}

var bucketRoutes = map[string][]route{
	http.MethodGet: {
		{match: has(subUploads), params: listMultipartUploadsParams, action: "s3:ListBucketMultipartUploads", handle: (*Gateway).listMultipartUploads},
		{match: has(subLocation), params: locationOnlyParams, action: "s3:GetBucketLocation", handle: (*Gateway).getBucketLocation},
		{match: has(subACL), params: aclOnlyParams, action: "s3:GetBucketAcl", handle: (*Gateway).getBucketACL},
		{match: has(subPolicy), params: policyOnlyParams, action: "s3:GetBucketPolicy", handle: (*Gateway).getBucketPolicy},
		{match: has(subCORS), params: corsOnlyParams, action: "s3:GetBucketCORS", handle: (*Gateway).getBucketCors},
		{match: has(subObjectLock), params: objectLockOnlyParams, action: "s3:GetBucketObjectLockConfiguration", handle: (*Gateway).getObjectLockConfiguration},
		{match: has(subVersioning), params: versioningOnlyParams, action: "s3:GetBucketVersioning", handle: (*Gateway).getBucketVersioning},
		{match: has(subVersions), params: listObjectVersionsParams, action: "s3:ListBucket", handle: (*Gateway).listObjectVersions},
		{match: hasListTypeV2, params: listObjectsV2Params, action: "s3:ListBucket", handle: (*Gateway).listObjectsV2},
		{params: listObjectsV1Params, action: "s3:ListBucket", handle: (*Gateway).listObjectsV1},
	},
	http.MethodHead: {
		{action: "s3:ListBucket", handle: (*Gateway).headBucket},
	},
	http.MethodPut: {
		{match: has(subVersioning), params: versioningOnlyParams, action: "s3:PutBucketVersioning", handle: (*Gateway).putBucketVersioning},
		{match: has(subObjectLock), params: objectLockOnlyParams, action: "s3:PutBucketObjectLockConfiguration", handle: (*Gateway).putObjectLockConfiguration},
		{match: has(subACL), handle: rejectACL},
		// bucket policies and CORS rules are defined in the store, not via the S3 API
		notImplemented(has(subPolicy), "PutBucketPolicy"),
		notImplemented(has(subCORS), "PutBucketCors"),
		notImplemented(nil, "this bucket operation"),
	},
	http.MethodDelete: {
		notImplemented(has(subPolicy), "DeleteBucketPolicy"),
		notImplemented(has(subCORS), "DeleteBucketCors"),
		notImplemented(nil, "this bucket operation"),
	},
	http.MethodPost: {
		// s3:DeleteObject is evaluated per object inside deleteObjects
		{match: has(subDelete), params: deleteOnlyParams, handle: (*Gateway).deleteObjects},
		notImplemented(nil, "this bucket operation"),
	},
}

var objectRoutes = map[string][]route{
	http.MethodGet: {
		{match: has(qpUploadID), params: listPartsParams, action: "s3:ListMultipartUploadParts", handle: (*Gateway).listParts},
		{match: has(subTagging), params: taggingParams, action: "s3:GetObjectTagging", handle: (*Gateway).getObjectTagging},
		{match: has(subACL), params: aclParams, action: "s3:GetObjectAcl", handle: (*Gateway).getObjectACL},
		{match: has(subRetention), params: retentionParams, action: "s3:GetObjectRetention", handle: (*Gateway).getObjectRetention},
		{match: has(subLegalHold), params: legalHoldParams, action: "s3:GetObjectLegalHold", handle: (*Gateway).getObjectLegalHold},
		{params: getObjectParams, action: "s3:GetObject", handle: (*Gateway).getObject},
	},
	http.MethodHead: {
		{params: versionIDOnlyParams, action: "s3:GetObject", handle: (*Gateway).headObject},
	},
	http.MethodPut: {
		{match: has(subTagging), params: taggingParams, action: "s3:PutObjectTagging", handle: (*Gateway).putObjectTagging},
		{match: has(subACL), handle: rejectACL},
		{match: has(subRetention), params: retentionParams, bypass: true, action: "s3:PutObjectRetention", handle: (*Gateway).putObjectRetention},
		{match: has(subLegalHold), params: legalHoldParams, action: "s3:PutObjectLegalHold", handle: (*Gateway).putObjectLegalHold},
		{match: hasUploadPart, params: uploadPartParams, action: "s3:PutObject", handle: (*Gateway).uploadPartOrCopy},
		{params: noParams, aclHdr: true, action: "s3:PutObject", handle: (*Gateway).putObjectOrCopy},
	},
	http.MethodDelete: {
		{match: has(qpUploadID), params: uploadIDOnlyParams, action: "s3:AbortMultipartUpload", handle: (*Gateway).abortMultipartUpload},
		{match: has(subTagging), params: taggingParams, action: "s3:DeleteObjectTagging", handle: (*Gateway).deleteObjectTagging},
		{params: versionIDOnlyParams, bypass: true, action: "s3:DeleteObject", handle: (*Gateway).deleteObject},
	},
	http.MethodPost: {
		{match: has(subUploads), params: uploadsOnlyParams, aclHdr: true, action: "s3:PutObject", handle: (*Gateway).createMultipartUpload},
		{match: has(qpUploadID), params: uploadIDOnlyParams, action: "s3:PutObject", handle: (*Gateway).completeMultipartUpload},
		notImplemented(nil, "this operation"),
	},
}

// paramSet is the set of query parameters an operation accepts.
type paramSet map[string]bool

// newParamSet builds a paramSet from parameter names.
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
	subACL        = "acl"
	subCORS       = "cors"
	subDelete     = "delete"
	subLegalHold  = "legal-hold"
	subLocation   = "location"
	subObjectLock = "object-lock"
	subPolicy     = "policy"
	subRetention  = "retention"
	subTagging    = "tagging"
	subUploads    = "uploads"
	subVersioning = "versioning"
	subVersions   = "versions"
	// other operation-selecting parameters
	qpUploadID   = "uploadId"
	qpPartNumber = "partNumber"
	qpListType   = "list-type"
	qpVersionID  = "versionId"
)

var (
	noParams             = newParamSet()
	aclOnlyParams        = newParamSet(subACL)
	policyOnlyParams     = newParamSet(subPolicy)
	corsOnlyParams       = newParamSet(subCORS)
	objectLockOnlyParams = newParamSet(subObjectLock)
	retentionParams      = newParamSet(subRetention, qpVersionID)
	legalHoldParams      = newParamSet(subLegalHold, qpVersionID)
	aclParams            = newParamSet(subACL, qpVersionID)
	locationOnlyParams   = newParamSet(subLocation)
	deleteOnlyParams     = newParamSet(subDelete)
	uploadsOnlyParams    = newParamSet(subUploads)
	uploadIDOnlyParams   = newParamSet(qpUploadID)
	versionIDOnlyParams  = newParamSet(qpVersionID)
	versioningOnlyParams = newParamSet(subVersioning)
	taggingParams        = newParamSet(subTagging, qpVersionID)
	uploadPartParams     = newParamSet(qpUploadID, qpPartNumber)
	listPartsParams      = newParamSet(qpUploadID, "max-parts", "part-number-marker")
	listObjectsV1Params  = newParamSet(
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
		qpVersionID, "response-content-type", "response-content-disposition",
		"response-cache-control", "response-content-encoding",
		"response-content-language", "response-expires")
)
