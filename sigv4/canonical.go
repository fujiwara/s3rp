package sigv4

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The canonical request is built here rather than by re-signing a clone with
// aws-sdk-go-v2's v4.Signer, which the verifier used to do. That signer is a
// client: it decides what to sign, so it cannot reproduce every request
// another client signed. The case that forced the change is content-length —
// the SDK signs it only for a positive Request.ContentLength and drops a
// Content-Length header it is handed, so a client signing "content-length:0"
// (the CRT transfer client of aws-cli ≥ 2.38 does on every bodiless request)
// could never verify. Built from the request as sent, every signed header is
// canonicalized from its own value.
//
// The URI path and query follow aws-sdk-go-v2 exactly (the query is decoded
// and re-encoded, see docs/sigv4-canonicalization.md), so every request the
// SDK signs still canonicalizes to the same bytes; the differential fuzz
// target holds the two together.

// errSignedHeaderMissing reports a header named in SignedHeaders that the
// request does not carry; the caller answers InvalidRequest.
type errSignedHeaderMissing string

func (e errSignedHeaderMissing) Error() string {
	return fmt.Sprintf("signed header %q is not present in the request", string(e))
}

// checkSignedHeaderList enforces the shape SigV4 gives SignedHeaders:
// lower-case, strictly ascending (so no duplicates), and including host.
// The signature is computed over the list as the client wrote it, so a list
// out of order cannot describe a canonical request the client built.
func checkSignedHeaderList(names []string) error {
	hasHost := false
	for i, n := range names {
		if n == "" || n != strings.ToLower(n) {
			return fmt.Errorf("signed header %q is not lower-case", n)
		}
		if i > 0 && names[i-1] >= n {
			return fmt.Errorf("signed headers are not sorted and unique at %q", n)
		}
		switch n {
		case "host":
			hasHost = true
		case "authorization":
			return fmt.Errorf("the authorization header cannot be signed")
		}
	}
	if !hasHost {
		return fmt.Errorf("host is not a signed header")
	}
	return nil
}

// canonicalRequest builds the SigV4 canonical request for an inbound
// request. signedHeaders must have passed checkSignedHeaderList; dropQuery
// names the query parameter carrying the signature itself (presigned), which
// the canonical query leaves out.
func canonicalRequest(r *http.Request, signedHeaders []string, dropQuery, payloadHash string) (string, error) {
	u, err := url.ParseRequestURI(r.RequestURI)
	if err != nil {
		// not from a server (e.g. tests); fall back to the parsed URL
		u = r.URL
	}
	var b strings.Builder
	b.WriteString(r.Method)
	b.WriteByte('\n')
	b.WriteString(canonicalURI(u))
	b.WriteByte('\n')
	b.WriteString(canonicalQuery(u, dropQuery))
	b.WriteByte('\n')
	for _, name := range signedHeaders {
		v, err := canonicalHeaderValue(r, name)
		if err != nil {
			return "", err
		}
		b.WriteString(name)
		b.WriteByte(':')
		b.WriteString(v)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(strings.Join(signedHeaders, ";"))
	b.WriteByte('\n')
	b.WriteString(payloadHash)
	return b.String(), nil
}

// canonicalURI is the path exactly as sent: S3 signs it without the extra
// escaping pass other services apply, so a key containing "/" or "%2F" keeps
// its wire form.
func canonicalURI(u *url.URL) string {
	var p string
	if u.Opaque != "" {
		// "scheme://host/path" carried in Opaque: the path starts at the
		// first "/" after the authority
		opaque, _, _ := strings.Cut(u.Opaque, "?")
		opaque = strings.TrimPrefix(opaque, "//")
		if i := strings.IndexByte(opaque, '/'); i >= 0 {
			p = opaque[i:]
		}
	} else {
		p = u.EscapedPath()
	}
	if p == "" {
		return "/"
	}
	return p
}

// canonicalQuery decodes the query and re-encodes it sorted by key, then by
// value, with spaces as %20. Pairs that fail to decode are dropped, as
// url.URL.Query drops them.
func canonicalQuery(u *url.URL, drop string) string {
	q := u.Query()
	if drop != "" {
		q.Del(drop)
	}
	for k := range q {
		sort.Strings(q[k])
	}
	return strings.ReplaceAll(q.Encode(), "+", "%20")
}

// canonicalHeaderValue is a signed header's canonical value: its values
// joined by commas, each trimmed and with runs of spaces collapsed.
func canonicalHeaderValue(r *http.Request, name string) (string, error) {
	var values []string
	switch name {
	case "host":
		h := r.Host
		if h == "" {
			h = r.URL.Host
		}
		values = []string{h}
	case "content-length":
		values = r.Header["Content-Length"]
		if len(values) == 0 && r.ContentLength > 0 {
			// a request built in code (tests) may carry its length in
			// the field only; a server request with a body has the header
			values = []string{strconv.FormatInt(r.ContentLength, 10)}
		}
	default:
		values = r.Header[http.CanonicalHeaderKey(name)]
	}
	if len(values) == 0 {
		return "", errSignedHeaderMissing(name)
	}
	var b strings.Builder
	for i, v := range values {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(collapseSpaces(v))
	}
	return b.String(), nil
}

// collapseSpaces trims surrounding whitespace and collapses each run of
// spaces inside the value to one.
func collapseSpaces(v string) string {
	v = strings.TrimSpace(v)
	if !strings.Contains(v, "  ") {
		return v
	}
	var b strings.Builder
	b.Grow(len(v))
	prevSpace := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		if c == ' ' && prevSpace {
			continue
		}
		prevSpace = c == ' '
		b.WriteByte(c)
	}
	return b.String()
}

// signature computes the request signature over a canonical request.
func signature(signingKey []byte, t time.Time, scope, canonical string) string {
	h := sha256.Sum256([]byte(canonical))
	sts := sigV4Algorithm + "\n" + t.UTC().Format(amzDateFormat) + "\n" + scope + "\n" + hex.EncodeToString(h[:])
	return hex.EncodeToString(hmacSHA256(signingKey, []byte(sts)))
}
