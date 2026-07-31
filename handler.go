package s3rp

import (
	"errors"
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
		return app.listBuckets(w, vr)
	}

	rt := app.byKey[vr.AccessKeyID][bucket]
	if rt == nil {
		return errAccessDenied()
	}

	query := r.URL.Query()
	switch {
	case key == "":
		switch r.Method {
		case http.MethodGet:
			if query.Has("uploads") {
				if sub := unsupportedQuery(query, listMultipartUploadsParams); sub != "" {
					return errNotImplemented(sub)
				}
				return app.listMultipartUploads(w, r, rt)
			}
			if query.Get("list-type") != "2" {
				return errNotImplemented("this bucket operation")
			}
			if sub := unsupportedQuery(query, listObjectsV2Params); sub != "" {
				return errNotImplemented(sub)
			}
			return app.listObjectsV2(w, r, rt)
		case http.MethodHead:
			return app.headBucket(w, r, rt)
		default:
			return errNotImplemented("this bucket operation")
		}
	default:
		switch r.Method {
		case http.MethodGet:
			if query.Has("uploadId") {
				if sub := unsupportedQuery(query, listPartsParams); sub != "" {
					return errNotImplemented(sub)
				}
				return app.listParts(w, r, rt, key)
			}
			if sub := unsupportedQuery(query, getObjectParams); sub != "" {
				return errNotImplemented(sub)
			}
			return app.getObject(w, r, rt, key)
		case http.MethodHead:
			if sub := unsupportedQuery(query, nil); sub != "" {
				return errNotImplemented(sub)
			}
			return app.headObject(w, r, rt, key)
		case http.MethodPut:
			if r.Header.Get("x-amz-copy-source") != "" {
				if query.Has("uploadId") {
					return errNotImplemented("UploadPartCopy")
				}
				return errNotImplemented("CopyObject")
			}
			if query.Has("uploadId") || query.Has("partNumber") {
				if sub := unsupportedQuery(query, uploadPartParams); sub != "" {
					return errNotImplemented(sub)
				}
				return app.uploadPart(w, r, rt, key, vr)
			}
			if sub := unsupportedQuery(query, nil); sub != "" {
				return errNotImplemented(sub)
			}
			return app.putObject(w, r, rt, key, vr)
		case http.MethodDelete:
			if query.Has("uploadId") {
				if sub := unsupportedQuery(query, uploadIDOnlyParams); sub != "" {
					return errNotImplemented(sub)
				}
				return app.abortMultipartUpload(w, r, rt, key)
			}
			if sub := unsupportedQuery(query, nil); sub != "" {
				return errNotImplemented(sub)
			}
			return app.deleteObject(w, r, rt, key)
		case http.MethodPost:
			switch {
			case query.Has("uploads"):
				if sub := unsupportedQuery(query, uploadsOnlyParams); sub != "" {
					return errNotImplemented(sub)
				}
				return app.createMultipartUpload(w, r, rt, key)
			case query.Has("uploadId"):
				if sub := unsupportedQuery(query, uploadIDOnlyParams); sub != "" {
					return errNotImplemented(sub)
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
}

var listObjectsV2Params = map[string]bool{
	"list-type":          true,
	"prefix":             true,
	"delimiter":          true,
	"max-keys":           true,
	"continuation-token": true,
	"start-after":        true,
	"fetch-owner":        true,
	"encoding-type":      true,
}

var listMultipartUploadsParams = map[string]bool{
	"uploads":          true,
	"prefix":           true,
	"delimiter":        true,
	"key-marker":       true,
	"upload-id-marker": true,
	"max-uploads":      true,
	"encoding-type":    true,
}

var listPartsParams = map[string]bool{
	"uploadId":           true,
	"max-parts":          true,
	"part-number-marker": true,
}

var uploadPartParams = map[string]bool{
	"uploadId":   true,
	"partNumber": true,
}

var uploadsOnlyParams = map[string]bool{
	"uploads": true,
}

var uploadIDOnlyParams = map[string]bool{
	"uploadId": true,
}

var getObjectParams = map[string]bool{
	"response-content-type":        true,
	"response-content-disposition": true,
	"response-cache-control":       true,
	"response-content-encoding":    true,
	"response-content-language":    true,
	"response-expires":             true,
}

// unsupportedQuery returns the first query parameter not in allowed.
// Unknown subresources are rejected loudly (501) rather than silently
// ignored, so that clients using unsupported operations fail clearly.
func unsupportedQuery(query url.Values, allowed map[string]bool) string {
	for k := range query {
		if k == "x-id" {
			// an aws-sdk internal operation hint, not a subresource
			continue
		}
		if strings.HasPrefix(strings.ToLower(k), "x-amz-") {
			// presigned auth params and hoisted headers, not subresources
			continue
		}
		if !allowed[k] {
			return "query parameter " + k
		}
	}
	return ""
}
