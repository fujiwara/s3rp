package checksum_test

import (
	"net/http"
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
	for _, alg := range []string{"crc32", "crc32c", "crc64nvme", "sha1", "sha256"} {
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
	// an unknown algorithm must be reported, not silently treated as one of
	// the supported ones: the caller decides what to do with a request
	// declaring a checksum this build cannot compute
	for _, alg := range []string{"", "md5", "crc32C", "CRC32", "sha512"} {
		if h := checksum.NewHash(alg); h != nil {
			t.Errorf("expect nil for %q, got %T", alg, h)
		}
	}
}

func TestFromHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("x-amz-checksum-crc32", "y/Q5Jg==")
	h.Set("x-amz-checksum-sha256", "FeKw08M4keuw8e9gnsQZQgwg4yDOlMZfvIwzEkSOsiU=")
	got := checksum.FromHeaders(h)
	if got.CRC32 == nil || *got.CRC32 != "y/Q5Jg==" {
		t.Errorf("unexpected crc32 %v", got.CRC32)
	}
	if got.SHA256 == nil || *got.SHA256 != "FeKw08M4keuw8e9gnsQZQgwg4yDOlMZfvIwzEkSOsiU=" {
		t.Errorf("unexpected sha256 %v", got.SHA256)
	}
	// absent headers stay nil rather than becoming an empty value, so the
	// caller does not send an empty checksum to a backend
	if got.CRC32C != nil || got.CRC64NVME != nil || got.SHA1 != nil {
		t.Errorf("expect nil for absent algorithms, got %+v", got)
	}
	if diff := cmp.Diff(checksum.Values{}, checksum.FromHeaders(http.Header{})); diff != "" {
		t.Errorf("empty headers must yield no values (-want +got):\n%s", diff)
	}
}

func TestSetHeaders(t *testing.T) {
	v := func(s string) *string { return &s }
	empty := ""

	h := http.Header{}
	checksum.SetHeaders(h, checksum.Values{
		CRC32:  v("y/Q5Jg=="),
		CRC32C: &empty, // present but empty: nothing to report
		SHA1:   nil,
	}, "FULL_OBJECT")
	want := http.Header{
		"X-Amz-Checksum-Crc32": {"y/Q5Jg=="},
		"X-Amz-Checksum-Type":  {"FULL_OBJECT"},
	}
	if diff := cmp.Diff(want, h); diff != "" {
		t.Errorf("unexpected headers (-want +got):\n%s", diff)
	}

	// no checksum type means no type header
	h = http.Header{}
	checksum.SetHeaders(h, checksum.Values{SHA256: v("x")}, "")
	if _, ok := h["X-Amz-Checksum-Type"]; ok {
		t.Errorf("expect no type header, got %v", h)
	}
}

func TestTrailerAlgorithm(t *testing.T) {
	for _, tc := range []struct{ header, want string }{
		{"x-amz-checksum-crc32", "crc32"},
		{"X-Amz-Checksum-CRC32C", "crc32c"},            // case-insensitive
		{" x-amz-checksum-sha256 ", "sha256"},          // padded
		{"x-amz-meta-foo,x-amz-checksum-sha1", "sha1"}, // picked out of a list
		{"", ""},
		{"x-amz-meta-foo", ""},
		{"x-amz-checksum-md5", ""}, // declared but unsupported
	} {
		h := http.Header{}
		if tc.header != "" {
			h.Set("x-amz-trailer", tc.header)
		}
		if got := checksum.TrailerAlgorithm(h); got != tc.want {
			t.Errorf("%q: expect %q, got %q", tc.header, tc.want, got)
		}
	}
}

func TestValuesAlgorithm(t *testing.T) {
	v := func(s string) *string { return &s }
	cases := []struct {
		name string
		vals checksum.Values
		want string
	}{
		{"none", checksum.Values{}, ""},
		{"crc32", checksum.Values{CRC32: v("x")}, "CRC32"},
		{"crc32c", checksum.Values{CRC32C: v("x")}, "CRC32C"},
		{"crc64nvme", checksum.Values{CRC64NVME: v("x")}, "CRC64NVME"},
		{"sha1", checksum.Values{SHA1: v("x")}, "SHA1"},
		{"sha256", checksum.Values{SHA256: v("x")}, "SHA256"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.vals.Algorithm(); got != tc.want {
				t.Errorf("expect %q, got %q", tc.want, got)
			}
		})
	}
}
