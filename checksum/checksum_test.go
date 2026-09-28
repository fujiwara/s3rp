package checksum_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/checksum"
	"github.com/google/go-cmp/cmp"
)

// The algorithms are known-answer tested against "123456789", the check
// value every CRC catalogue publishes, so a wrong polynomial, a wrong bit
// order or a wrong byte order fails here rather than at a backend that
// rejects the upload. The expected values were computed independently of
// this package.
//
//	CRC-32/ISO-HDLC   0xCBF43926
//	CRC-32/ISCSI      0xE3069283 (Castagnoli)
//	CRC-64/NVME       0xAE8B14860A799888
func TestNewHashKnownAnswers(t *testing.T) {
	const check = "123456789"
	for _, tc := range []struct{ alg, want string }{
		{"crc32", "y/Q5Jg=="},
		{"crc32c", "4waSgw=="},
		{"crc64nvme", "rosUhgp5mIg="},
		{"sha1", "98O8HYCOBHMq32eZZczDTKeuNEE="},
		{"sha256", "FeKw08M4keuw8e9gnsQZQgwg4yDOlMZfvIwzEkSOsiU="},
		{"sha512", "2eZ2LdHI6vbWGzxhkvxAjU1tXxF20MKRabwk5xw/J0rSf81YEbMT1oH35V7ALXPUmclUVba1u1A6z1dPuo/+hQ=="},
		{"md5", "JfnnlDI7RTiF9RgfG2JNCw=="},
	} {
		t.Run(tc.alg, func(t *testing.T) {
			h := checksum.NewHash(tc.alg)
			if h == nil {
				t.Fatalf("%s is not supported", tc.alg)
			}
			h.Write([]byte(check))
			if got := checksum.Base64(h); got != tc.want {
				t.Errorf("expect %q, got %q", tc.want, got)
			}
		})
	}
}

// A value is the base64 of the sum in network byte order; writing the
// payload in pieces must not change it.
func TestNewHashIsIncremental(t *testing.T) {
	for _, alg := range []string{"crc32", "crc32c", "crc64nvme", "sha1", "sha256", "sha512", "md5"} {
		whole := checksum.NewHash(alg)
		whole.Write([]byte("123456789"))
		split := checksum.NewHash(alg)
		split.Write([]byte("1234"))
		split.Write([]byte("56789"))
		if a, b := checksum.Base64(whole), checksum.Base64(split); a != b {
			t.Errorf("%s: %q in one write, %q in two", alg, a, b)
		}
	}
}

func TestNewHashUnsupported(t *testing.T) {
	// an algorithm the gateway does not compute must be reported, not
	// silently treated as one it does: the caller decides what to do with a
	// request declaring a checksum this build cannot compute
	for _, alg := range []string{"", "xxhash64", "xxhash3", "xxhash128", "crc16"} {
		if h := checksum.NewHash(alg); h != nil {
			t.Errorf("expect nil for %q, got %T", alg, h)
		}
	}
	// names are case-insensitive, as in the headers that carry them
	if checksum.NewHash("CRC32") == nil || checksum.NewHash("crc32C") == nil {
		t.Error("expect a hasher regardless of case")
	}
}

// The table's invariants: a hasher exactly for the Computable algorithms,
// and every name maps to its own header.
func TestAlgorithms(t *testing.T) {
	seen := map[string]bool{}
	for _, a := range checksum.Algorithms() {
		if seen[a.Name] {
			t.Errorf("%s listed twice", a.Name)
		}
		seen[a.Name] = true
		if (a.NewHash() != nil) != a.Computable() {
			t.Errorf("%s: Computable %v but hasher present = %v", a.Name, a.Computable(), a.NewHash() != nil)
		}
		if a.Name != strings.ToUpper(a.Name) {
			t.Errorf("%s: names are upper-case", a.Name)
		}
		if got, ok := checksum.Lookup(strings.ToLower(a.Name)); !ok || got.Name != a.Name {
			t.Errorf("Lookup(%q) = %v, %v", strings.ToLower(a.Name), got.Name, ok)
		}
	}
	if _, ok := checksum.Lookup("CRC16"); ok {
		t.Error("expect an unknown name not to be found")
	}
	if got := (checksum.Algorithm{Name: "CRC64NVME"}).Header(); got != "x-amz-checksum-crc64nvme" {
		t.Errorf("unexpected header %q", got)
	}
}

func TestFromHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("x-amz-checksum-crc32", "y/Q5Jg==")
	h.Set("x-amz-checksum-sha256", "FeKw08M4keuw8e9gnsQZQgwg4yDOlMZfvIwzEkSOsiU=")
	// read whatever the Support: refusing is the caller's gate
	h.Set("x-amz-checksum-xxhash3", "AAAAAAAAAAA=")
	want := checksum.Values{
		"CRC32":   "y/Q5Jg==",
		"SHA256":  "FeKw08M4keuw8e9gnsQZQgwg4yDOlMZfvIwzEkSOsiU=",
		"XXHASH3": "AAAAAAAAAAA=",
	}
	if diff := cmp.Diff(want, checksum.FromHeaders(h)); diff != "" {
		t.Errorf("unexpected values (-want +got):\n%s", diff)
	}
	// no headers, no values (and no allocation)
	if got := checksum.FromHeaders(http.Header{}); got != nil {
		t.Errorf("empty headers must yield nil, got %v", got)
	}
}

func TestSetHeaders(t *testing.T) {
	h := http.Header{}
	checksum.SetHeaders(h, checksum.Values{
		"CRC32":  "y/Q5Jg==",
		"CRC32C": "", // present but empty: nothing to report
		"SHA512": "c2hhNTEy",
	}, "FULL_OBJECT")
	want := http.Header{
		"X-Amz-Checksum-Crc32":  {"y/Q5Jg=="},
		"X-Amz-Checksum-Sha512": {"c2hhNTEy"},
		"X-Amz-Checksum-Type":   {"FULL_OBJECT"},
	}
	if diff := cmp.Diff(want, h); diff != "" {
		t.Errorf("unexpected headers (-want +got):\n%s", diff)
	}

	// no checksum type means no type header
	h = http.Header{}
	checksum.SetHeaders(h, checksum.Values{"SHA256": "x"}, "")
	if _, ok := h["X-Amz-Checksum-Type"]; ok {
		t.Errorf("expect no type header, got %v", h)
	}
}

func TestTrailerAlgorithm(t *testing.T) {
	for _, tc := range []struct {
		header, want string
		err          error
	}{
		{header: "x-amz-checksum-crc32", want: "crc32"},
		{header: "X-Amz-Checksum-CRC32C", want: "crc32c"},   // case-insensitive
		{header: " x-amz-checksum-sha256 ", want: "sha256"}, // padded
		{header: "", want: ""},
		// a trailer the decoder cannot verify must not be silently dropped
		// computable, so declarable; whether a service offers it is s3gw's
		{header: "x-amz-checksum-sha512", want: "sha512"},
		{header: "x-amz-checksum-xxhash3", err: checksum.ErrUnsupportedTrailer},
		{header: "x-amz-meta-foo", err: checksum.ErrUnsupportedTrailer},
		{header: "x-amz-meta-foo,x-amz-checksum-sha1", err: checksum.ErrUnsupportedTrailer},
		{header: "x-amz-checksum-crc32,x-amz-checksum-sha1", err: checksum.ErrUnsupportedTrailer},
	} {
		h := http.Header{}
		if tc.header != "" {
			h.Set("x-amz-trailer", tc.header)
		}
		got, err := checksum.TrailerAlgorithm(h)
		if !errors.Is(err, tc.err) {
			t.Errorf("%q: expect error %v, got %v", tc.header, tc.err, err)
		}
		if got != tc.want {
			t.Errorf("%q: expect %q, got %q", tc.header, tc.want, got)
		}
	}
}

func TestValuesAlgorithm(t *testing.T) {
	cases := []struct {
		name string
		vals checksum.Values
		want string
	}{
		{"none", nil, ""},
		{"empty value", checksum.Values{"CRC32": ""}, ""},
		{"crc32", checksum.Values{"CRC32": "x"}, "CRC32"},
		{"crc64nvme", checksum.Values{"CRC64NVME": "x"}, "CRC64NVME"},
		{"sha512", checksum.Values{"SHA512": "x"}, "SHA512"},
		// table order decides between several
		{"two", checksum.Values{"SHA256": "x", "CRC32C": "y"}, "CRC32C"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.vals.Algorithm(); got != tc.want {
				t.Errorf("expect %q, got %q", tc.want, got)
			}
		})
	}
}
