package s3gw

import (
	"fmt"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp/checksum"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"
)

// The per-algorithm conversions (checksumsFrom*, setChecksums*, the s3xml
// Checksums) are generated for every algorithm in checksum.Algorithms;
// which algorithms a client may use and see is the Gateway's
// checksumPolicy, applied in this file and nowhere else.
//go:generate go run ./internal/checksumgen/cmd/checksumgen

// ChecksumSupport is how far a service offers a checksum algorithm to its
// clients. It is one setting for the whole Gateway, deliberately not per
// backend: clients cannot tell which backend serves a bucket, so a service
// on several kinds of backend offers what all of them handle.
type ChecksumSupport int

const (
	// ChecksumRefused: the algorithm's value header, trailer, name
	// (x-amz-checksum-algorithm) and CompleteMultipartUpload part values
	// are refused (501), and values the backend reports are not shown.
	ChecksumRefused ChecksumSupport = iota
	// ChecksumForwarded: value headers and the name pass to the backend,
	// which verifies and stores the checksum — offer it only for an
	// algorithm every backend the service uses does. A trailer is refused.
	ChecksumForwarded
	// ChecksumVerified: ChecksumForwarded, plus an aws-chunked trailer is
	// verified by the gateway itself. Requires a checksum.Computable
	// algorithm.
	ChecksumVerified
)

func (s ChecksumSupport) String() string {
	switch s {
	case ChecksumForwarded:
		return "forwarded"
	case ChecksumVerified:
		return "verified"
	}
	return "refused"
}

// defaultChecksumSupport is what a Gateway offers until SetChecksumSupport:
// the algorithms every bundled backend (Ceph RGW, versitygw, RustFS)
// handles. Every other algorithm is refused.
var defaultChecksumSupport = map[string]ChecksumSupport{
	"CRC32":     ChecksumVerified,
	"CRC32C":    ChecksumVerified,
	"CRC64NVME": ChecksumVerified,
	"SHA1":      ChecksumVerified,
	"SHA256":    ChecksumVerified,
}

// DefaultChecksumSupport returns the support a Gateway starts with, with
// an entry for every algorithm in checksum.Algorithms; the map is the
// caller's to modify and pass to SetChecksumSupport.
func DefaultChecksumSupport() map[string]ChecksumSupport {
	m := map[string]ChecksumSupport{}
	for _, a := range checksum.Algorithms() {
		m[a.Name] = defaultChecksumSupport[a.Name]
	}
	return m
}

// SetChecksumSupport replaces the support of every checksum algorithm: an
// algorithm m does not name is refused, so a service offers exactly what
// it lists — a later default does not widen it. Start from
// DefaultChecksumSupport to change a few. Names are case-insensitive. It
// errors, changing nothing, on a name that is not an S3 checksum algorithm,
// on ChecksumVerified for an algorithm the gateway cannot compute, and on
// an algorithm named twice in different case. Call before serving requests.
func (g *Gateway) SetChecksumSupport(m map[string]ChecksumSupport) error {
	p, err := newChecksumPolicy(m)
	if err != nil {
		return err
	}
	g.checksums = p
	return nil
}

// checksumPolicy is a validated ChecksumSupport for every algorithm, keyed
// by checksum.Algorithm.Name and by value header.
type checksumPolicy struct {
	byName   map[string]ChecksumSupport
	byHeader map[string]ChecksumSupport
}

func newChecksumPolicy(m map[string]ChecksumSupport) (*checksumPolicy, error) {
	p := &checksumPolicy{byName: map[string]ChecksumSupport{}, byHeader: map[string]ChecksumSupport{}}
	for name, s := range m {
		a, ok := checksum.Lookup(name)
		if !ok {
			return nil, fmt.Errorf("checksum support: %q is not an S3 checksum algorithm", name)
		}
		if s < ChecksumRefused || s > ChecksumVerified {
			return nil, fmt.Errorf("checksum support: invalid support %d for %s", s, a.Name)
		}
		if s == ChecksumVerified && !a.Computable() {
			return nil, fmt.Errorf("checksum support: %s cannot be verified by the gateway; offer it as forwarded", a.Name)
		}
		// names are case-insensitive, so "sha512" and "SHA512" are one
		// algorithm; which of two entries won would depend on map order
		if _, dup := p.byName[a.Name]; dup {
			return nil, fmt.Errorf("checksum support: %s is listed more than once", a.Name)
		}
		p.byName[a.Name] = s
		p.byHeader[a.Header()] = s
	}
	return p, nil
}

func mustChecksumPolicy(m map[string]ChecksumSupport) *checksumPolicy {
	p, err := newChecksumPolicy(m)
	if err != nil {
		panic(err)
	}
	return p
}

// support is an algorithm's support by name, case-insensitively; an
// unknown name is refused.
func (p *checksumPolicy) support(name string) ChecksumSupport {
	a, _ := checksum.Lookup(name)
	return p.byName[a.Name]
}

// knownHeader reports whether a lower-case request header is the value
// header of an offered algorithm (checkKnownAmzHeaders).
func (p *checksumPolicy) knownHeader(lname string) bool {
	return p.byHeader[lname] != ChecksumRefused
}

// offered keeps the values of offered algorithms: a checksum the service
// does not offer is not shown, whatever the backend reports.
func (p *checksumPolicy) offered(v checksum.Values) checksum.Values {
	for name := range v {
		if p.byName[name] == ChecksumRefused {
			delete(v, name)
		}
	}
	return v
}

// checksumAlgorithm reads x-amz-checksum-algorithm, the name a client asks
// the backend to compute (CreateMultipartUpload, CopyObject).
func (c *opCtx) checksumAlgorithm() (types.ChecksumAlgorithm, *s3err.Error) {
	v := c.signed("x-amz-checksum-algorithm")
	if v == "" {
		return "", nil
	}
	a, ok := checksum.Lookup(v)
	if !ok {
		return "", s3err.New(http.StatusBadRequest, "InvalidRequest",
			"Value for x-amz-checksum-algorithm header is invalid.")
	}
	if c.g.checksums.support(a.Name) == ChecksumRefused {
		return "", s3err.NotImplemented("checksum algorithm " + a.Name)
	}
	return types.ChecksumAlgorithm(a.Name), nil
}

// checkChecksums refuses values of a refused algorithm that arrive outside
// a header (the parts of a CompleteMultipartUpload body), which
// checkKnownAmzHeaders never sees.
func (c *opCtx) checkChecksums(v checksum.Values) *s3err.Error {
	for name := range v {
		if c.g.checksums.support(name) == ChecksumRefused {
			return s3err.NotImplemented("checksum algorithm " + name)
		}
	}
	return nil
}

// checkTrailer refuses a declared trailer the service does not offer as
// verified; checksum.TrailerAlgorithm has already refused one the gateway
// cannot compute.
func (c *opCtx) checkTrailer(alg string) *s3err.Error {
	if alg != "" && c.g.checksums.support(alg) != ChecksumVerified {
		return s3err.New(http.StatusNotImplemented, "NotImplemented",
			"The trailer declared by x-amz-trailer is not supported.")
	}
	return nil
}

// setChecksumHeaders and reportChecksums are the only ways a response
// shows checksums, so the policy's filter cannot be bypassed
// (TestChecksumsReportedThroughPolicy).
func (c *opCtx) setChecksumHeaders(h http.Header, v checksum.Values, checksumType string) {
	checksum.SetHeaders(h, c.g.checksums.offered(v), checksumType)
}

func (c *opCtx) reportChecksums(v checksum.Values) s3xml.Checksums {
	return xmlChecksums(c.g.checksums.offered(v))
}

// reportAlgorithm is an algorithm name as a response shows it: "" for one
// the service does not offer.
func (c *opCtx) reportAlgorithm(name string) string {
	if name == "" || c.g.checksums.support(name) == ChecksumRefused {
		return ""
	}
	return name
}

// reportAlgorithms filters a listing entry's algorithm names.
func (c *opCtx) reportAlgorithms(algs []types.ChecksumAlgorithm) []string {
	var result []string
	for _, a := range algs {
		if name := c.reportAlgorithm(string(a)); name != "" {
			result = append(result, name)
		}
	}
	return result
}
