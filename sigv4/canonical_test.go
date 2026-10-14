package sigv4_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/sigv4"
)

// TestVerifySignedHeaderList: the signature covers the SignedHeaders list
// as the client wrote it, so a list that cannot describe a canonical
// request — out of order, duplicated, not lower-case, without host, or
// claiming the Authorization header itself — is refused before anything is
// computed over it, and a listed header the request does not carry is a
// malformed request.
func TestVerifySignedHeaderList(t *testing.T) {
	cases := []struct {
		name, signedHeaders, code, detail string
	}{
		{"unsorted", "x-amz-date;host;x-amz-content-sha256", "SignatureDoesNotMatch", "not sorted"},
		{"duplicated", "host;host;x-amz-content-sha256;x-amz-date", "SignatureDoesNotMatch", "not sorted"},
		{"upper-case", "Host;x-amz-content-sha256;x-amz-date", "SignatureDoesNotMatch", "not lower-case"},
		{"empty name", "host;;x-amz-content-sha256;x-amz-date", "SignatureDoesNotMatch", "not lower-case"},
		{"no host", "x-amz-content-sha256;x-amz-date", "SignatureDoesNotMatch", "host is not a signed header"},
		{"authorization", "authorization;host;x-amz-content-sha256;x-amz-date", "SignatureDoesNotMatch", "authorization"},
		{"absent header", "content-md5;host;x-amz-content-sha256;x-amz-date", "InvalidRequest", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := signedRequest(t, testSecret)
			auth := r.Header.Get("Authorization")
			i := strings.Index(auth, "SignedHeaders=")
			j := strings.Index(auth[i:], ",")
			r.Header.Set("Authorization", auth[:i]+"SignedHeaders="+tc.signedHeaders+auth[i+j:])

			_, s3e := newVerifier().Verify(r, lookup)
			if s3e == nil {
				t.Fatal("expect refusal")
			}
			if s3e.Code != tc.code {
				t.Fatalf("code %s (%s), want %s", s3e.Code, s3e.Message, tc.code)
			}
			if tc.detail == "" {
				return
			}
			var f *sigv4.AuthFailure
			if !errors.As(s3e, &f) || f.Reason != sigv4.ReasonSignatureMismatch || !strings.Contains(f.Detail, tc.detail) {
				t.Errorf("cause %+v, want %s naming %q", f, sigv4.ReasonSignatureMismatch, tc.detail)
			}
		})
	}
}

// TestVerifyPresignedDuplicateAuthParam: the checks read the first value of
// an auth parameter while the signature covers every value, so a URL
// carrying one twice is refused rather than verified with the two possibly
// disagreeing.
func TestVerifyPresignedDuplicateAuthParam(t *testing.T) {
	for _, p := range []string{"X-Amz-Date=20260801T120000Z", "X-Amz-Expires=300", "X-Amz-Signature=00"} {
		r := presignedRequest(t, "http://s3.example.com/bucket/key.txt?X-Amz-Expires=300")
		r.URL.RawQuery += "&" + p
		r.RequestURI = r.URL.RequestURI()
		_, s3e := newVerifier().Verify(r, lookup)
		if s3e == nil || s3e.Code != "AuthorizationQueryParametersError" {
			t.Errorf("duplicated %s: got %v, want AuthorizationQueryParametersError", p, s3e)
		}
	}
}
