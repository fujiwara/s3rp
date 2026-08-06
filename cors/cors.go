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
	"fmt"
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

// Validate checks a rule's structure: at least one origin and one method,
// only methods the S3 API can actually serve, and a non-negative max age.
// A rule with no origins or methods can never match a request — a dead
// rule its author believes in — and AWS rejects the same mistakes at
// PutBucketCors time, so a store accepting rules for a bucket should run
// this where definitions are written (evaluation deliberately does not
// re-check per request).
func (r *Rule) Validate() error {
	if len(r.AllowedOrigins) == 0 {
		return fmt.Errorf("cors rule requires at least one allowed origin")
	}
	if len(r.AllowedMethods) == 0 {
		return fmt.Errorf("cors rule requires at least one allowed method")
	}
	for _, m := range r.AllowedMethods {
		switch m {
		case "GET", "PUT", "POST", "DELETE", "HEAD":
		default:
			return fmt.Errorf("unsupported cors method %q", m)
		}
	}
	if r.MaxAgeSeconds < 0 {
		return fmt.Errorf("cors max_age_seconds must not be negative")
	}
	return nil
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

// headerAllowed reports whether a requested header matches an AllowedHeaders
// entry. As in AWS, an entry may carry a "*" wildcard ("x-amz-*" allows every
// Amazon-specific header), and matching is case-insensitive because header
// names are.
func headerAllowed(allowed []string, header string) bool {
	header = strings.ToLower(header)
	for _, a := range allowed {
		if policy.Match(strings.ToLower(a), header) {
			return true
		}
	}
	return false
}
