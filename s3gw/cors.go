package s3gw

import (
	"errors"
	"net/http"

	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"

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
func (g *Gateway) handlePreflight(w http.ResponseWriter, r *http.Request) error {
	origin := r.Header.Get("Origin")
	method := r.Header.Get("Access-Control-Request-Method")
	if origin == "" || method == "" {
		return s3err.New(http.StatusBadRequest, "BadRequest",
			"Insufficient information. Origin request header needed.")
	}
	t, err := g.requestTarget(r)
	if err != nil || t.bucket == "" {
		return corsNotAllowed()
	}
	b, err := g.store.GetBucketByName(r.Context(), t.bucket)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return corsNotAllowed()
		}
		return s3err.Internal(err, "bucket lookup failed")
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

func (g *Gateway) getBucketCors(c *opCtx) error {
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
