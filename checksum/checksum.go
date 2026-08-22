// Package checksum implements the S3 x-amz-checksum-* algorithms and the
// headers that carry them. It depends only on the standard library so it can
// be reused by any service that speaks the S3 checksum protocol.
package checksum

import (
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"net/http"
	"strings"
)

// Checksums flow end-to-end: x-amz-checksum-* request headers pass through
// to the backend, aws-chunked trailer Values are verified by the proxy
// (chunked.go) and recomputed toward an https backend via ChecksumAlgorithm
// (s3gw trailerChecksumAlgorithm), and response Values pass back to the
// client.

// HeaderPrefix is the prefix an algorithm's checksum header carries
// ("x-amz-checksum-" + the lower-case algorithm name). It is exported because
// the name is built in one place and parsed in another: a chunked body's
// trailer names the header, and the caller verifying that trailer must build
// the same name this package's TrailerAlgorithm cut off it.
const HeaderPrefix = "x-amz-checksum-"

// crc64NVMETable is the CRC64/NVMe polynomial (reversed), as used by
// x-amz-checksum-crc64nvme.
var crc64NVMETable = crc64.MakeTable(0x9A6C9329AC4BC9B5)

// Values carries the x-amz-checksum-* values of a request or response.
type Values struct {
	CRC32     *string
	CRC32C    *string
	CRC64NVME *string
	SHA1      *string
	SHA256    *string
}

// Algorithm returns the upper-case name of the algorithm a value is set
// for ("" when none) — the form x-amz-sdk-checksum-algorithm and the SDK's
// ChecksumAlgorithm parameter use. A request carries at most one checksum.
func (v Values) Algorithm() string {
	switch {
	case v.CRC32 != nil:
		return "CRC32"
	case v.CRC32C != nil:
		return "CRC32C"
	case v.CRC64NVME != nil:
		return "CRC64NVME"
	case v.SHA1 != nil:
		return "SHA1"
	case v.SHA256 != nil:
		return "SHA256"
	}
	return ""
}

func FromHeaders(h http.Header) Values {
	get := func(alg string) *string {
		if v := h.Get(HeaderPrefix + alg); v != "" {
			return &v
		}
		return nil
	}
	return Values{
		CRC32:     get("crc32"),
		CRC32C:    get("crc32c"),
		CRC64NVME: get("crc64nvme"),
		SHA1:      get("sha1"),
		SHA256:    get("sha256"),
	}
}

// SetHeaders sets x-amz-checksum-* response headers.
func SetHeaders(h http.Header, cs Values, checksumType string) {
	set := func(alg string, v *string) {
		if v != nil && *v != "" {
			h.Set(HeaderPrefix+alg, *v)
		}
	}
	set("crc32", cs.CRC32)
	set("crc32c", cs.CRC32C)
	set("crc64nvme", cs.CRC64NVME)
	set("sha1", cs.SHA1)
	set("sha256", cs.SHA256)
	if checksumType != "" {
		h.Set("x-amz-checksum-type", checksumType)
	}
}

// TrailerAlgorithm returns the checksum algorithm declared in the
// x-amz-trailer header ("x-amz-trailer: x-amz-checksum-crc32" -> "crc32"),
// or "" if the request declares no checksum trailer.
func TrailerAlgorithm(h http.Header) string {
	for t := range strings.SplitSeq(h.Get("x-amz-trailer"), ",") {
		t = strings.ToLower(strings.TrimSpace(t))
		if alg, ok := strings.CutPrefix(t, HeaderPrefix); ok {
			if NewHash(alg) != nil {
				return alg
			}
		}
	}
	return ""
}

// NewHash returns a hasher for the algorithm; the base64 of its
// Sum(nil) is the x-amz-checksum-* value. Returns nil for unsupported
// algorithms.
func NewHash(alg string) hash.Hash {
	switch alg {
	case "crc32":
		return crc32.NewIEEE()
	case "crc32c":
		return crc32.New(crc32.MakeTable(crc32.Castagnoli))
	case "crc64nvme":
		return crc64.New(crc64NVMETable)
	case "sha1":
		return sha1.New()
	case "sha256":
		return sha256.New()
	}
	return nil
}

func Base64(h hash.Hash) string {
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
