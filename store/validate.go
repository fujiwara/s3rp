package store

import (
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode/utf8"
)

// Name charsets are gateway invariants, not cosmetics, so every Store
// implementation must enforce them where definitions are written (the
// gateway does not re-check per request):
//
//   - A bucket name is an S3-style name, deliberately without ".": besides
//     feeding path-style routing (the request path splits on "/") and
//     "bucket/key" policy resources, a bucket name must stay usable as a
//     single DNS label for virtual-hosted-style addressing, and a dotted
//     name breaks there — "my.bucket.s3.example.com" is not covered by a
//     "*.s3.example.com" wildcard certificate. AWS allows dots but
//     documents the same breakage and recommends against them; refusing
//     them outright costs nothing at creation time and removes the trap.
//   - Tenant and user names share the charset of policy principals
//     ("tenant/user" — see the policy package's principal validation, which
//     this must stay in sync with). A name outside it could never be
//     written in a Principal element: its grants and denials would be
//     unexpressible, which is a dead end, not a choice.
var (
	bucketNameRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)
	// tenant and user names deliberately share one charset: a principal is
	// "tenant/user", so both halves must be writable in a policy. A digit
	// may lead (an account-ID-style all-numeric tenant name is valid); "-"
	// and "_" may not.
	principalPartRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]+$`)
)

// ValidateBucketName checks that name is a valid front-side bucket name.
func ValidateBucketName(name string) error {
	if !bucketNameRegexp.MatchString(name) {
		return fmt.Errorf("invalid bucket name %q", name)
	}
	return nil
}

// ValidateTenantName checks that name is a valid tenant name — one a
// policy principal ("tenant/user") can carry.
func ValidateTenantName(name string) error {
	if !principalPartRegexp.MatchString(name) {
		return fmt.Errorf("invalid tenant name %q", name)
	}
	return nil
}

// ValidateUserName checks that name is a valid user name — one a policy
// principal ("tenant/user") can carry.
func ValidateUserName(name string) error {
	if !principalPartRegexp.MatchString(name) {
		return fmt.Errorf("invalid user name %q", name)
	}
	return nil
}

// Validate checks a backend definition: the endpoint is either empty
// (Amazon S3, resolved by the SDK from the region) or an http(s) URL, and
// static credentials are set together (both empty = the SDK default
// credential chain). Like SetDefaults, it is the Store implementation's to
// call where definitions are written; a backend that fails it does not
// produce a working client, just later and less clearly.
func (b *Backend) Validate() error {
	if b.Endpoint != "" {
		u, err := url.Parse(b.Endpoint)
		if err != nil {
			return fmt.Errorf("invalid backend endpoint: %w", err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return fmt.Errorf("backend endpoint must be an http(s) URL: %s", b.Endpoint)
		}
	}
	if (b.AccessKeyID == "") != (b.SecretAccessKey == "") {
		return fmt.Errorf("backend access_key_id and secret_access_key must be set together")
	}
	return nil
}

// S3's limits on a bucket's tag set.
const (
	MaxBucketTags       = 50
	MaxTagKeyLength     = 128
	MaxTagValueLength   = 256
	reservedTagKeyStart = "aws:"
)

// tagCharsRegexp is the character class S3 allows in a tag key or value:
// letters, numbers, spaces and + - = . _ : / @ (the class the gateway
// holds object tags to). Besides being S3's rule it keeps out what a tag
// could not survive being served with — invalid UTF-8 and the characters
// XML forbids, which the response would carry as U+FFFD.
var tagCharsRegexp = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)

// ValidateBucketTags checks a bucket's tag set against the limits S3 puts
// on one: at most 50 tags, a key of 1 to 128 characters that does not
// start with "aws:", a value of at most 256, both of letters, numbers,
// spaces and + - = . _ : / @.
func ValidateBucketTags(tags map[string]string) error {
	if len(tags) > MaxBucketTags {
		return fmt.Errorf("%d bucket tags, at most %d are allowed", len(tags), MaxBucketTags)
	}
	for k, v := range tags {
		if n := utf8.RuneCountInString(k); n < 1 || n > MaxTagKeyLength {
			return fmt.Errorf("bucket tag key %q: must be 1 to %d characters", k, MaxTagKeyLength)
		}
		if strings.HasPrefix(strings.ToLower(k), reservedTagKeyStart) {
			return fmt.Errorf("bucket tag key %q: the %q prefix is reserved", k, reservedTagKeyStart)
		}
		if utf8.RuneCountInString(v) > MaxTagValueLength {
			return fmt.Errorf("bucket tag %q: value longer than %d characters", k, MaxTagValueLength)
		}
		if !utf8.ValidString(k) || !utf8.ValidString(v) || !tagCharsRegexp.MatchString(k) || !tagCharsRegexp.MatchString(v) {
			return fmt.Errorf("bucket tag %q: only letters, numbers, spaces and + - = . _ : / @ are allowed", k)
		}
	}
	return nil
}

// ValidateLifecycleText checks that text can be served as a bucket's
// lifecycle configuration: a well-formed XML document whose root element
// is LifecycleConfiguration. The rules themselves are not checked — the
// gateway serves the document without reading it, and what a rule may say
// is for whoever enforces it — so this only keeps a client from being
// handed something its parser refuses.
func ValidateLifecycleText(text string) error {
	return validateXMLDocument(text, "LifecycleConfiguration")
}

// ValidateReplicationText is ValidateLifecycleText for a bucket's
// replication configuration: a well-formed XML document whose root element
// is ReplicationConfiguration.
func ValidateReplicationText(text string) error {
	return validateXMLDocument(text, "ReplicationConfiguration")
}

func validateXMLDocument(text, root string) error {
	dec := xml.NewDecoder(strings.NewReader(text))
	depth, seen := 0, false
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("invalid %s document: %w", root, err)
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if depth == 0 {
				if seen || t.Name.Local != root {
					return fmt.Errorf("invalid %s document: root element is %s", root, t.Name.Local)
				}
				seen = true
			}
			// the decoder lets a repeated attribute through, which no
			// XML parser a client uses does
			for i, a := range t.Attr {
				for _, b := range t.Attr[:i] {
					if a.Name == b.Name {
						return fmt.Errorf("invalid %s document: attribute %s repeated on %s", root, a.Name.Local, t.Name.Local)
					}
				}
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			// XML's own white space only: TrimSpace would also pass U+00A0
			if depth == 0 && strings.Trim(string(t), " \t\r\n") != "" {
				return fmt.Errorf("invalid %s document: text outside the root element", root)
			}
		}
	}
	if !seen {
		return fmt.Errorf("invalid %s document: no root element", root)
	}
	return nil
}
