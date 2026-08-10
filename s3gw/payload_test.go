package s3gw_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

func TestPayloadVerifier(t *testing.T) {
	body := []byte("hello world payload")
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	t.Run("matching hash passes", func(t *testing.T) {
		r := s3gw.NewPayloadVerifier(bytes.NewReader(body), want, int64(len(body)))
		got, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Errorf("body altered: %q", got)
		}
	})

	t.Run("tampered body is rejected", func(t *testing.T) {
		tampered := append([]byte(nil), body...)
		tampered[0] = 'H'
		r := s3gw.NewPayloadVerifier(bytes.NewReader(tampered), want, int64(len(tampered)))
		_, err := io.ReadAll(r)
		assertS3Code(t, err, "XAmzContentSHA256Mismatch")
	})

	// a mis-cased but valid hash must still be verified, not silently skipped
	t.Run("uppercase hash is accepted", func(t *testing.T) {
		r := s3gw.NewPayloadVerifier(bytes.NewReader(body), strings.ToUpper(want), int64(len(body)))
		if _, err := io.ReadAll(r); err != nil {
			t.Errorf("uppercase hash should verify: %v", err)
		}
	})

	t.Run("uppercase hash still rejects tampering", func(t *testing.T) {
		tampered := append([]byte(nil), body...)
		tampered[0] = 'H'
		r := s3gw.NewPayloadVerifier(bytes.NewReader(tampered), strings.ToUpper(want), int64(len(tampered)))
		_, err := io.ReadAll(r)
		assertS3Code(t, err, "XAmzContentSHA256Mismatch")
	})

	t.Run("verified without reading to EOF", func(t *testing.T) {
		tampered := append([]byte(nil), body...)
		tampered[len(tampered)-1] = 'X'
		r := s3gw.NewPayloadVerifier(bytes.NewReader(tampered), want, int64(len(tampered)))
		// read exactly the declared length, byte by byte, then stop
		buf := make([]byte, 1)
		var readErr error
		for range tampered {
			if _, err := r.Read(buf); err != nil {
				readErr = err
				break
			}
		}
		assertS3Code(t, readErr, "XAmzContentSHA256Mismatch")
	})
}

// TestXMLBodyHashMismatch: an XML-bodied operation whose body does not match
// the signed payload hash must answer with the verifying reader's own error
// (XAmzContentSHA256Mismatch), not a generic failed-to-read — the same rule
// fromSDKError applies on the PutObject path.
func TestXMLBodyHashMismatch(t *testing.T) {
	pathStyle := true
	gw := s3gw.New(memStore{
		keys: map[string]*store.Key{
			testAccessKeyID: {
				AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey,
				Tenant: "testtenant", User: "testuser",
			},
		},
		buckets: map[string]*store.Bucket{
			"testbucket": {
				Tenant: "testtenant", Name: "testbucket",
				Backend: &store.Backend{
					Endpoint: "http://backend.invalid", Region: "us-east-1",
					Bucket: "backend-testbucket", AccessKeyID: "bk", SecretAccessKey: "bs",
					UsePathStyle: &pathStyle,
				},
			},
		},
	})
	if err := gw.SetBackend("testbucket", stubGet{}); err != nil {
		t.Fatal(err)
	}

	body := []byte(`<Delete><Object><Key>a.txt</Key></Object></Delete>`)
	other := sha256.Sum256([]byte("not the body that is sent"))
	req := signedRequest(t, "POST", "http://s3.example.com/testbucket?delete",
		body, hex.EncodeToString(other[:]), time.Now(), testCreds(), nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "XAmzContentSHA256Mismatch") {
		t.Errorf("expect XAmzContentSHA256Mismatch, got %d: %s", w.Code, w.Body.String())
	}
}

func assertS3Code(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expect error with code %s, got nil", code)
	}
	if !strings.Contains(err.Error(), code) {
		t.Errorf("expect error code %s, got %v", code, err)
	}
}
