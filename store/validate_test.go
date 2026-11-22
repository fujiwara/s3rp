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

func TestValidateLifecycleText(t *testing.T) {
	for _, text := range []string{
		`<LifecycleConfiguration/>`,
		`<?xml version="1.0" encoding="UTF-8"?>
<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Rule><ID>a</ID><Status>Enabled</Status><Expiration><Days>1</Days></Expiration></Rule>
</LifecycleConfiguration>`,
	} {
		if err := store.ValidateLifecycleText(text); err != nil {
			t.Errorf("%q must be valid: %v", text, err)
		}
	}
	for name, text := range map[string]string{
		"empty":                ``,
		"not XML":              `{"Rules": []}`,
		"unclosed":             `<LifecycleConfiguration><Rule>`,
		"mismatched tags":      `<LifecycleConfiguration><Rule></LifecycleConfiguration>`,
		"another root":         `<CORSConfiguration/>`,
		"two roots":            `<LifecycleConfiguration/><LifecycleConfiguration/>`,
		"text after":           `<LifecycleConfiguration/>trailing`,
		"no-break space after": "<LifecycleConfiguration/>\u00a0",
		"repeated attribute":   `<LifecycleConfiguration><Rule ID="one" ID="two"/></LifecycleConfiguration>`,
	} {
		if err := store.ValidateLifecycleText(text); err == nil {
			t.Errorf("%s: %q must be rejected", name, text)
		}
	}
}

func TestValidateReplicationText(t *testing.T) {
	if err := store.ValidateReplicationText(`<ReplicationConfiguration><Role>r</Role></ReplicationConfiguration>`); err != nil {
		t.Errorf("must be valid: %v", err)
	}
	for name, text := range map[string]string{
		"empty":        ``,
		"another root": `<LifecycleConfiguration/>`,
		"unclosed":     `<ReplicationConfiguration><Rule>`,
	} {
		if err := store.ValidateReplicationText(text); err == nil {
			t.Errorf("%s: %q must be rejected", name, text)
		}
	}
}

func TestValidateBucketTags(t *testing.T) {
	many := map[string]string{}
	for i := range 51 {
		many[strings.Repeat("k", i+1)] = "v"
	}
	for name, tags := range map[string]map[string]string{
		"none":          nil,
		"plain":         {"env": "prod", "team": "storage"},
		"empty value":   {"env": ""},
		"longest":       {strings.Repeat("k", 128): strings.Repeat("v", 256)},
		"other scripts": {"名前": "値"},
		"every symbol":  {"a+b-c=d.e_f:g/h@i": "x y"},
	} {
		if err := store.ValidateBucketTags(tags); err != nil {
			t.Errorf("%s must be valid: %v", name, err)
		}
	}
	for name, tags := range map[string]map[string]string{
		"empty key":      {"": "v"},
		"key too long":   {strings.Repeat("k", 129): "v"},
		"value too long": {"k": strings.Repeat("v", 257)},
		"reserved key":   {"aws:created-by": "x"},
		// what the XML response could not carry unchanged
		"NUL in a value":      {"env": "a\x00b"},
		"NUL in a key":        {"e\x00v": "x"},
		"control character":   {"env": "a\x01b"},
		"invalid UTF-8":       {"env": "a\xffb"},
		"invalid UTF-8 key":   {"e\xffv": "x"},
		"noncharacter":        {"env": "a\ufffeb"},
		"markup":              {"env": "a<b"},
		"symbol outside S3's": {"env": "100%"},
		"too many":            many,
	} {
		if err := store.ValidateBucketTags(tags); err == nil {
			t.Errorf("%s must be rejected", name)
		}
	}
}

// One tag's rules are shared by a bucket's tags and the gateway's check of
// object tags.
func TestValidTag(t *testing.T) {
	for _, k := range []string{"env", "a+b-c=d.e_f:g/h@i", "名前", strings.Repeat("k", 128)} {
		if !store.ValidTagKey(k) {
			t.Errorf("key %q must be valid", k)
		}
	}
	for _, k := range []string{"", strings.Repeat("k", 129), "a<b", "e\x00v", "e\xffv", "😀", "e\u0301"} {
		if store.ValidTagKey(k) {
			t.Errorf("key %q must be invalid", k)
		}
	}
	for _, v := range []string{"", "x y", "café 東京", strings.Repeat("v", 256)} {
		if !store.ValidTagValue(v) {
			t.Errorf("value %q must be valid", v)
		}
	}
	for _, v := range []string{strings.Repeat("v", 257), "100%", "a\x00b", "a\xffb", "😀"} {
		if store.ValidTagValue(v) {
			t.Errorf("value %q must be invalid", v)
		}
	}
}

func TestValidateNotificationText(t *testing.T) {
	if err := store.ValidateNotificationText(`<NotificationConfiguration><EventBridgeConfiguration/></NotificationConfiguration>`); err != nil {
		t.Errorf("must be valid: %v", err)
	}
	for name, text := range map[string]string{
		"empty":        ``,
		"another root": `<ReplicationConfiguration/>`,
		"unclosed":     `<NotificationConfiguration><TopicConfiguration>`,
	} {
		if err := store.ValidateNotificationText(text); err == nil {
			t.Errorf("%s: %q must be rejected", name, text)
		}
	}
}

func TestValidateLoggingText(t *testing.T) {
	if err := store.ValidateLoggingText(`<BucketLoggingStatus><LoggingEnabled><TargetBucket>b</TargetBucket><TargetPrefix>p/</TargetPrefix></LoggingEnabled></BucketLoggingStatus>`); err != nil {
		t.Errorf("must be valid: %v", err)
	}
	for name, text := range map[string]string{
		"empty":        ``,
		"another root": `<NotificationConfiguration/>`,
		"unclosed":     `<BucketLoggingStatus><LoggingEnabled>`,
	} {
		if err := store.ValidateLoggingText(text); err == nil {
			t.Errorf("%s: %q must be rejected", name, text)
		}
	}
}
