package s3gw

import (
	"net/http"
	"sort"
	"strings"

	"github.com/fujiwara/s3rp/s3err"
)

// hdrStorageClass names the storage class an upload asks for. It is read
// through Signed: the class decides where the object physically lands and
// what it costs, which is semantics beyond the object's own attributes.
const hdrStorageClass = "x-amz-storage-class"

// hdrCopySource is named because two distant sites must agree on it: the PUT
// dispatch branches on its presence (handler.go) and the copy operation
// parses it (copy.go). Were they to disagree, a copy would silently become a
// plain upload of an empty body rather than fail.
const hdrCopySource = "x-amz-copy-source"

// the upload headers that add an action to the authorization (handler.go
// uploadActions); the values are applied to the backend input by
// proxy.go / multipart.go / copy.go and objectlock.go
const (
	hdrTagging               = "x-amz-tagging"
	hdrObjectLockMode        = "x-amz-object-lock-mode"
	hdrObjectLockRetainUntil = "x-amz-object-lock-retain-until-date"
	hdrObjectLockLegalHold   = "x-amz-object-lock-legal-hold"
)

// knownAmzHeaders is every x-amz-* request header some operation honors or
// refuses by name. checkKnownAmzHeaders refuses any other with
// NotImplemented — the header-path counterpart of the 501 for an unknown
// query subresource and for an unknown POST form field: a header the
// gateway would otherwise ignore is a request it cannot honor, and ignoring
// it is how an encryption context, a redirect location or a grant would be
// dropped while the client believes it applied. The verifier's
// unsigned-header gate runs first, so everything checked here is signed.
//
// A header handled on one route is known on every route (x-amz-tagging on a
// GET is ignored, as on Amazon S3); what matters is that no header the
// gateway never reads gets through. TestKnownAmzHeadersCoverSource keeps
// the list in step with the headers the operation files read.
var knownAmzHeaders = map[string]bool{
	// consumed by the verifier
	"x-amz-date": true, "x-amz-content-sha256": true, "x-amz-security-token": true,
	"x-amz-decoded-content-length": true, "x-amz-trailer": true,
	// sent by the browser SDKs
	"x-amz-user-agent": true,
	// checksums (checksum.FromHeaders / TrailerAlgorithm)
	"x-amz-checksum-algorithm": true, "x-amz-checksum-type": true, "x-amz-checksum-mode": true,
	"x-amz-checksum-crc32": true, "x-amz-checksum-crc32c": true, "x-amz-checksum-crc64nvme": true,
	"x-amz-checksum-sha1": true, "x-amz-checksum-sha256": true, "x-amz-sdk-checksum-algorithm": true,
	// object writes and copies
	hdrStorageClass: true, hdrTagging: true, "x-amz-tagging-directive": true, "x-amz-metadata-directive": true,
	hdrCopySource: true, "x-amz-copy-source-if-match": true, "x-amz-copy-source-if-none-match": true,
	"x-amz-copy-source-if-modified-since": true, "x-amz-copy-source-if-unmodified-since": true,
	"x-amz-copy-source-range": true, "x-amz-mp-object-size": true,
	// encryption (sse.go); the SSE-C family is deliberately absent so a
	// stray customer-key header meets the 501 even without its algorithm
	hdrSSE: true, hdrSSEKMSKeyID: true,
	// ACLs, refused by name (acl.go)
	"x-amz-acl": true, "x-amz-grant-read": true, "x-amz-grant-write": true,
	"x-amz-grant-read-acp": true, "x-amz-grant-write-acp": true, "x-amz-grant-full-control": true,
	// Object Lock
	hdrObjectLockMode: true, hdrObjectLockRetainUntil: true, hdrObjectLockLegalHold: true,
	"x-amz-bypass-governance-retention": true,
	// reads and conditional deletes
	"x-amz-object-attributes": true, "x-amz-max-parts": true, "x-amz-part-number-marker": true,
	"x-amz-if-match-size": true, "x-amz-if-match-last-modified-time": true,
}

// checkKnownAmzHeaders refuses a request carrying an x-amz-* header no
// operation handles (see knownAmzHeaders); x-amz-meta-* is user metadata and
// always known. It runs on every authenticated entry path — handleRequest
// after the signature verifies, handlePostObject after the policy does —
// and looks at names only, so the coverage set is irrelevant to it.
func (s signedHeader) checkKnownAmzHeaders() *s3err.Error {
	var unknown []string
	for name := range s.h {
		lname := strings.ToLower(name)
		if !strings.HasPrefix(lname, "x-amz-") || knownAmzHeaders[lname] || strings.HasPrefix(lname, amzMetaPrefix) {
			continue
		}
		unknown = append(unknown, lname)
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return s3err.NotImplemented("header " + strings.Join(unknown, ", "))
}

// amzMetaPrefix is the user-metadata header prefix, matched rather than
// compared: a mistyped prefix would not fail a lookup loudly but would
// silently stop collecting metadata (AmzMeta) or stop covering fields the
// POST policy must bind (postobject.go).
const amzMetaPrefix = "x-amz-meta-"

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
// stores, and is nil when the request carries none — Op.Request uses that to
// tell "no metadata" from "empty metadata". Metadata is covered by the
// unsigned-x-amz-* gate like any other x-amz-* header; the signed-set check
// mirrors Signed's defense in depth.
func (s signedHeader) AmzMeta() map[string]string {
	var md map[string]string
	for k, vs := range s.h {
		lk := strings.ToLower(k)
		if name, ok := strings.CutPrefix(lk, amzMetaPrefix); ok && len(vs) > 0 && s.signed[lk] {
			if md == nil {
				md = make(map[string]string)
			}
			md[name] = vs[0]
		}
	}
	return md
}
