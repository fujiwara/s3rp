package s3gw

import (
	"net/http"
	"strings"
)

// signedHeader reads request headers in view of what the SigV4 signature
// covers. Operation handlers must not read r.Header directly: which method a
// value is read through records — and enforces — whether it is allowed to
// carry semantics beyond the object's own attributes.
// TestNoDirectRequestHeaderReads keeps direct reads out of the operation
// files.
//
// The x-amz-checksum-* headers are the one deliberate exception to the
// no-direct-reads rule: checksum.FromHeaders / checksum.TrailerAlgorithm take
// the http.Header wholesale (the package is a shared leaf), which is safe
// because the verifier refuses any request carrying an unsigned x-amz-*
// header, so every x-amz-* value present is signature-covered.
type signedHeader struct {
	h      http.Header
	signed map[string]bool // lower-cased names whose values the signature covers
}

// newSignedHeader pairs a request's headers with the verified signature's
// coverage set. A nil set (POST policy uploads, which have no signed headers)
// makes Signed answer "" for everything, which is exactly right: nothing a
// POST request carries as a header is signature-covered.
func newSignedHeader(r *http.Request, signed map[string]bool) signedHeader {
	return signedHeader{h: r.Header, signed: signed}
}

// postFieldHeader wraps a POST upload's form fields as a signedHeader on
// which every value is covered, so the checks shared with the header paths
// (checkSSE, applySSE) run unchanged over fields. The coverage claim is the
// POST policy's two-way rule — every submitted field must be bound by a
// condition of the signed policy document — so this must only be built after
// verifyPostRequest has succeeded.
func postFieldHeader(fields map[string]string) signedHeader {
	h := make(http.Header, len(fields))
	signed := make(map[string]bool, len(fields))
	for k, v := range fields {
		h.Set(k, v)
		signed[strings.ToLower(k)] = true
	}
	return signedHeader{h: h, signed: signed}
}

// Signed returns the header's value when the signature covers it, and ""
// otherwise. Every value that drives authorization, resource selection or
// integrity must be read through this. For x-amz-* names the verifier has
// already refused any request carrying an unsigned one, so "" simply means
// absent; the membership check here is defense in depth against that gate
// regressing, not the primary enforcement.
func (s signedHeader) Signed(name string) string {
	if !s.signed[strings.ToLower(name)] {
		return ""
	}
	return s.h.Get(name)
}

// Attribute returns the header's value whether or not it was signed. For
// values that become the object's own attributes (Content-Type,
// Cache-Control, ...) or shape a read (Range, If-*): S3 requires only
// x-amz-* headers to be signed — presigners deliberately leave Content-Type
// off a presigned PUT — and an unsigned value here cannot exceed what the
// signature granted. Refusal checks (SSE-C, canned ACLs) also read through
// this, deliberately: treating an unsigned copy of a refused header as
// absent would be exactly the silent drop the refusal exists to prevent.
func (s signedHeader) Attribute(name string) string {
	return s.h.Get(name)
}

// AttributeValues is Attribute for a header whose absence must stay
// distinguishable from an empty value (Content-MD5).
func (s signedHeader) AttributeValues(name string) []string {
	return s.h.Values(name)
}

// SignedValues is Signed for a header sent with multiple values
// (x-amz-object-attributes). Nil when the signature does not cover it.
func (s signedHeader) SignedValues(name string) []string {
	if !s.signed[strings.ToLower(name)] {
		return nil
	}
	return s.h.Values(name)
}

// AmzMeta collects the x-amz-meta-* headers into the metadata map an upload
// stores. Metadata is covered by the unsigned-x-amz-* gate like any other
// x-amz-* header; the signed-set check mirrors Signed's defense in depth.
func (s signedHeader) AmzMeta() map[string]string {
	md := make(map[string]string)
	for k, vs := range s.h {
		lk := strings.ToLower(k)
		if name, ok := strings.CutPrefix(lk, "x-amz-meta-"); ok && len(vs) > 0 && s.signed[lk] {
			md[name] = vs[0]
		}
	}
	return md
}
