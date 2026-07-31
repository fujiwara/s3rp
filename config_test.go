package s3rp_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp"
	"github.com/google/go-cmp/cmp"
)

func TestLoadConfig(t *testing.T) {
	t.Setenv("TEST_BACKEND_ACCESS_KEY_ID", "backendkey")
	t.Setenv("TEST_BACKEND_SECRET_ACCESS_KEY", "backendsecret")
	cfg, err := s3rp.LoadConfig("testdata/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Listen != ":8080" {
		t.Errorf("expect :8080, got %s", cfg.Listen)
	}
	if len(cfg.Buckets) != 2 {
		t.Fatalf("expect 2 buckets, got %d", len(cfg.Buckets))
	}
	photos := cfg.Buckets[0]
	if photos.Backend.AccessKeyID != "backendkey" {
		t.Errorf("env not expanded: %s", photos.Backend.AccessKeyID)
	}
	if photos.Backend.SecretAccessKey.String() != "backendsecret" {
		t.Errorf("env not expanded: %s", photos.Backend.SecretAccessKey.String())
	}
	if photos.Backend.Bucket != "photos-prod" {
		t.Errorf("expect photos-prod, got %s", photos.Backend.Bucket)
	}
	logs := cfg.Buckets[1]
	// defaults
	if logs.Backend.Region != s3rp.DefaultRegion {
		t.Errorf("expect default region, got %s", logs.Backend.Region)
	}
	if logs.Backend.Bucket != "logs" {
		t.Errorf("expect bucket name fallback, got %s", logs.Backend.Bucket)
	}
	if logs.Backend.UsePathStyle == nil || !*logs.Backend.UsePathStyle {
		t.Error("expect use_path_style default true")
	}
}

var validateTestCases = []struct {
	name   string
	yaml   string
	errStr string
}{
	{
		name:   "no buckets",
		yaml:   `listen: ":8080"`,
		errStr: "no buckets",
	},
	{
		name: "duplicate bucket",
		yaml: `
buckets:
  - name: foo
    backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
    keys: [{access_key_id: k, secret_access_key: s}]
  - name: foo
    backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
    keys: [{access_key_id: k, secret_access_key: s}]
`,
		errStr: "duplicate bucket name",
	},
	{
		name: "invalid bucket name",
		yaml: `
buckets:
  - name: "Invalid_Bucket"
    backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
    keys: [{access_key_id: k, secret_access_key: s}]
`,
		errStr: "invalid bucket name",
	},
	{
		name: "credentials not set together",
		yaml: `
buckets:
  - name: foo
    backend: {endpoint: http://b.example.com, access_key_id: a}
    keys: [{access_key_id: k, secret_access_key: s}]
`,
		errStr: "must be set together",
	},
	{
		name: "invalid endpoint scheme",
		yaml: `
buckets:
  - name: foo
    backend: {endpoint: "ftp://b.example.com", access_key_id: a, secret_access_key: s}
    keys: [{access_key_id: k, secret_access_key: s}]
`,
		errStr: "http(s)",
	},
	{
		name: "no keys",
		yaml: `
buckets:
  - name: foo
    backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
`,
		errStr: "at least one key",
	},
	{
		name: "same key id different secrets",
		yaml: `
buckets:
  - name: foo
    backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
    keys: [{access_key_id: k, secret_access_key: s1}]
  - name: bar
    backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
    keys: [{access_key_id: k, secret_access_key: s2}]
`,
		errStr: "different secrets",
	},
}

func TestConfigValidate(t *testing.T) {
	dir := t.TempDir()
	for _, tc := range validateTestCases {
		t.Run(tc.name, func(t *testing.T) {
			f := dir + "/" + strings.ReplaceAll(tc.name, " ", "_") + ".yaml"
			if err := writeFile(t, f, tc.yaml); err != nil {
				t.Fatal(err)
			}
			_, err := s3rp.LoadConfig(f)
			if err == nil {
				t.Fatal("expect error, got nil")
			}
			if !strings.Contains(err.Error(), tc.errStr) {
				t.Errorf("expect error containing %q, got %q", tc.errStr, err.Error())
			}
		})
	}
}

// A backend without an endpoint means AWS S3: virtual-hosted style and the
// SDK default credential chain.
func TestConfigAWSBackendDefaults(t *testing.T) {
	dir := t.TempDir()
	f := dir + "/aws.yaml"
	if err := writeFile(t, f, `
buckets:
  - name: foo
    backend:
      region: ap-northeast-1
    keys: [{access_key_id: k, secret_access_key: s}]
`); err != nil {
		t.Fatal(err)
	}
	cfg, err := s3rp.LoadConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Buckets[0].Backend
	if b.UsePathStyle == nil || *b.UsePathStyle {
		t.Error("expect use_path_style default false without endpoint")
	}
	if b.Region != "ap-northeast-1" {
		t.Errorf("unexpected region %s", b.Region)
	}
}

func TestPasswordMasked(t *testing.T) {
	b, err := json.Marshal(struct {
		Secret s3rp.Password `json:"secret"`
		Empty  s3rp.Password `json:"empty"`
	}{Secret: "topsecret", Empty: ""})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"secret":"********","empty":""}`
	if diff := cmp.Diff(want, string(b)); diff != "" {
		t.Errorf("unexpected marshal result (-want +got):\n%s", diff)
	}
	if s3rp.Password("topsecret").String() != "topsecret" {
		t.Error("String() must return the raw value")
	}
}
