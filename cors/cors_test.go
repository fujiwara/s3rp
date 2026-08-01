package cors_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/fujiwara/s3rp/cors"
)

var rules = []*cors.Rule{
	{
		AllowedOrigins: []string{"https://app.example.com", "https://*.preview.example.com"},
		AllowedMethods: []string{"GET", "PUT"},
		AllowedHeaders: []string{"content-type", "x-amz-*"},
		ExposeHeaders:  []string{"ETag"},
		MaxAgeSeconds:  3600,
	},
	{
		AllowedOrigins: []string{"*"},
		AllowedMethods: []string{"HEAD"},
	},
}

func TestMatch(t *testing.T) {
	cases := []struct {
		name           string
		origin, method string
		want           bool
	}{
		{"exact origin and method", "https://app.example.com", "GET", true},
		{"wildcard origin", "https://pr-42.preview.example.com", "PUT", true},
		{"wildcard does not cross into another host", "https://evil.com", "GET", false},
		{"origin matches but method does not", "https://app.example.com", "DELETE", false},
		{"any origin via the second rule", "https://somewhere.else", "HEAD", true},
		{"method only allowed by the wildcard rule", "https://app.example.com", "HEAD", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := cors.Match(rules, tc.origin, tc.method) != nil; got != tc.want {
				t.Errorf("Match(%q, %q) = %v, want %v", tc.origin, tc.method, got, tc.want)
			}
		})
	}
}

func preflight(origin, method, headers string) (*httptest.ResponseRecorder, bool) {
	r := httptest.NewRequest("OPTIONS", "http://s3.example.com/bucket/key", nil)
	r.Header.Set("Origin", origin)
	r.Header.Set("Access-Control-Request-Method", method)
	if headers != "" {
		r.Header.Set("Access-Control-Request-Headers", headers)
	}
	w := httptest.NewRecorder()
	return w, cors.AllowPreflight(w, r, rules)
}

func TestAllowPreflight(t *testing.T) {
	t.Run("concrete origin is echoed with credentials", func(t *testing.T) {
		w, ok := preflight("https://app.example.com", "PUT", "")
		if !ok {
			t.Fatal("expect the preflight to be allowed")
		}
		h := w.Header()
		if got := h.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
			t.Errorf("unexpected allow-origin %q", got)
		}
		if h.Get("Access-Control-Allow-Credentials") != "true" {
			t.Error("a concrete origin should allow credentials")
		}
		if got := h.Get("Access-Control-Allow-Methods"); got != "GET, PUT" {
			t.Errorf("unexpected allow-methods %q", got)
		}
		if got := h.Get("Access-Control-Max-Age"); got != "3600" {
			t.Errorf("unexpected max-age %q", got)
		}
		if h.Get("Vary") == "" {
			t.Error("expect a Vary header")
		}
		if w.Code != http.StatusOK {
			t.Errorf("expect 200, got %d", w.Code)
		}
	})

	// "*" and credentials are mutually exclusive in browsers
	t.Run("wildcard origin without credentials", func(t *testing.T) {
		w, ok := preflight("https://somewhere.else", "HEAD", "")
		if !ok {
			t.Fatal("expect the preflight to be allowed")
		}
		if got := w.Header().Get("Access-Control-Allow-Origin"); got != "*" {
			t.Errorf("unexpected allow-origin %q", got)
		}
		if w.Header().Get("Access-Control-Allow-Credentials") != "" {
			t.Error("credentials must not be allowed alongside *")
		}
	})

	t.Run("requested headers", func(t *testing.T) {
		cases := []struct {
			name    string
			headers string
			want    bool
		}{
			{"allowed exactly", "content-type", true},
			{"case-insensitive", "Content-Type", true},
			{"wildcard entry", "x-amz-meta-foo", true},
			{"wildcard entry, mis-cased", "X-Amz-Meta-Foo", true},
			{"not allowed", "x-custom-header", false},
			{"one of several not allowed", "content-type, x-custom-header", false},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				w, ok := preflight("https://app.example.com", "PUT", tc.headers)
				if ok != tc.want {
					t.Fatalf("AllowPreflight(%q) = %v, want %v", tc.headers, ok, tc.want)
				}
				if !ok {
					// a refusal must not have written anything: the caller
					// still owns the response
					if len(w.Header()) != 0 {
						t.Errorf("expect no headers written on refusal, got %v", w.Header())
					}
					return
				}
				if got := w.Header().Get("Access-Control-Allow-Headers"); got != tc.headers {
					t.Errorf("unexpected allow-headers %q", got)
				}
			})
		}
	})

	t.Run("no matching rule writes nothing", func(t *testing.T) {
		w, ok := preflight("https://evil.com", "DELETE", "")
		if ok {
			t.Fatal("expect the preflight to be refused")
		}
		if len(w.Header()) != 0 {
			t.Errorf("expect no headers written, got %v", w.Header())
		}
	})
}

func TestSetHeaders(t *testing.T) {
	do := func(origin, method string) http.Header {
		r := httptest.NewRequest(method, "http://s3.example.com/bucket/key", nil)
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		w := httptest.NewRecorder()
		cors.SetHeaders(w, r, rules)
		return w.Header()
	}

	h := do("https://app.example.com", "GET")
	if got := h.Get("Access-Control-Allow-Origin"); got != "https://app.example.com" {
		t.Errorf("unexpected allow-origin %q", got)
	}
	if got := h.Get("Access-Control-Expose-Headers"); got != "ETag" {
		t.Errorf("unexpected expose-headers %q", got)
	}
	if h.Get("Vary") != "Origin" {
		t.Errorf("unexpected vary %q", h.Get("Vary"))
	}

	// an ordinary request simply gets no CORS headers
	if got := do("", "GET"); len(got) != 0 {
		t.Errorf("expect no headers without an Origin, got %v", got)
	}
	if got := do("https://evil.com", "GET"); len(got) != 0 {
		t.Errorf("expect no headers for an unmatched origin, got %v", got)
	}
}
