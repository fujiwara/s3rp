package sigv4

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// suitePathAsSent lists the suite's vectors that S3 signs differently, with
// the canonical URI S3 uses instead. The suite normalizes the path and
// escapes it a second time, which every other service does and S3 does not
// (the suite's own normalize-path.txt says so): the path is signed as sent.
var suitePathAsSent = map[string]string{
	"get-relative":            "/example/..",
	"get-relative-relative":   "/example1/example2/../..",
	"get-slash":               "//",
	"get-slash-dot-slash":     "/./",
	"get-slash-pointless-dot": "/./example",
	"get-slashes":             "//example//",
	"get-special-character":   "/example/$delete",
}

// suiteUnsignedToken is the vector that adds X-Amz-Security-Token after
// signing. Its canonical request matches, and the verifier refuses the
// request all the same: S3 requires every x-amz-* header to be signed.
const suiteUnsignedToken = "post-sts-header-after"

// suiteStaleSreq is the vector whose .sreq disagrees with its own .authz:
// the signed request repeats get-vanilla's signature, while .creq, .sts and
// .authz are consistent with one another.
const suiteStaleSreq = "get-vanilla-with-session-token"

// TestAWS4TestSuite replays AWS's SigV4 test suite (testdata/aws4_testsuite)
// against the canonicalization: known answers from AWS itself rather than
// agreement with another implementation. Every vector is accounted for —
// it matches the suite's canonical request and signature, or it is one of
// suitePathAsSent and must differ in the canonical URI alone.
func TestAWS4TestSuite(t *testing.T) {
	const (
		secret = "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"
		date   = "20150830T123600Z"
		scope  = "20150830/us-east-1/service/aws4_request"
	)
	// the suite signs for service "service", so the key is derived here
	k := hmacSHA256([]byte("AWS4"+secret), []byte("20150830"))
	k = hmacSHA256(k, []byte("us-east-1"))
	k = hmacSHA256(k, []byte("service"))
	key := hmacSHA256(k, []byte("aws4_request"))
	signingTime, _ := time.Parse(amzDateFormat, date)

	var files []string
	for _, pattern := range []string{"*/*.sreq", "*/*/*.sreq"} {
		m, err := filepath.Glob(filepath.Join("testdata", "aws4_testsuite", pattern))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, m...)
	}
	if len(files) != 34 {
		t.Fatalf("found %d vectors, want the suite's 34", len(files))
	}
	seen := map[string]bool{}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".sreq")
		seen[name] = true
		t.Run(name, func(t *testing.T) {
			r, body := readSuiteRequest(t, f)
			wantCanonical := readSuiteFile(t, strings.TrimSuffix(f, ".sreq")+".creq")

			// .authz is the answer. The .sreq of suiteStaleSreq carries
			// another vector's signature.
			authz := strings.TrimSpace(readSuiteFile(t, strings.TrimSuffix(f, ".sreq")+".authz"))
			if inRequest := r.Header.Get("Authorization"); (inRequest != authz) != (name == suiteStaleSreq) {
				t.Errorf(".sreq Authorization %q, .authz %q", inRequest, authz)
			}
			_, rest, ok := strings.Cut(authz, "SignedHeaders=")
			signedList, wantSignature, ok2 := strings.Cut(rest, ", Signature=")
			if !ok || !ok2 {
				t.Fatalf("unexpected Authorization %q", authz)
			}
			signed := strings.Split(signedList, ";")
			if err := checkSignedHeaderList(signed); err != nil {
				t.Fatal(err)
			}
			payload := sha256.Sum256([]byte(body))
			canonical, err := canonicalRequest(r, signed, "", hex.EncodeToString(payload[:]))
			if err != nil {
				t.Fatal(err)
			}
			got := signature(key, signingTime, scope, canonical)

			if asSent, ok := suitePathAsSent[name]; ok {
				lines := strings.Split(canonical, "\n")
				wantLines := strings.Split(wantCanonical, "\n")
				if lines[1] != asSent {
					t.Errorf("canonical URI %q, want the path as sent %q", lines[1], asSent)
				}
				if wantLines[1] == asSent {
					t.Fatalf("the suite signs %q as sent; drop it from suitePathAsSent", asSent)
				}
				lines[1] = wantLines[1]
				if strings.Join(lines, "\n") != wantCanonical {
					t.Errorf("canonical request differs beyond the URI\n%s\nwant\n%s", canonical, wantCanonical)
				}
				if got == wantSignature {
					t.Error("signature matches a canonical request with a different URI")
				}
				return
			}
			if canonical != wantCanonical {
				t.Errorf("canonical request\n%s\nwant\n%s", canonical, wantCanonical)
			}
			if got != wantSignature {
				t.Errorf("signature %s, want %s", got, wantSignature)
			}

			refusal := requireSignedAmzHeaders("AKIDEXAMPLE", r.Header, signed)
			if name == suiteUnsignedToken {
				if refusal == nil {
					t.Error("unsigned X-Amz-Security-Token was not refused")
				}
			} else if refusal != nil {
				t.Errorf("refused: %v", refusal)
			}
		})
	}
	for name := range suitePathAsSent {
		if !seen[name] {
			t.Errorf("suitePathAsSent names %q, which the suite does not have", name)
		}
	}
	if !seen[suiteUnsignedToken] {
		t.Errorf("the suite does not have %q", suiteUnsignedToken)
	}
}

// readSuiteRequest parses a .sreq file into the request a server would see.
// The files are not valid HTTP (a raw space or UTF-8 in the request target,
// no space after a header's colon), so net/http cannot read them. A folded
// header line joins the previous one with a single space, as net/textproto
// unfolds it.
func readSuiteRequest(t *testing.T, path string) (*http.Request, string) {
	t.Helper()
	head, body, _ := strings.Cut(readSuiteFile(t, path), "\n\n")
	lines := strings.Split(head, "\n")
	method, target, ok := strings.Cut(lines[0], " ")
	target, ok2 := strings.CutSuffix(target, " HTTP/1.1")
	if !ok || !ok2 {
		t.Fatalf("unexpected request line %q", lines[0])
	}
	r := &http.Request{Method: method, RequestURI: target, Header: http.Header{}}
	var last string
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' {
			values := r.Header[last]
			if len(values) == 0 {
				t.Fatalf("folded line %q follows no header", line)
			}
			values[len(values)-1] += " " + strings.TrimSpace(line)
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			t.Fatalf("unexpected header line %q", line)
		}
		last = http.CanonicalHeaderKey(name)
		if last == "Host" {
			r.Host = strings.TrimSpace(value)
			continue
		}
		r.Header.Add(last, strings.TrimSpace(value))
	}
	return r, body
}

func readSuiteFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}
