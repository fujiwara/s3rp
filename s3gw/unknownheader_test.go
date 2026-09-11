package s3gw_test

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/s3gw"
)

// An x-amz-* request header no operation reads is refused with
// NotImplemented rather than ignored — the header-path counterpart of the
// 501 for an unknown query subresource and an unknown POST field.
func TestUnknownAmzHeaderRefused(t *testing.T) {
	tests := []struct {
		name     string
		method   string
		headers  map[string]string
		wantCode string // "" = success
		wantMsg  string
	}{
		{
			name: "website redirect on PUT", method: "PUT",
			headers:  map[string]string{"x-amz-website-redirect-location": "/elsewhere"},
			wantCode: "NotImplemented", wantMsg: "header x-amz-website-redirect-location",
		},
		{
			// the case that motivates the check: an encryption context
			// the client believes applies
			name: "kms encryption context on PUT", method: "PUT",
			headers: map[string]string{
				"x-amz-server-side-encryption":         "aws:kms",
				"x-amz-server-side-encryption-context": "eyJrIjoidiJ9",
			},
			wantCode: "NotImplemented", wantMsg: "header x-amz-server-side-encryption-context",
		},
		{
			name: "request payer on GET", method: "GET",
			headers:  map[string]string{"x-amz-request-payer": "requester"},
			wantCode: "NotImplemented", wantMsg: "header x-amz-request-payer",
		},
		{
			name: "several unknown, named sorted", method: "PUT",
			headers: map[string]string{
				"x-amz-write-offset-bytes":    "0",
				"x-amz-expected-bucket-owner": "123456789012",
			},
			wantCode: "NotImplemented", wantMsg: "header x-amz-expected-bucket-owner, x-amz-write-offset-bytes",
		},
		{
			// a stray SSE-C key without its algorithm header: not silently
			// dropped either
			name: "sse-c key without algorithm", method: "PUT",
			headers:  map[string]string{"x-amz-server-side-encryption-customer-key": "AAAA"},
			wantCode: "NotImplemented", wantMsg: "header x-amz-server-side-encryption-customer-key",
		},
		{
			// an explicit grant is an ACL write, refused as such
			name: "grant on PUT", method: "PUT",
			headers:  map[string]string{"x-amz-grant-read": "id=someone"},
			wantCode: "AccessControlListNotSupported",
		},
		{
			// what the browser SDKs send
			name: "x-amz-user-agent", method: "GET",
			headers: map[string]string{"x-amz-user-agent": "aws-sdk-js/3.0.0"},
		},
		{
			name: "known headers on a route that ignores them", method: "GET",
			headers: map[string]string{"x-amz-tagging": "a=b", "x-amz-storage-class": "STANDARD"},
		},
		{
			name: "user metadata", method: "PUT",
			headers: map[string]string{"x-amz-meta-anything": "v"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gw := newTestGateway(t)
			stub := &stubBackend{
				getOut: &s3.GetObjectOutput{Body: http.NoBody, ContentLength: aws.Int64(0)},
				putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
			}
			if err := gw.SetBackend("testbucket", stub); err != nil {
				t.Fatal(err)
			}
			req := signedRequest(t, tt.method, "http://s3.example.com/testbucket/a", nil, emptyPayloadHash,
				time.Now(), testCreds(), func(r *http.Request) {
					for k, v := range tt.headers {
						r.Header.Set(k, v)
					}
				})
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, req)
			if tt.wantCode == "" {
				if w.Code != http.StatusOK {
					t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
				}
				return
			}
			body := w.Body.String()
			if !strings.Contains(body, "<Code>"+tt.wantCode+"</Code>") {
				t.Errorf("expect %s, got %d: %s", tt.wantCode, w.Code, body)
			}
			if tt.wantMsg != "" && !strings.Contains(body, tt.wantMsg) {
				t.Errorf("expect message naming %q, got: %s", tt.wantMsg, body)
			}
			if w.Code == http.StatusNotImplemented && stub.putIn != nil {
				t.Error("expect the refusal before the backend is called")
			}
		})
	}
}

// The signature gate comes first: an unknown header that is also unsigned
// is the 403 the gate produces, not a 501.
func TestUnknownAmzHeaderUnsignedIsAccessDenied(t *testing.T) {
	gw := newTestGateway(t)
	if err := gw.SetBackend("testbucket", &stubBackend{}); err != nil {
		t.Fatal(err)
	}
	req := signedRequest(t, "PUT", "http://s3.example.com/testbucket/a", nil, emptyPayloadHash, time.Now(), testCreds(), nil)
	req.Header.Set("x-amz-website-redirect-location", "/elsewhere")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "<Code>AccessDenied</Code>") {
		t.Errorf("expect 403 AccessDenied, got %d: %s", w.Code, w.Body.String())
	}
}

// Through a real SDK client: an option the gateway does not implement fails
// visibly instead of being dropped.
func TestUnknownAmzHeaderThroughSDK(t *testing.T) {
	stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)}}
	client, _, _ := newTestProxyWithGateway(t, stub)
	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
		Body:                    strings.NewReader("hello"),
		WebsiteRedirectLocation: aws.String("/elsewhere"),
	})
	if err == nil || !strings.Contains(err.Error(), "NotImplemented") {
		t.Fatalf("expect NotImplemented, got %v", err)
	}
	if stub.putIn != nil {
		t.Error("expect the refusal before the backend is called")
	}
}

// Every x-amz-* header an operation file reads must be in the allowlist, or
// adding a handler for a new header would leave it refused; and the
// allowlist must not name a header nothing reads, or that header would be
// silently ignored — the very thing the check exists to prevent.
func TestKnownAmzHeadersCoverSource(t *testing.T) {
	// names that appear in the sources but are not request headers the
	// operations read: response headers, and the POST form / presigned
	// query auth fields
	notRequestHeaders := map[string]bool{
		"x-amz-request-id": true, "x-amz-version-id": true, "x-amz-delete-marker": true,
		"x-amz-mp-parts-count": true, "x-amz-tagging-count": true, "x-amz-bucket-region": true,
		"x-amz-copy-source-version-id": true,
		"x-amz-algorithm":              true, "x-amz-credential": true, "x-amz-signature": true,
	}
	// refused by name before the allowlist runs, and deliberately absent
	// from it (see knownAmzHeaders)
	refusedByName := map[string]bool{
		"x-amz-server-side-encryption-customer-algorithm":             true,
		"x-amz-copy-source-server-side-encryption-customer-algorithm": true,
	}
	// in the allowlist without a literal in the sources: read through a
	// shared leaf package, or consumed by the verifier
	readElsewhere := map[string]bool{
		"x-amz-date": true, "x-amz-content-sha256": true, "x-amz-security-token": true,
		"x-amz-decoded-content-length": true, "x-amz-trailer": true, "x-amz-user-agent": true,
		"x-amz-checksum-crc32": true, "x-amz-checksum-crc32c": true, "x-amz-checksum-crc64nvme": true,
		"x-amz-checksum-sha1": true, "x-amz-checksum-sha256": true, "x-amz-sdk-checksum-algorithm": true,
	}
	literal := regexp.MustCompile(`"(x-amz-[a-z0-9-]+)"`)
	inSource := map[string]bool{}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range literal.FindAllStringSubmatch(string(src), -1) {
			name := m[1]
			if strings.HasPrefix(name, "x-amz-meta-") || notRequestHeaders[name] || refusedByName[name] {
				continue
			}
			inSource[name] = true
			if !s3gw.KnownAmzHeaders[name] {
				t.Errorf("%s reads %q, which knownAmzHeaders does not list: it would be refused", f, name)
			}
		}
	}
	for name := range s3gw.KnownAmzHeaders {
		if !inSource[name] && !readElsewhere[name] {
			t.Errorf("knownAmzHeaders lists %q, which no operation file reads: it would be silently ignored", name)
		}
	}
}
