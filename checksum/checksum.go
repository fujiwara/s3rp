// Package checksum implements the S3 x-amz-checksum-* algorithms and the
// headers that carry them. It depends only on the standard library so it can
// be reused by any service that speaks the S3 checksum protocol.
package checksum

import (
	"crypto/sha1"
	"crypto/sha256"
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

// Support is how far the gateway itself handles an algorithm.
type Support int

const (
	// Refused: the algorithm's value header, trailer and name are refused
	// (501), so nothing claims a check the gateway would not pass on.
	Refused Support = iota
	// Forwarded: value headers and the algorithm name pass to the backend,
	// which verifies and stores the checksum — whether it does depends on
	// the backend. A trailer is refused: the gateway cannot verify it, and
	// dropping it would report a check nobody made.
	Forwarded
	// Verified: Forwarded, and the gateway computes the algorithm itself,
	// so an aws-chunked trailer is verified by the gateway.
	Verified
)

func (s Support) String() string {
	switch s {
	case Forwarded:
		return "forwarded"
	case Verified:
		return "verified"
	}
	return "refused"
}

// Algorithm is one S3 checksum algorithm and how far the gateway handles it.
type Algorithm struct {
	// Name is the upper-case name x-amz-checksum-algorithm and the SDK's
	// ChecksumAlgorithm use ("CRC64NVME").
	Name    string
	Support Support
	newHash func() hash.Hash // set iff Support == Verified
}

// Header is the algorithm's value header ("x-amz-checksum-crc64nvme").
func (a Algorithm) Header() string { return HeaderPrefix + strings.ToLower(a.Name) }

// NewHash returns a hasher for a Verified algorithm, nil otherwise; the
// base64 of its Sum(nil) is the x-amz-checksum-* value.
func (a Algorithm) NewHash() hash.Hash {
	if a.newHash == nil {
		return nil
	}
	return a.newHash()
}

// algorithms is every checksum algorithm S3 defines, the refused ones
// included, so it is the one place that answers "what about X?". Changing
// an algorithm's Support is the whole change: the value conversions are
// generated for every algorithm listed here (s3gw's go:generate), and the
// gates read Support.
var algorithms = []Algorithm{
	{Name: "CRC32", Support: Verified, newHash: func() hash.Hash { return crc32.NewIEEE() }},
	{Name: "CRC32C", Support: Verified, newHash: func() hash.Hash { return crc32.New(crc32.MakeTable(crc32.Castagnoli)) }},
	{Name: "CRC64NVME", Support: Verified, newHash: func() hash.Hash { return crc64.New(crc64NVMETable) }},
	{Name: "SHA1", Support: Verified, newHash: sha1.New},
	{Name: "SHA256", Support: Verified, newHash: sha256.New},
	{Name: "SHA512", Support: Refused},
	{Name: "MD5", Support: Refused},
	// no implementation in the standard library, which this package is
	// limited to
	{Name: "XXHASH64", Support: Refused},
	{Name: "XXHASH3", Support: Refused},
	{Name: "XXHASH128", Support: Refused},
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

// FromHeaders reads the value header of every algorithm, whatever its
// Support: refusing is the caller's gate (s3gw's known-header check), not
// a silent drop here.
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
// algorithm that is not Verified, a non-checksum trailer, more than one
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

// NewHash returns a hasher for a Verified algorithm named case-insensitively
// ("crc32" or "CRC32"), nil for any other.
func NewHash(name string) hash.Hash {
	a, _ := Lookup(name)
	return a.NewHash()
}

func Base64(h hash.Hash) string {
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}
