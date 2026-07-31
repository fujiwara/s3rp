package s3rp

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/store"
)

// CORS is handled by the proxy itself (not passed through to the backend):
// the browser talks to s3rp, e.g. for uploads via presigned URLs.

// matchCORSRule returns the first rule matching the origin and method.
func matchCORSRule(rules []*store.CORSRule, origin, method string) *store.CORSRule {
	for _, rule := range rules {
		for _, o := range rule.AllowedOrigins {
			if policy.Match(o, origin) {
				if slices.Contains(rule.AllowedMethods, method) {
					return rule
				}
			}
		}
	}
	return nil
}

func allowsAllOrigins(rule *store.CORSRule) bool {
	return slices.Contains(rule.AllowedOrigins, "*")
}

// handlePreflight answers a CORS preflight request. Preflights are sent by
// browsers without authentication, so this runs before signature
// verification and must not reveal anything beyond the CORS decision.
func (app *S3RP) handlePreflight(w http.ResponseWriter, r *http.Request) error {
	origin := r.Header.Get("Origin")
	method := r.Header.Get("Access-Control-Request-Method")
	if origin == "" || method == "" {
		return newS3Error(http.StatusBadRequest, "BadRequest",
			"Insufficient information. Origin request header needed.")
	}
	bucket, _, err := splitPath(r.URL.EscapedPath())
	if err != nil || bucket == "" {
		return corsNotAllowed()
	}
	b, err := app.store.GetBucketByName(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return corsNotAllowed()
		}
		return newS3Error(http.StatusInternalServerError, "InternalError", "bucket lookup failed")
	}
	rule := matchCORSRule(b.CORS, origin, method)
	if rule == nil {
		return corsNotAllowed()
	}
	if reqHeaders := r.Header.Get("Access-Control-Request-Headers"); reqHeaders != "" {
		for h := range strings.SplitSeq(reqHeaders, ",") {
			h = strings.TrimSpace(h)
			if !corsHeaderAllowed(rule.AllowedHeaders, h) {
				return corsNotAllowed()
			}
		}
		w.Header().Set("Access-Control-Allow-Headers", reqHeaders)
	}
	h := w.Header()
	if allowsAllOrigins(rule) {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
	}
	h.Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethods, ", "))
	if rule.MaxAgeSeconds > 0 {
		h.Set("Access-Control-Max-Age", strconv.Itoa(rule.MaxAgeSeconds))
	}
	h.Set("Vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers")
	w.WriteHeader(http.StatusOK)
	return nil
}

func corsNotAllowed() *S3Error {
	return newS3Error(http.StatusForbidden, "AccessForbidden",
		"CORSResponse: This CORS request is not allowed.")
}

func corsHeaderAllowed(allowed []string, header string) bool {
	for _, a := range allowed {
		if a == "*" || strings.EqualFold(a, header) {
			return true
		}
	}
	return false
}

// setCORSHeaders decorates an actual (non-preflight) response with CORS
// headers when the request carries a matching Origin.
func setCORSHeaders(w http.ResponseWriter, r *http.Request, b *store.Bucket) {
	origin := r.Header.Get("Origin")
	if origin == "" || len(b.CORS) == 0 {
		return
	}
	rule := matchCORSRule(b.CORS, origin, r.Method)
	if rule == nil {
		return
	}
	h := w.Header()
	if allowsAllOrigins(rule) {
		h.Set("Access-Control-Allow-Origin", "*")
	} else {
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Credentials", "true")
	}
	if len(rule.ExposeHeaders) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeaders, ", "))
	}
	h.Set("Vary", "Origin")
}

// getBucketCors returns the CORS configuration of the bucket as XML.
func (app *S3RP) getBucketCors(w http.ResponseWriter, rt *bucketRT) error {
	if len(rt.cfg.CORS) == 0 {
		return newS3Error(http.StatusNotFound, "NoSuchCORSConfiguration",
			"The CORS configuration does not exist")
	}
	result := &CORSConfiguration{XMLNS: s3XMLNS}
	for _, rule := range rt.cfg.CORS {
		xr := CORSRuleXML{
			AllowedOrigin: rule.AllowedOrigins,
			AllowedMethod: rule.AllowedMethods,
			AllowedHeader: rule.AllowedHeaders,
			ExposeHeader:  rule.ExposeHeaders,
			MaxAgeSeconds: rule.MaxAgeSeconds,
		}
		result.Rules = append(result.Rules, xr)
	}
	return writeXML(w, result)
}
