// Package checksum implements the S3 x-amz-checksum-* algorithms and the
// headers that carry them. It depends only on the standard library so it can
// be reused by any service that speaks the S3 checksum protocol.
package checksum

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"hash"
	"hash/crc32"
	"hash/crc64"
	"net/http"
	"slices"
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

// Algorithm is one S3 checksum algorithm. Whether a service offers it to
// clients is not this package's concern (s3gw's SetChecksumSupport); what
// is here is the fact of whether it can be computed, which an
// aws-chunked trailer needs.
type Algorithm struct {
	// Name is the upper-case name x-amz-checksum-algorithm and the SDK's
	// ChecksumAlgorithm use ("CRC64NVME").
	Name    string
	newHash func() hash.Hash // nil: not implemented by this package
}

// Header is the algorithm's value header ("x-amz-checksum-crc64nvme").
func (a Algorithm) Header() string { return HeaderPrefix + strings.ToLower(a.Name) }

// Computable reports whether this package implements the algorithm.
func (a Algorithm) Computable() bool { return a.newHash != nil }

// NewHash returns a hasher for a Computable algorithm, nil otherwise; the
// base64 of its Sum(nil) is the x-amz-checksum-* value.
func (a Algorithm) NewHash() hash.Hash {
	if a.newHash == nil {
		return nil
	}
	return a.newHash()
}

// algorithms is every checksum algorithm S3 defines. The value conversions
// are generated for every algorithm listed here (s3gw's go:generate).
var algorithms = []Algorithm{
	{Name: "CRC32", newHash: func() hash.Hash { return crc32.NewIEEE() }},
	{Name: "CRC32C", newHash: func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) }},
	{Name: "CRC64NVME", newHash: func() hash.Hash { return crc64.New(crc64NVMETable) }},
	{Name: "SHA1", newHash: sha1.New},
	{Name: "SHA256", newHash: sha256.New},
	{Name: "SHA512", newHash: sha512.New},
	{Name: "MD5", newHash: md5.New},
	// no implementation in the standard library, which this package is
	// limited to
	{Name: "XXHASH64"},
	{Name: "XXHASH3"},
	{Name: "XXHASH128"},
}

// Algorithms returns every S3 checksum algorithm in a fixed order.
func Algorithms() []Algorithm { return slices.Clone(algorithms) }

// Lookup finds an algorithm by name, case-insensitively; false means the
// name is not an S3 checksum algorithm at all.
func Lookup(name string) (Algorithm, bool) {
	for _, a := range algorithms {
		if strings.EqualFold(a.Name, name) {
			return a, true
		}
	}
	return Algorithm{}, false
}

// Values carries the x-amz-checksum-* values of a request or response,
// keyed by Algorithm.Name. A nil Values carries none.
type Values map[string]string

// Algorithm returns the name of the algorithm a value is set for ("" when
// none) — the form x-amz-sdk-checksum-algorithm and the SDK's
// ChecksumAlgorithm parameter use. A request carries at most one checksum.
func (v Values) Algorithm() string {
	for _, a := range algorithms {
		if v[a.Name] != "" {
			return a.Name
		}
	}
	return ""
}

// FromHeaders reads the value header of every algorithm: which ones a
// request may carry is the caller's gate (s3gw's known-header check), not a
// silent drop here.
func FromHeaders(h http.Header) Values {
	var v Values
	for _, a := range algorithms {
		if s := h.Get(a.Header()); s != "" {
			if v == nil {
				v = Values{}
			}
			v[a.Name] = s
		}
	}
	return v
}

// SetHeaders sets x-amz-checksum-* response headers.
func SetHeaders(h http.Header, v Values, checksumType string) {
	for _, a := range algorithms {
		if s := v[a.Name]; s != "" {
			h.Set(a.Header(), s)
		}
	}
	if checksumType != "" {
		h.Set("x-amz-checksum-type", checksumType)
	}
}

// ErrUnsupportedTrailer is returned by TrailerAlgorithm when x-amz-trailer
// declares something other than a single supported checksum.
var ErrUnsupportedTrailer = errors.New("unsupported x-amz-trailer")

// TrailerAlgorithm returns the checksum algorithm declared in the
// x-amz-trailer header ("x-amz-trailer: x-amz-checksum-crc32" -> "crc32"),
// or "" if the request declares no trailer. Any other declaration — an
// algorithm that is not Computable, a non-checksum trailer, more than one
// trailer — is ErrUnsupportedTrailer: a trailer the decoder cannot verify
// would otherwise be dropped while the client believes it was applied.
func TrailerAlgorithm(h http.Header) (string, error) {
	var alg string
	for t := range strings.SplitSeq(h.Get("x-amz-trailer"), ",") {
		t = strings.ToLower(strings.TrimSpace(t))
		if t == "" {
			continue
		}
		a, ok := strings.CutPrefix(t, HeaderPrefix)
		if !ok || NewHash(a) == nil || alg != "" {
			return "", ErrUnsupportedTrailer
		}
		alg = a
	}
	return alg, nil
}

// NewHash returns a hasher for a Computable algorithm named
// case-insensitively ("crc32" or "CRC32"), nil for any other.
func NewHash(name string) hash.Hash {
	a, _ := Lookup(name)
	return a.NewHash()
}

func Base64(h hash.Hash) string {
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
