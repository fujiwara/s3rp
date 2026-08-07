package store_test

import (
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/store"
)

func TestValidateBucketName(t *testing.T) {
	valid := []string{"photos", "my-bucket", "0abc", "abc0"}
	for _, name := range valid {
		if err := store.ValidateBucketName(name); err != nil {
			t.Errorf("bucket name %q must be valid: %v", name, err)
		}
	}
	// "my.bucket" is rejected on purpose: a dotted name is not a single
	// DNS label, which breaks virtual-hosted-style addressing under a
	// wildcard certificate
	invalid := []string{
		"", "ab", "Photos", "-bucket", "bucket-", ".bucket", "my.bucket",
		"my_bucket", "my/bucket", strings.Repeat("a", 64),
	}
	for _, name := range invalid {
		if err := store.ValidateBucketName(name); err == nil {
			t.Errorf("bucket name %q must be rejected", name)
		}
	}
}

// Tenant and user names share the charset of policy principals
// ("tenant/user"): a name outside it could never be written in a Principal
// element.
func TestValidatePrincipalPartNames(t *testing.T) {
	valid := []string{"acme", "tenant-b", "some_user", "t2", "0tenant", "123456789012"}
	invalid := []string{"", "a", "0", "Acme", "-tenant", "_tenant", "my.tenant", "my/tenant", "my tenant"}
	for _, name := range valid {
		if err := store.ValidateTenantName(name); err != nil {
			t.Errorf("tenant name %q must be valid: %v", name, err)
		}
		if err := store.ValidateUserName(name); err != nil {
			t.Errorf("user name %q must be valid: %v", name, err)
		}
	}
	for _, name := range invalid {
		if err := store.ValidateTenantName(name); err == nil {
			t.Errorf("tenant name %q must be rejected", name)
		}
		if err := store.ValidateUserName(name); err == nil {
			t.Errorf("user name %q must be rejected", name)
		}
	}
}

func TestBackendValidate(t *testing.T) {
	cases := []struct {
		name    string
		backend *store.Backend
		errStr  string // empty = valid
	}{
		{"aws with credential chain", &store.Backend{}, ""},
		{"http endpoint", &store.Backend{Endpoint: "http://s3.example.com:9000", AccessKeyID: "a", SecretAccessKey: "s"}, ""},
		{"https endpoint", &store.Backend{Endpoint: "https://s3.example.com"}, ""},
		{"ftp endpoint", &store.Backend{Endpoint: "ftp://s3.example.com"}, "http(s)"},
		{"missing scheme", &store.Backend{Endpoint: "s3.example.com:9000"}, "http(s)"},
		{"key without secret", &store.Backend{AccessKeyID: "a"}, "must be set together"},
		{"secret without key", &store.Backend{SecretAccessKey: "s"}, "must be set together"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.backend.Validate()
			if tc.errStr == "" {
				if err != nil {
					t.Errorf("expect valid, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errStr) {
				t.Errorf("expect error containing %q, got %v", tc.errStr, err)
			}
		})
	}
}
