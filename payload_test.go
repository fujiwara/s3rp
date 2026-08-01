package s3rp_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp"
)

func TestPayloadVerifier(t *testing.T) {
	body := []byte("hello world payload")
	sum := sha256.Sum256(body)
	want := hex.EncodeToString(sum[:])

	t.Run("matching hash passes", func(t *testing.T) {
		r := s3rp.NewPayloadVerifier(bytes.NewReader(body), want, int64(len(body)))
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
		r := s3rp.NewPayloadVerifier(bytes.NewReader(tampered), want, int64(len(tampered)))
		_, err := io.ReadAll(r)
		assertS3Code(t, err, "XAmzContentSHA256Mismatch")
	})

	// a mis-cased but valid hash must still be verified, not silently skipped
	t.Run("uppercase hash is accepted", func(t *testing.T) {
		r := s3rp.NewPayloadVerifier(bytes.NewReader(body), strings.ToUpper(want), int64(len(body)))
		if _, err := io.ReadAll(r); err != nil {
			t.Errorf("uppercase hash should verify: %v", err)
		}
	})

	t.Run("uppercase hash still rejects tampering", func(t *testing.T) {
		tampered := append([]byte(nil), body...)
		tampered[0] = 'H'
		r := s3rp.NewPayloadVerifier(bytes.NewReader(tampered), strings.ToUpper(want), int64(len(tampered)))
		_, err := io.ReadAll(r)
		assertS3Code(t, err, "XAmzContentSHA256Mismatch")
	})

	t.Run("verified without reading to EOF", func(t *testing.T) {
		tampered := append([]byte(nil), body...)
		tampered[len(tampered)-1] = 'X'
		r := s3rp.NewPayloadVerifier(bytes.NewReader(tampered), want, int64(len(tampered)))
		// read exactly the declared length, byte by byte, then stop
		buf := make([]byte, 1)
		var readErr error
		for i := 0; i < len(tampered); i++ {
			if _, err := r.Read(buf); err != nil {
				readErr = err
				break
			}
		}
		assertS3Code(t, readErr, "XAmzContentSHA256Mismatch")
	})
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
