package sigv4_test

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/fujiwara/s3rp/sigv4"
)

// Fuzz targets for SigV4 request verification. The signature math is the
// SDK's own signer on both sides, so these do not test HMAC arithmetic —
// they test everything around it, which is where the verifier's own code
// runs: request-clone construction (escaping preservation, header
// selection, scope parsing) and the parsers that consume attacker bytes
// before any signature is checked. Two directions:
//
//   - Completeness: whatever the real SDK signer signs, the verifier must
//     accept — a refusal is a legitimate client locked out.
//   - Soundness: mutating any signed material of an accepted request must
//     be refused — an acceptance is a fail-open.

// fuzzPath builds a valid request path from arbitrary bytes: unreserved
// bytes and "/" stay literal (so "//" and "." segments arise), a NUL is
// the fuzzer's spelling for an encoded slash, and everything else is
// percent-encoded.
func fuzzPath(raw []byte) string {
	var sb strings.Builder
	sb.WriteByte('/')
	for _, b := range raw {
		switch {
		case b == 0:
			sb.WriteString("%2F")
		case b == '/' || b == '-' || b == '.' || b == '_' || b == '~',
			'a' <= b && b <= 'z', 'A' <= b && b <= 'Z', '0' <= b && b <= '9':
			sb.WriteByte(b)
		default:
			fmt.Fprintf(&sb, "%%%02X", b)
		}
	}
	return sb.String()
}

func keepBytes(s string, keep func(byte) bool, cap int) string {
	var sb strings.Builder
	for i := 0; i < len(s) && sb.Len() < cap; i++ {
		if keep(s[i]) {
			sb.WriteByte(s[i])
		}
	}
	return sb.String()
}

func fuzzRegion(s string) string {
	r := keepBytes(s, func(b byte) bool {
		return b == '-' || 'a' <= b && b <= 'z' || '0' <= b && b <= '9'
	}, 32)
	if r == "" {
		return "us-east-1"
	}
	return r
}

func fuzzHeaderValue(s string) string {
	return keepBytes(s, func(b byte) bool { return 0x20 <= b && b <= 0x7e }, 256)
}

// fuzzHeaderName yields a header name the request may freely carry, or ""
// when the input reduces to nothing usable. Names the signer, the
// transport or the verifier treat specially are excluded — the point is
// arbitrary application headers, not colliding with the protocol.
func fuzzHeaderName(s string) string {
	n := keepBytes(s, func(b byte) bool {
		return b == '-' || 'a' <= b && b <= 'z' || '0' <= b && b <= '9'
	}, 40)
	n = strings.Trim(n, "-")
	switch n {
	case "", "host", "authorization", "content-length", "expect",
		"transfer-encoding", "connection", "trailer", "te", "upgrade",
		"user-agent", "x-amz-date", "x-amz-content-sha256", "x-amz-security-token":
		return ""
	}
	return n
}

// signServerRequest signs a client request with the SDK signer and rebuilds
// it as the server would see it. Returns nil when the driver-generated
// input does not survive URL parsing — a driver artifact, not a finding.
func signServerRequest(t *testing.T, method, pathAndQuery string, headers http.Header, payloadHash, region, token string) *http.Request {
	t.Helper()
	if _, err := url.ParseRequestURI(pathAndQuery); err != nil {
		return nil
	}
	urlStr := "http://s3.example.com" + pathAndQuery
	req, err := http.NewRequest(method, urlStr, nil)
	if err != nil {
		return nil
	}
	if req.URL.RequestURI() != pathAndQuery {
		// the URL does not round-trip; signing it would not represent what
		// a client sends on the wire
		return nil
	}
	maps.Copy(req.Header, headers)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	if token != "" {
		req.Header.Set("X-Amz-Security-Token", token)
	}
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecret, SessionToken: token}
	if err := signer.SignHTTP(context.Background(), creds, req, payloadHash, "s3", region, testTime); err != nil {
		return nil
	}
	sr := httptest.NewRequest(method, urlStr, nil)
	sr.Header = req.Header
	sr.Host = req.Host
	return sr
}

func cloneServerRequest(sr *http.Request, method, pathAndQuery string) *http.Request {
	if _, err := url.ParseRequestURI(pathAndQuery); err != nil {
		return nil
	}
	defer func() { recover() }() // httptest.NewRequest panics on targets it cannot parse
	c := httptest.NewRequest(method, "http://s3.example.com"+pathAndQuery, nil)
	c.Header = make(http.Header, len(sr.Header))
	for k, vs := range sr.Header {
		c.Header[k] = append([]string(nil), vs...)
	}
	c.Host = sr.Host
	return c
}

func flipBit(s string, pos int, bit uint8) string {
	if len(s) == 0 {
		return s
	}
	p := pos % len(s)
	if p < 0 {
		p += len(s)
	}
	b := []byte(s)
	b[p] ^= 1 << (bit % 8)
	return string(b)
}

// flipHeaderPart flips one bit inside the given substring of a header
// value, e.g. within the Signature= field of Authorization. Reports false
// when the marker is absent.
func flipHeaderPart(h http.Header, name, marker string, pos int, bit uint8) bool {
	v := h.Get(name)
	i := strings.Index(v, marker)
	if i < 0 {
		return false
	}
	rest := v[i+len(marker):]
	if end := strings.IndexByte(rest, ','); end >= 0 {
		rest = rest[:end]
	}
	if rest == "" {
		return false
	}
	mutated := v[:i+len(marker)] + flipBit(rest, pos, bit) + v[i+len(marker)+len(rest):]
	h.Set(name, mutated)
	return true
}

// sameRequestURI reports whether two request URIs name the same request as
// the gateway executes it: identical raw path bytes (%2F in a key is not a
// separator, so the path is compared unescaped-as-sent) and an identical
// decoded query. SigV4 canonicalizes the query by re-encoding it, so wire
// forms that decode identically — %2A vs %2a — carry the same signature,
// and accepting one for the other is correct, not a bypass.
func sameRequestURI(a, b string) bool {
	ap, aq, _ := strings.Cut(a, "?")
	bp, bq, _ := strings.Cut(b, "?")
	if ap != bp {
		return false
	}
	av, err1 := url.ParseQuery(aq)
	bv, err2 := url.ParseQuery(bq)
	return err1 == nil && err2 == nil && reflect.DeepEqual(av, bv)
}

func fuzzLookup(token string) sigv4.SecretLookup {
	return func(_ context.Context, accessKeyID, _ string) (sigv4.Credential, error) {
		if accessKeyID != testAccessKeyID {
			return sigv4.Credential{}, sigv4.ErrUnknownKey
		}
		return sigv4.Credential{SecretAccessKey: testSecret, SessionToken: token}, nil
	}
}

var fuzzMethods = []string{"GET", "PUT", "POST", "DELETE", "HEAD"}

func FuzzVerifyHeaderRoundtrip(f *testing.F) {
	f.Add([]byte("bucket/key.txt"), "prefix", "photos/", "x-amz-meta-note", "hello world", "us-east-1", "", uint8(0), uint8(0), 0, uint8(0))
	f.Add([]byte("b/a//b\x00c"), "list-type", "2", "cache-control", "  spaces  collapse  ", "ap-northeast-1", "FQoGZXIvYXdzEBYaD", uint8(1), uint8(1), 5, uint8(3))
	f.Add([]byte("b/\xe3\x81\x82"), "", "", "", "", "eu-west-1", "", uint8(2), uint8(5), -3, uint8(7))

	f.Fuzz(func(t *testing.T, rawPath []byte, qKey, qVal, hName, hVal, rawRegion, rawToken string, sel, mutSel uint8, mutPos int, mutBit uint8) {
		if len(rawPath) > 512 {
			rawPath = rawPath[:512]
		}
		method := fuzzMethods[int(sel)%len(fuzzMethods)]
		pq := fuzzPath(rawPath)
		if qKey != "" {
			k := url.QueryEscape(qKey)
			if strings.HasPrefix(strings.ToLower(k), "x-amz") {
				k = "q" + k
			}
			pq += "?" + k + "=" + url.QueryEscape(qVal)
			if sel&0x40 != 0 {
				pq += "&" + k + "=" + url.QueryEscape(qVal) // repeated key
			}
		}
		headers := http.Header{}
		name := fuzzHeaderName(hName)
		if name != "" {
			headers.Set(name, fuzzHeaderValue(hVal))
			if sel&0x80 != 0 {
				headers.Add(name, fuzzHeaderValue(hVal)+"2") // repeated header
			}
		}
		payloadHash := emptyPayload
		if sel&0x20 != 0 {
			payloadHash = "UNSIGNED-PAYLOAD"
		}
		region := fuzzRegion(rawRegion)
		token := keepBytes(rawToken, func(b byte) bool {
			return b == '+' || b == '/' || b == '=' || b == '-' || b == '_' ||
				'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9'
		}, 128)

		sr := signServerRequest(t, method, pq, headers, payloadHash, region, token)
		if sr == nil {
			return
		}
		lookup := fuzzLookup(token)

		// completeness: what the SDK signed must verify
		got, verr := newVerifier().Verify(sr, lookup)
		if verr != nil {
			t.Fatalf("SDK-signed request refused: %v (method %s, path %q, region %q)", verr, method, pq, region)
		}
		if got.AccessKeyID != testAccessKeyID || got.Region != region || got.PayloadHash != payloadHash {
			t.Fatalf("verified facts do not match what was signed: %+v", got)
		}

		// soundness: mutate one piece of signed material, must be refused
		mut := cloneServerRequest(sr, method, pq)
		if mut == nil {
			return
		}
		mutated := true
		mutPQ := pq
		switch mutSel % 6 {
		case 0: // request path or query
			mutPQ = flipBit(pq, mutPos, mutBit)
			if mut = cloneServerRequest(sr, method, mutPQ); mut == nil {
				return // unparseable after the flip: refused before verification
			}
		case 1:
			mutated = flipHeaderPart(mut.Header, "Authorization", "Signature=", mutPos, mutBit)
		case 2:
			mutated = flipHeaderPart(mut.Header, "Authorization", "Credential=", mutPos, mutBit)
		case 3:
			target := "X-Amz-Date"
			if name != "" {
				target = name
			}
			v := mut.Header.Get(target)
			if v == "" {
				return
			}
			mut.Header.Set(target, flipBit(v, mutPos, mutBit))
		case 4:
			mut.Header.Set("X-Amz-Content-Sha256", flipBit(payloadHash, mutPos, mutBit))
		case 5:
			m2 := fuzzMethods[(int(sel)+1)%len(fuzzMethods)]
			if mut = cloneServerRequest(sr, m2, pq); mut == nil {
				return
			}
		}
		if !mutated {
			return
		}
		if _, verr := newVerifier().Verify(mut, lookup); verr == nil {
			// acceptance is a finding unless the mutated URI names the very
			// same request — query escape aliasing under canonicalization
			if !sameRequestURI(pq, mutPQ) {
				t.Fatalf("mutation %d accepted (method %s, path %q -> %q)", mutSel%6, method, pq, mutPQ)
			}
		}
	})
}

func FuzzVerifyPresignRoundtrip(f *testing.F) {
	f.Add([]byte("bucket/key.txt"), "response-content-type", "text/plain", "us-east-1", "", uint32(300), 0, uint8(0))
	f.Add([]byte("b/a//\x00z"), "", "", "ap-northeast-1", "FQoGZXIvYXdzEBYaD", uint32(604800), 9, uint8(2))

	f.Fuzz(func(t *testing.T, rawPath []byte, qKey, qVal, rawRegion, rawToken string, expires uint32, mutPos int, mutBit uint8) {
		if len(rawPath) > 512 {
			rawPath = rawPath[:512]
		}
		pq := fuzzPath(rawPath) + fmt.Sprintf("?X-Amz-Expires=%d", expires%604800+1)
		if qKey != "" {
			k := url.QueryEscape(qKey)
			if strings.HasPrefix(strings.ToLower(k), "x-amz") {
				k = "q" + k
			}
			pq += "&" + k + "=" + url.QueryEscape(qVal)
		}
		if _, err := url.ParseRequestURI(pq); err != nil {
			return
		}
		region := fuzzRegion(rawRegion)
		token := keepBytes(rawToken, func(b byte) bool {
			return b == '+' || b == '/' || b == '=' ||
				'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9'
		}, 128)
		req, err := http.NewRequest("GET", "http://s3.example.com"+pq, nil)
		if err != nil || req.URL.RequestURI() != pq {
			return
		}
		signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
		creds := aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecret, SessionToken: token}
		signedURI, _, err := signer.PresignHTTP(context.Background(), creds, req, "UNSIGNED-PAYLOAD", "s3", region, testTime)
		if err != nil {
			return
		}
		lookup := fuzzLookup(token)
		su, err := url.Parse(signedURI)
		if err != nil {
			return
		}

		sr := httptest.NewRequest("GET", signedURI, nil)
		sr.Host = req.Host
		got, verr := newVerifier().Verify(sr, lookup)
		if verr != nil {
			t.Fatalf("SDK-presigned URL refused: %v (path %q, region %q)", verr, pq, region)
		}
		if got.AccessKeyID != testAccessKeyID || got.Region != region {
			t.Fatalf("verified facts do not match what was presigned: %+v", got)
		}

		// soundness: everything in a presigned URL's path and query except
		// the signature itself is signed, and the signature is the check —
		// so a bit flipped anywhere must be refused (or fail to parse)
		mpq := flipBit(su.RequestURI(), mutPos, mutBit)
		if _, err := url.ParseRequestURI(mpq); err != nil || !strings.HasPrefix(mpq, "/") {
			return
		}
		mut := cloneServerRequest(sr, "GET", mpq)
		if mut == nil {
			return
		}
		if _, verr := newVerifier().Verify(mut, lookup); verr == nil {
			if !sameRequestURI(su.RequestURI(), mpq) {
				t.Fatalf("presigned URL with bit %d of byte %d flipped accepted (path %q)", mutBit%8, mutPos, pq)
			}
		}
	})
}

// FuzzVerifyAdversarial feeds raw attacker-shaped requests to Verify: no
// input constructed without the secret may verify, and none may panic.
func FuzzVerifyAdversarial(f *testing.F) {
	f.Add("AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/20260801/us-east-1/s3/aws4_request, SignedHeaders=host;x-amz-date, Signature=abc123",
		"20260801T120000Z", emptyPayload, "", "")
	f.Add("", "", "", "X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKIDEXAMPLE%2F20260801%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Date=20260801T120000Z&X-Amz-Expires=300&X-Amz-SignedHeaders=host&X-Amz-Signature=deadbeef", "")
	f.Add("Basic dXNlcjpwYXNz", "not-a-date", "UNSIGNED-PAYLOAD", "X-Amz-Signature=00", "sometoken")

	f.Fuzz(func(t *testing.T, auth, amzDate, sha, rawQuery, token string) {
		pq := "/bucket/key.txt"
		if q := fuzzHeaderValue(rawQuery); q != "" {
			pq += "?" + q
		}
		if _, err := url.ParseRequestURI(pq); err != nil {
			return
		}
		mkReq := func() (r *http.Request) {
			defer func() { recover() }()
			return httptest.NewRequest("GET", "http://s3.example.com"+pq, nil)
		}
		sr := mkReq()
		if sr == nil {
			return
		}
		if v := fuzzHeaderValue(auth); v != "" {
			sr.Header.Set("Authorization", v)
		}
		if v := fuzzHeaderValue(amzDate); v != "" {
			sr.Header.Set("X-Amz-Date", v)
		}
		if v := fuzzHeaderValue(sha); v != "" {
			sr.Header.Set("X-Amz-Content-Sha256", v)
		}
		if v := fuzzHeaderValue(token); v != "" {
			sr.Header.Set("X-Amz-Security-Token", v)
		}
		if got, verr := newVerifier().Verify(sr, fuzzLookup("")); verr == nil {
			t.Fatalf("request without the secret verified as %+v", got)
		}
	})
}
