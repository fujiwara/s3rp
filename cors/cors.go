// Package cors evaluates S3-style CORS rules and writes the corresponding
// response headers.
//
// CORS is a contract between the browser and the server the browser talks to,
// so a proxy in front of an object store must answer it itself rather than
// pass it through to the backend. This package holds that evaluation; the
// caller supplies the rules (looking them up by bucket is its business) and
// maps a refusal onto whatever error representation it uses.
package cors

import (
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/fujiwara/s3rp/policy"
)

// Rule is one CORS rule of a bucket. AllowedOrigins entries may contain the
// "*" wildcard (e.g. "https://*.example.com"); "*" alone allows any origin.
type Rule struct {
	AllowedOrigins []string `yaml:"allowed_origins" json:"allowed_origins"`
	AllowedMethods []string `yaml:"allowed_methods" json:"allowed_methods"`
	AllowedHeaders []string `yaml:"allowed_headers,omitempty" json:"allowed_headers,omitempty"`
	ExposeHeaders  []string `yaml:"expose_headers,omitempty" json:"expose_headers,omitempty"`
	MaxAgeSeconds  int      `yaml:"max_age_seconds,omitempty" json:"max_age_seconds,omitempty"`
}

// Match returns the first rule allowing the origin and method, or nil.
func Match(rules []*Rule, origin, method string) *Rule {
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

// AllowPreflight evaluates a preflight request against the rules and, when it
// is allowed, writes the preflight response (headers and 200). It reports
// whether the request was allowed; on false nothing has been written, so the
// caller is free to answer with its own error.
//
// The caller is expected to have established that this is a preflight, i.e.
// that Origin and Access-Control-Request-Method are both present.
func AllowPreflight(w http.ResponseWriter, r *http.Request, rules []*Rule) bool {
	origin := r.Header.Get("Origin")
	rule := Match(rules, origin, r.Header.Get("Access-Control-Request-Method"))
	if rule == nil {
		return false
	}
	reqHeaders := r.Header.Get("Access-Control-Request-Headers")
	for h := range strings.SplitSeq(reqHeaders, ",") {
		if h = strings.TrimSpace(h); h == "" {
			continue
		}
		if !headerAllowed(rule.AllowedHeaders, h) {
			return false
		}
	}
	h := w.Header()
	if reqHeaders != "" {
		h.Set("Access-Control-Allow-Headers", reqHeaders)
	}
	setAllowOrigin(h, rule, origin)
	h.Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethods, ", "))
	if rule.MaxAgeSeconds > 0 {
		h.Set("Access-Control-Max-Age", strconv.Itoa(rule.MaxAgeSeconds))
	}
	h.Set("Vary", "Origin, Access-Control-Request-Method, Access-Control-Request-Headers")
	w.WriteHeader(http.StatusOK)
	return true
}

// SetHeaders decorates an actual (non-preflight) response with CORS headers
// when the request carries an Origin matching one of the rules. A request
// without an Origin, or one no rule allows, is left untouched: it is an
// ordinary request that simply gets no CORS headers.
func SetHeaders(w http.ResponseWriter, r *http.Request, rules []*Rule) {
	origin := r.Header.Get("Origin")
	if origin == "" || len(rules) == 0 {
		return
	}
	rule := Match(rules, origin, r.Method)
	if rule == nil {
		return
	}
	h := w.Header()
	setAllowOrigin(h, rule, origin)
	if len(rule.ExposeHeaders) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeaders, ", "))
	}
	h.Set("Vary", "Origin")
}

// setAllowOrigin echoes the request origin unless the rule allows every
// origin, in which case "*" is returned. Credentials are only meaningful with
// a concrete origin: the browser rejects them alongside "*".
func setAllowOrigin(h http.Header, rule *Rule, origin string) {
	if slices.Contains(rule.AllowedOrigins, "*") {
		h.Set("Access-Control-Allow-Origin", "*")
		return
	}
	h.Set("Access-Control-Allow-Origin", origin)
	h.Set("Access-Control-Allow-Credentials", "true")
}

func headerAllowed(allowed []string, header string) bool {
	for _, a := range allowed {
		if a == "*" || strings.EqualFold(a, header) {
			return true
		}
	}
	return false
}
