package s3rp

import (
	"errors"
	"github.com/fujiwara/s3rp/store"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type handlerFunc func(w http.ResponseWriter, r *http.Request) error

// Handler returns the http.Handler of the proxy.
//
// A single catch-all route is used instead of ServeMux patterns because the
// mux cleans paths (collapsing // and resolving dot segments) and redirects,
// which breaks S3 keys and signature verification.
func (app *S3RP) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", app.wrapHandler(app.handleRequest))
	return mux
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (app *S3RP) wrapHandler(h handlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		requestID := newRequestID()
		w.Header().Set("x-amz-request-id", requestID)
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		err := h(sw, r)
		if err != nil {
			var s3err *S3Error
			if !errors.As(err, &s3err) {
				slog.ErrorContext(r.Context(), "internal error", "error", err, "request_id", requestID)
				s3err = newS3Error(http.StatusInternalServerError, "InternalError",
					"We encountered an internal error. Please try again.")
			}
			writeS3Error(sw, r, s3err, requestID)
		}
		slog.InfoContext(r.Context(), "request",
			"remote", r.RemoteAddr,
			"method", r.Method,
			"path", r.URL.Path,
			"query", r.URL.RawQuery,
			"status", sw.status,
			"duration", time.Since(start).String(),
			"request_id", requestID,
		)
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

func (app *S3RP) handleRequest(w http.ResponseWriter, r *http.Request) error {
	// browsers send CORS preflights without authentication
	if r.Method == http.MethodOptions {
		return app.handlePreflight(w, r)
	}
	vr, s3err := app.verifyRequest(r)
	if s3err != nil {
		return s3err
	}
	bucket, key, err := splitPath(r.URL.EscapedPath())
	if err != nil {
		return newS3Error(http.StatusBadRequest, "InvalidURI", "Couldn't parse the specified URI.")
	}

	if bucket == "" {
		if r.Method != http.MethodGet {
			return newS3Error(http.StatusMethodNotAllowed, "MethodNotAllowed",
				"The specified method is not allowed against this resource.")
		}
		return app.listBuckets(w, r, vr)
	}

	b, err := app.store.GetBucket(r.Context(), vr.Tenant, bucket)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return errAccessDenied()
		}
		slog.ErrorContext(r.Context(), "failed to look up bucket", "error", err)
		return newS3Error(http.StatusInternalServerError, "InternalError", "bucket lookup failed")
	}
	client, err := app.backendClient(r.Context(), b.Backend)
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to build backend client", "error", err)
		return newS3Error(http.StatusInternalServerError, "InternalError", "backend client failed")
	}
	rt := &bucketRT{cfg: b, client: client}
	setCORSHeaders(w, r, b)

	query := r.URL.Query()
	if key == "" {
		return app.handleBucketRequest(w, r, rt, vr, query)
	}
	return app.handleObjectRequest(w, r, rt, vr, query, key)
}

func (app *S3RP) handleBucketRequest(w http.ResponseWriter, r *http.Request, rt *bucketRT, vr *verifiedRequest, query url.Values) error {
	// the resource of bucket-level operations is the bucket itself
	authorize := func(action string) *S3Error {
		return app.authorize(vr, rt.cfg, action, rt.cfg.Name)
	}
	switch r.Method {
	case http.MethodGet:
		switch {
		case query.Has("uploads"):
			if err := listMultipartUploadsParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:ListBucketMultipartUploads"); err != nil {
				return err
			}
			return app.listMultipartUploads(w, r, rt)
		case query.Has("location"):
			if err := locationOnlyParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:GetBucketLocation"); err != nil {
				return err
			}
			return app.getBucketLocation(w, rt)
		case query.Has("acl"):
			if err := aclOnlyParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:GetBucketAcl"); err != nil {
				return err
			}
			return app.getBucketACL(w, vr)
		case query.Has("policy"):
			if err := policyOnlyParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:GetBucketPolicy"); err != nil {
				return err
			}
			return app.getBucketPolicy(w, rt)
		case query.Has("cors"):
			if err := corsOnlyParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:GetBucketCORS"); err != nil {
				return err
			}
			return app.getBucketCors(w, rt)
		case query.Has("versioning"):
			if err := versioningOnlyParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:GetBucketVersioning"); err != nil {
				return err
			}
			return app.getBucketVersioning(w, r, rt)
		case query.Has("versions"):
			if err := listObjectVersionsParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:ListBucket"); err != nil {
				return err
			}
			return app.listObjectVersions(w, r, rt)
		case query.Get("list-type") == "2":
			if err := listObjectsV2Params.check(query); err != nil {
				return err
			}
			if err := authorize("s3:ListBucket"); err != nil {
				return err
			}
			return app.listObjectsV2(w, r, rt)
		default:
			if err := listObjectsV1Params.check(query); err != nil {
				return err
			}
			if err := authorize("s3:ListBucket"); err != nil {
				return err
			}
			return app.listObjectsV1(w, r, rt)
		}
	case http.MethodHead:
		if err := authorize("s3:ListBucket"); err != nil {
			return err
		}
		return app.headBucket(w, r, rt)
	case http.MethodPut:
		if query.Has("versioning") {
			if err := versioningOnlyParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:PutBucketVersioning"); err != nil {
				return err
			}
			return app.putBucketVersioning(w, r, rt, vr)
		}
		if query.Has("acl") {
			return errACLNotSupported()
		}
		if query.Has("policy") {
			// bucket policies are defined in the config (or a future
			// control plane), not via the S3 API
			return errNotImplemented("PutBucketPolicy")
		}
		if query.Has("cors") {
			return errNotImplemented("PutBucketCors")
		}
		return errNotImplemented("this bucket operation")
	case http.MethodDelete:
		if query.Has("policy") {
			return errNotImplemented("DeleteBucketPolicy")
		}
		if query.Has("cors") {
			return errNotImplemented("DeleteBucketCors")
		}
		return errNotImplemented("this bucket operation")
	case http.MethodPost:
		if query.Has("delete") {
			if err := deleteOnlyParams.check(query); err != nil {
				return err
			}
			// s3:DeleteObject is evaluated per object inside
			return app.deleteObjects(w, r, rt, vr)
		}
		return errNotImplemented("this bucket operation")
	default:
		return errNotImplemented("this bucket operation")
	}
}

func (app *S3RP) handleObjectRequest(w http.ResponseWriter, r *http.Request, rt *bucketRT, vr *verifiedRequest, query url.Values, key string) error {
	authorize := func(action string) *S3Error {
		return app.authorize(vr, rt.cfg, action, rt.cfg.Name+"/"+key)
	}
	switch r.Method {
	case http.MethodGet:
		if query.Has("uploadId") {
			if err := listPartsParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:ListMultipartUploadParts"); err != nil {
				return err
			}
			return app.listParts(w, r, rt, key)
		}
		if query.Has("tagging") {
			if err := taggingParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:GetObjectTagging"); err != nil {
				return err
			}
			return app.getObjectTagging(w, r, rt, key)
		}
		if query.Has("acl") {
			if err := aclParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:GetObjectAcl"); err != nil {
				return err
			}
			return app.getObjectACL(w, r, rt, key, vr)
		}
		if err := getObjectParams.check(query); err != nil {
			return err
		}
		if err := authorize("s3:GetObject"); err != nil {
			return err
		}
		return app.getObject(w, r, rt, key)
	case http.MethodHead:
		if err := versionIDOnlyParams.check(query); err != nil {
			return err
		}
		if err := authorize("s3:GetObject"); err != nil {
			return err
		}
		return app.headObject(w, r, rt, key)
	case http.MethodPut:
		if query.Has("tagging") {
			if err := taggingParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:PutObjectTagging"); err != nil {
				return err
			}
			return app.putObjectTagging(w, r, rt, key, vr)
		}
		if query.Has("acl") {
			return errACLNotSupported()
		}
		hasCopySource := r.Header.Get("x-amz-copy-source") != ""
		if query.Has("uploadId") || query.Has("partNumber") {
			if err := uploadPartParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:PutObject"); err != nil {
				return err
			}
			if hasCopySource {
				return app.uploadPartCopy(w, r, rt, key, vr)
			}
			return app.uploadPart(w, r, rt, key, vr)
		}
		if err := noParams.check(query); err != nil {
			return err
		}
		if err := checkACLHeader(r); err != nil {
			return err
		}
		if err := authorize("s3:PutObject"); err != nil {
			return err
		}
		if hasCopySource {
			return app.copyObject(w, r, rt, key, vr)
		}
		return app.putObject(w, r, rt, key, vr)
	case http.MethodDelete:
		if query.Has("uploadId") {
			if err := uploadIDOnlyParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:AbortMultipartUpload"); err != nil {
				return err
			}
			return app.abortMultipartUpload(w, r, rt, key)
		}
		if query.Has("tagging") {
			if err := taggingParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:DeleteObjectTagging"); err != nil {
				return err
			}
			return app.deleteObjectTagging(w, r, rt, key)
		}
		if err := versionIDOnlyParams.check(query); err != nil {
			return err
		}
		if err := authorize("s3:DeleteObject"); err != nil {
			return err
		}
		return app.deleteObject(w, r, rt, key)
	case http.MethodPost:
		switch {
		case query.Has("uploads"):
			if err := uploadsOnlyParams.check(query); err != nil {
				return err
			}
			if err := checkACLHeader(r); err != nil {
				return err
			}
			if err := authorize("s3:PutObject"); err != nil {
				return err
			}
			return app.createMultipartUpload(w, r, rt, key)
		case query.Has("uploadId"):
			if err := uploadIDOnlyParams.check(query); err != nil {
				return err
			}
			if err := authorize("s3:PutObject"); err != nil {
				return err
			}
			return app.completeMultipartUpload(w, r, rt, key, vr)
		default:
			return errNotImplemented("this operation")
		}
	default:
		return newS3Error(http.StatusMethodNotAllowed, "MethodNotAllowed",
			"The specified method is not allowed against this resource.")
	}
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
func (p paramSet) check(query url.Values) *S3Error {
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
			return errNotImplemented("query parameter " + k)
		}
	}
	return nil
}

var (
	noParams             = newParamSet()
	aclOnlyParams        = newParamSet("acl")
	policyOnlyParams     = newParamSet("policy")
	corsOnlyParams       = newParamSet("cors")
	aclParams            = newParamSet("acl", "versionId")
	locationOnlyParams   = newParamSet("location")
	deleteOnlyParams     = newParamSet("delete")
	uploadsOnlyParams    = newParamSet("uploads")
	uploadIDOnlyParams   = newParamSet("uploadId")
	versionIDOnlyParams  = newParamSet("versionId")
	versioningOnlyParams = newParamSet("versioning")
	taggingParams        = newParamSet("tagging", "versionId")
	uploadPartParams     = newParamSet("uploadId", "partNumber")
	listPartsParams      = newParamSet("uploadId", "max-parts", "part-number-marker")
	listObjectsV1Params  = newParamSet(
		"prefix", "delimiter", "marker", "max-keys", "encoding-type")
	listObjectsV2Params = newParamSet(
		"list-type", "prefix", "delimiter", "max-keys", "continuation-token",
		"start-after", "fetch-owner", "encoding-type")
	listObjectVersionsParams = newParamSet(
		"versions", "prefix", "delimiter", "key-marker", "version-id-marker",
		"max-keys", "encoding-type")
	listMultipartUploadsParams = newParamSet(
		"uploads", "prefix", "delimiter", "key-marker", "upload-id-marker",
		"max-uploads", "encoding-type")
	getObjectParams = newParamSet(
		"versionId", "response-content-type", "response-content-disposition",
		"response-cache-control", "response-content-encoding",
		"response-content-language", "response-expires")
)
