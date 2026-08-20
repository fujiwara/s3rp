package s3gw_test

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/google/go-cmp/cmp"
)

func TestSignedHeaderSemantics(t *testing.T) {
	r := httptest.NewRequest("PUT", "http://s3.example.com/bucket/key", nil)
	r.Header.Set("x-amz-tagging", "k=v")
	r.Header.Set("x-amz-storage-class", "GLACIER")
	r.Header.Set("x-amz-meta-signed", "yes")
	r.Header.Set("x-amz-meta-unsigned", "no")
	r.Header.Set("Content-Type", "text/plain")
	r.Header.Add("x-amz-object-attributes", "ETag")
	r.Header.Add("x-amz-object-attributes", "ObjectSize")
	signed := map[string]bool{
		"x-amz-tagging":           true,
		"x-amz-meta-signed":       true,
		"x-amz-object-attributes": true,
	}
	hdr := s3gw.NewSignedHeader(r, signed)

	if got := hdr.Signed("x-amz-tagging"); got != "k=v" {
		t.Errorf("Signed must return a covered header, got %q", got)
	}
	if got := hdr.Signed("X-Amz-Tagging"); got != "k=v" {
		t.Errorf("Signed must be case-insensitive, got %q", got)
	}
	if got := hdr.Signed("x-amz-storage-class"); got != "" {
		t.Errorf("Signed must not return an uncovered header, got %q", got)
	}
	if got := hdr.Attribute("Content-Type"); got != "text/plain" {
		t.Errorf("Attribute must return the value signed or not, got %q", got)
	}
	if got := hdr.Attribute("x-amz-storage-class"); got != "GLACIER" {
		t.Errorf("Attribute must see an uncovered header (refusal checks depend on it), got %q", got)
	}
	if diff := cmp.Diff(map[string]string{"signed": "yes"}, hdr.AmzMeta()); diff != "" {
		t.Errorf("AmzMeta must include only covered metadata (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]string{"ETag", "ObjectSize"}, hdr.SignedValues("x-amz-object-attributes")); diff != "" {
		t.Errorf("SignedValues mismatch (-want +got):\n%s", diff)
	}
	if got := hdr.SignedValues("x-amz-meta-unsigned"); got != nil {
		t.Errorf("SignedValues must not return an uncovered header, got %v", got)
	}

	// nil coverage set (POST policy uploads): nothing is signature-covered,
	// but attributes still read
	post := s3gw.NewSignedHeader(r, nil)
	if got := post.Signed("x-amz-tagging"); got != "" {
		t.Errorf("a nil set must cover nothing, got %q", got)
	}
	if post.Attribute("x-amz-tagging") != "k=v" || post.Attribute("Content-Type") != "text/plain" {
		t.Error("Attribute must still work with a nil set")
	}
}

// TestNoDirectRequestHeaderReads is the tripwire behind the signedHeader
// discipline: operation code must read request headers through the accessor,
// which records whether a value may carry semantics beyond the object's own
// attributes. It scans the package source for reads of the conventional
// request variable's headers, and for x-amz-* names read through Attribute —
// those must go through Signed; the refusal checks that deliberately see
// unsigned values (checkSSEC, checkACLHeader) live in sse.go and acl.go.
// Renamed variables and names behind constants slip past this — it is a
// tripwire for the honest mistake, not a proof; review guards the rest.
// cors.go is exempt (unauthenticated preflight, runs before signature
// verification, so no coverage set exists).
func TestNoDirectRequestHeaderReads(t *testing.T) {
	banned := []string{
		"r.Header.Get(",
		"r.Header.Values(",
		"r.Header[",
		"range r.Header",
	}
	bannedAmzAttr := []string{
		`attr("x-amz`,
		`Attribute("x-amz`,
		`AttributeValues("x-amz`,
	}
	exempt := map[string]bool{"cors.go": true}
	amzAttrExempt := map[string]bool{"sse.go": true, "acl.go": true}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || exempt[filepath.Base(f)] {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(src), "\n") {
			for _, tok := range banned {
				if strings.Contains(line, tok) {
					t.Errorf("%s:%d: direct request-header read %q — go through signedHeader (Signed/Attribute)", f, i+1, tok)
				}
			}
			if amzAttrExempt[filepath.Base(f)] {
				continue
			}
			for _, tok := range bannedAmzAttr {
				if strings.Contains(strings.ToLower(line), strings.ToLower(tok)) {
					t.Errorf("%s:%d: x-amz-* header read via Attribute %q — x-amz values carry semantics and must be read via Signed (refusal checks belong in sse.go/acl.go)", f, i+1, tok)
				}
			}
		}
	}
}

// An SSE-C refusal must fire even for a header outside the signature's
// coverage: SSE-C is refused because dropping it silently would store the
// object unprotected, and an unsigned copy of the header is exactly as
// dangerous as a signed one. Guards checkSSEC reading via Attribute, not
// Signed.
func TestSSECRefusedOnPOST(t *testing.T) {
	r := httptest.NewRequest("PUT", "http://s3.example.com/bucket/key", nil)
	r.Header.Set("x-amz-server-side-encryption-customer-algorithm", "AES256")
	hdr := s3gw.NewSignedHeader(r, nil) // nothing covered, header still present
	if err := s3gw.CheckSSEC(hdr); err == nil {
		t.Fatal("SSE-C must be refused even when the header is not signature-covered")
	}
}
