package s3rp_test

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/fujiwara/s3rp"
)

const (
	testAccessKeyID     = "S3RPTESTKEY001"
	testSecretAccessKey = "testsecret001"
	emptyPayloadHash    = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

func newTestApp(t *testing.T) *s3rp.S3RP {
	t.Helper()
	cfg := &s3rp.Config{
		Tenants: []*s3rp.TenantConfig{
			{
				Name: "testtenant",
				Users: []*s3rp.UserConfig{
					{
						Name: "testuser",
						Keys: []*s3rp.KeyConfig{
							{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey},
						},
					},
				},
				Buckets: []*s3rp.BucketConfig{
					{
						Name: "testbucket",
						Backend: &s3rp.BackendConfig{
							Endpoint:        "http://backend.invalid",
							Bucket:          "backend-testbucket",
							AccessKeyID:     "backendkey",
							SecretAccessKey: "backendsecret",
						},
					},
				},
			},
		},
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := s3rp.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return app
}

func testCreds() aws.Credentials {
	return aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey}
}
