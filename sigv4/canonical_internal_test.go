package sigv4

import (
	"net/http"
	"net/url"
	"testing"
)

// TestCanonicalRequestSignsAnyListedHeader: the canonical headers are what
// the client listed, with no ignored-header list of the kind a client-side
// signer keeps — a client may sign user-agent, as AWS allows — and
// content-length is signed whatever its value, 0 included.
func TestCanonicalRequestSignsAnyListedHeader(t *testing.T) {
	r := &http.Request{Method: "DELETE", RequestURI: "/bucket/key", Host: "s3.example.com", Header: http.Header{}}
	r.Header.Set("Content-Length", "0")
	r.Header.Set("User-Agent", "aws-cli/2.38.0  crt")
	got, err := canonicalRequest(r, []string{"content-length", "host", "user-agent"}, "", "UNSIGNED-PAYLOAD")
	if err != nil {
		t.Fatal(err)
	}
	want := "DELETE\n/bucket/key\n\ncontent-length:0\nhost:s3.example.com\nuser-agent:aws-cli/2.38.0 crt\n\ncontent-length;host;user-agent\nUNSIGNED-PAYLOAD"
	if got != want {
		t.Errorf("canonical request\n%s\nwant\n%s", got, want)
	}
}

// TestCollapseWhitespace: a run of spaces and tabs becomes one space, as
// Amazon S3 verifies it — measured against S3, which accepts the form
// botocore and the CRT sign ("a\tb" → "a b") and refuses aws-sdk-go-v2's,
// which keeps the tab.
func TestCollapseWhitespace(t *testing.T) {
	for in, want := range map[string]string{
		"plain":           "plain",
		"  a  b  ":        "a b",
		"a\tb":            "a b",
		"a \t b":          "a b",
		"\ta\t\tb\t":      "a b",
		`"a   b   c"`:     `"a b c"`,
		"no-change, here": "no-change, here",
	} {
		if got := collapseWhitespace(in); got != want {
			t.Errorf("collapseWhitespace(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestCanonicalQueryOrder pins the sort to the encoded forms: an escaped
// byte orders as '%', whatever it decodes to.
func TestCanonicalQueryOrder(t *testing.T) {
	cases := []struct{ name, query, want string }{
		{"escaped key before a letter", "X-Amz-Expires=8&%CD=0", "%CD=0&X-Amz-Expires=8"},
		{"escaped value before tilde", "a=~&a=%E3%81%82", "a=%E3%81%82&a=~"},
		{"key before its own prefix extension", "a-=1&a=2", "a=2&a-=1"},
		{"space as %20, signature dropped", "b=x+y&X-Amz-Signature=s&a=1", "a=1&b=x%20y"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := url.Parse("/k?" + c.query)
			if err != nil {
				t.Fatal(err)
			}
			if got := canonicalQuery(u, "X-Amz-Signature"); got != c.want {
				t.Errorf("canonicalQuery(%q) = %q, want %q", c.query, got, c.want)
			}
		})
	}
}
