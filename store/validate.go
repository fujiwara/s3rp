package store

import (
	"fmt"
	"net/url"
	"regexp"
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
