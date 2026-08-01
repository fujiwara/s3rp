package s3rp

import (
	"errors"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"
	"net/http"

	"github.com/fujiwara/s3rp/cors"
	"github.com/fujiwara/s3rp/store"
)

// CORS is handled by the proxy itself (not passed through to the backend):
// the browser talks to s3rp, e.g. for uploads via presigned URLs. The rule
// evaluation lives in the cors package; what remains here is resolving the
// bucket and mapping a refusal onto an S3 error.

// handlePreflight answers a CORS preflight request. Preflights are sent by
// browsers without authentication, so this runs before signature
// verification and must not reveal anything beyond the CORS decision.
func (app *S3RP) handlePreflight(w http.ResponseWriter, r *http.Request) error {
	origin := r.Header.Get("Origin")
	method := r.Header.Get("Access-Control-Request-Method")
	if origin == "" || method == "" {
		return s3err.New(http.StatusBadRequest, "BadRequest",
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
		return s3err.New(http.StatusInternalServerError, "InternalError", "bucket lookup failed")
	}
	if !cors.AllowPreflight(w, r, b.CORS) {
		return corsNotAllowed()
	}
	return nil
}

func corsNotAllowed() *s3err.Error {
	return s3err.New(http.StatusForbidden, "AccessForbidden",
		"CORSResponse: This CORS request is not allowed.")
}

// getBucketCors returns the CORS configuration of the bucket as XML.
func (app *S3RP) getBucketCors(c *opCtx) error {
	w, rt := c.w, c.rt
	if len(rt.cfg.CORS) == 0 {
		return s3err.New(http.StatusNotFound, "NoSuchCORSConfiguration",
			"The CORS configuration does not exist")
	}
	result := &s3xml.CORSConfiguration{XMLNS: s3xml.Namespace}
	for _, rule := range rt.cfg.CORS {
		xr := s3xml.CORSRuleXML{
			AllowedOrigin: rule.AllowedOrigins,
			AllowedMethod: rule.AllowedMethods,
			AllowedHeader: rule.AllowedHeaders,
			ExposeHeader:  rule.ExposeHeaders,
			MaxAgeSeconds: rule.MaxAgeSeconds,
		}
		result.Rules = append(result.Rules, xr)
	}
	return s3xml.Write(w, result)
}
