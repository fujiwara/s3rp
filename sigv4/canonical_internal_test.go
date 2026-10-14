package sigv4

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestCanonicalRequestAWSSuite checks the canonicalization against AWS's
// SigV4 test suite (aws4_testsuite, as carried by botocore): known answers
// from AWS itself rather than agreement with another implementation. The
// suite signs for service "service", so the key is derived here; vectors
// whose path the suite escapes a second time (non-S3 services do) are left
// out, since S3 signs the path as sent.
func TestCanonicalRequestAWSSuite(t *testing.T) {
	const (
		secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
		date   = "20150830T123600Z"
		scope  = "20150830/us-east-1/service/aws4_request"
	)
	cases := []struct {
		name, method, uri string
		headers           [][2]string
		body              string
		signedHeaders     string
		signature         string
	}{
		{"get-vanilla", "GET", "/", nil, "",
			"host;x-amz-date", "5fa00fa31553b73ebf1942676e86291e8372ff2a2260956d9b8aae1d763fbf31"},
		{"get-header-value-trim", "GET", "/", [][2]string{{"My-Header1", "value1"}, {"My-Header2", `"a   b   c"`}}, "",
			"host;my-header1;my-header2;x-amz-date", "acc3ed3afb60bb290fc8d2dd0098b9911fcaa05412b367055dee359757a9c736"},
		{"get-header-key-duplicate", "GET", "/", [][2]string{{"My-Header1", "value2"}, {"My-Header1", "value2"}, {"My-Header1", "value1"}}, "",
			"host;my-header1;x-amz-date", "c9d5ea9f3f72853aea855b47ea873832890dbdd183b4468f858259531a5138ea"},
		{"get-header-value-order", "GET", "/", [][2]string{{"My-Header1", "value4"}, {"My-Header1", "value1"}, {"My-Header1", "value3"}, {"My-Header1", "value2"}}, "",
			"host;my-header1;x-amz-date", "08c7e5a9acfcfeb3ab6b2185e75ce8b1deb5e634ec47601a50643f830c755c01"},
		{"get-vanilla-query-order-key-case", "GET", "/?Param2=value2&Param1=value1", nil, "",
			"host;x-amz-date", "b97d918cfa904a5beff61c982a1b6f458b799221646efd99d3219ec94cdf2500"},
		{"get-vanilla-query-unreserved", "GET", "/?-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz=-._~0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz", nil, "",
			"host;x-amz-date", "9c3e54bfcdf0b19771a7f523ee5669cdf59bc7cc0884027167c21bb143a40197"},
		{"post-header-key-sort", "POST", "/", [][2]string{{"My-Header1", "value1"}}, "",
			"host;my-header1;x-amz-date", "c5410059b04c1ee005303aed430f6e6645f61f4dc9e1461ec8f8916fdf18852c"},
		{"post-vanilla-query", "POST", "/?Param1=value1", nil, "",
			"host;x-amz-date", "28038455d6de14eafc1f9222cf5aa6f1a96197d7deb8263271d420d138af7f11"},
		{"post-x-www-form-urlencoded", "POST", "/", [][2]string{{"Content-Type", "application/x-www-form-urlencoded"}}, "Param1=value1",
			"content-type;host;x-amz-date", "ff11897932ad3f4e8b18135d722051e5ac45fc38421b1da7b9d196a0fe09473a"},
	}
	k := hmacSHA256([]byte("AWS4"+secret), []byte("20150830"))
	k = hmacSHA256(k, []byte("us-east-1"))
	k = hmacSHA256(k, []byte("service"))
	key := hmacSHA256(k, []byte("aws4_request"))
	signingTime, _ := time.Parse(amzDateFormat, date)

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &http.Request{Method: tc.method, RequestURI: tc.uri, Host: "example.amazonaws.com", Header: http.Header{}}
			for _, h := range tc.headers {
				r.Header.Add(h[0], h[1])
			}
			r.Header.Set("X-Amz-Date", date)
			signed := strings.Split(tc.signedHeaders, ";")
			if err := checkSignedHeaderList(signed); err != nil {
				t.Fatal(err)
			}
			payload := sha256.Sum256([]byte(tc.body))
			canonical, err := canonicalRequest(r, signed, "", hex.EncodeToString(payload[:]))
			if err != nil {
				t.Fatal(err)
			}
			if got := signature(key, signingTime, scope, canonical); got != tc.signature {
				t.Errorf("signature %s, want %s\ncanonical request:\n%s", got, tc.signature, canonical)
			}
		})
	}
}

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
