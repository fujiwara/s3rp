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
	if len(cfg.Tenants) != 2 {
		t.Fatalf("expect 2 tenants, got %d", len(cfg.Tenants))
	}
	acme := cfg.Tenants[0]
	if acme.Name != "acme" {
		t.Errorf("expect acme, got %s", acme.Name)
	}
	if len(acme.Users) != 2 || len(acme.Buckets) != 2 {
		t.Fatalf("expect 2 users and 2 buckets, got %d users %d buckets", len(acme.Users), len(acme.Buckets))
	}
	if acme.Users[0].Name != "app1" || len(acme.Users[0].Keys) != 2 {
		t.Errorf("unexpected user %+v", acme.Users[0])
	}
	photos := acme.Buckets[0]
	if photos.Backend.AccessKeyID != "backendkey" {
		t.Errorf("env not expanded: %s", photos.Backend.AccessKeyID)
	}
	if photos.Backend.SecretAccessKey.String() != "backendsecret" {
		t.Errorf("env not expanded: %s", photos.Backend.SecretAccessKey.String())
	}
	if photos.Backend.Bucket != "photos-prod" {
		t.Errorf("expect photos-prod, got %s", photos.Backend.Bucket)
	}
	logs := acme.Buckets[1]
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
		name:   "no tenants",
		yaml:   `listen: ":8080"`,
		errStr: "no tenants",
	},
	{
		name: "duplicate tenant",
		yaml: `
tenants:
  - name: foo
    users: [{name: user1, keys: [{access_key_id: k1, secret_access_key: s}]}]
    buckets:
      - name: bucket1
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
  - name: foo
    users: [{name: user1, keys: [{access_key_id: k2, secret_access_key: s}]}]
    buckets:
      - name: bucket2
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
`,
		errStr: "duplicate tenant name",
	},
	{
		name: "duplicate access key id across tenants",
		yaml: `
tenants:
  - name: foo
    users: [{name: user1, keys: [{access_key_id: k, secret_access_key: s}]}]
    buckets:
      - name: bucket1
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
  - name: bar
    users: [{name: user1, keys: [{access_key_id: k, secret_access_key: s}]}]
    buckets:
      - name: bucket2
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
`,
		errStr: "duplicate access_key_id",
	},
	{
		name: "duplicate bucket name across tenants",
		yaml: `
tenants:
  - name: foo
    users: [{name: user1, keys: [{access_key_id: k1, secret_access_key: s}]}]
    buckets:
      - name: shared
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
  - name: bar
    users: [{name: user1, keys: [{access_key_id: k2, secret_access_key: s}]}]
    buckets:
      - name: shared
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
`,
		errStr: "duplicate bucket name",
	},
	{
		name: "invalid bucket name",
		yaml: `
tenants:
  - name: foo
    users: [{name: user1, keys: [{access_key_id: k, secret_access_key: s}]}]
    buckets:
      - name: "Invalid_Bucket"
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
`,
		errStr: "invalid bucket name",
	},
	{
		name: "no users",
		yaml: `
tenants:
  - name: foo
    buckets:
      - name: bucket1
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
`,
		errStr: "at least one user",
	},
	{
		name: "invalid user name",
		yaml: `
tenants:
  - name: foo
    users: [{name: "User_1", keys: [{access_key_id: k, secret_access_key: s}]}]
    buckets:
      - name: bucket1
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
`,
		errStr: "invalid user name",
	},
	{
		name: "duplicate user name",
		yaml: `
tenants:
  - name: foo
    users:
      - {name: user1, keys: [{access_key_id: k1, secret_access_key: s}]}
      - {name: user1, keys: [{access_key_id: k2, secret_access_key: s}]}
    buckets:
      - name: bucket1
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
`,
		errStr: "duplicate user name",
	},
	{
		name: "user without keys",
		yaml: `
tenants:
  - name: foo
    users: [{name: user1}]
    buckets:
      - name: bucket1
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
`,
		errStr: "at least one key",
	},
	{
		name: "no buckets",
		yaml: `
tenants:
  - name: foo
    users: [{name: user1, keys: [{access_key_id: k, secret_access_key: s}]}]
`,
		errStr: "at least one bucket",
	},
	{
		name: "invalid endpoint scheme",
		yaml: `
tenants:
  - name: foo
    users: [{name: user1, keys: [{access_key_id: k, secret_access_key: s}]}]
    buckets:
      - name: bucket1
        backend: {endpoint: "ftp://b.example.com", access_key_id: a, secret_access_key: s}
`,
		errStr: "http(s)",
	},
	{
		name: "malformed policy json",
		yaml: `
tenants:
  - name: foo
    users: [{name: user1, keys: [{access_key_id: k, secret_access_key: s}]}]
    buckets:
      - name: bucket1
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
        policy: "{not json"
`,
		errStr: "invalid policy",
	},
	{
		name: "policy resource refers to another bucket",
		yaml: `
tenants:
  - name: foo
    users: [{name: user1, keys: [{access_key_id: k, secret_access_key: s}]}]
    buckets:
      - name: bucket1
        backend: {endpoint: http://b.example.com, access_key_id: a, secret_access_key: s}
        policy: |
          {"Statement": [{"Effect": "Deny", "Principal": "*", "Action": "s3:PutObject", "Resource": "otherbucket/*"}]}
`,
		errStr: "does not refer to this bucket",
	},
	{
		name: "credentials not set together",
		yaml: `
tenants:
  - name: foo
    users: [{name: user1, keys: [{access_key_id: k, secret_access_key: s}]}]
    buckets:
      - name: bucket1
        backend: {endpoint: http://b.example.com, access_key_id: a}
`,
		errStr: "must be set together",
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
tenants:
  - name: foo
    users: [{name: user-1_a, keys: [{access_key_id: k, secret_access_key: s}]}]
    buckets:
      - name: bucket1
        backend:
          region: ap-northeast-1
`); err != nil {
		t.Fatal(err)
	}
	cfg, err := s3rp.LoadConfig(f)
	if err != nil {
		t.Fatal(err)
	}
	b := cfg.Tenants[0].Buckets[0].Backend
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
