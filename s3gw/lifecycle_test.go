package s3gw_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/s3op"
)

const expireLogsLifecycle = `<?xml version="1.0" encoding="UTF-8"?>
<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Rule>
    <ID>expire-logs</ID>
    <Filter><Prefix>logs/</Prefix></Filter>
    <Status>Enabled</Status>
    <Expiration><Days>30</Days></Expiration>
  </Rule>
</LifecycleConfiguration>`

// a foreign tenant may read the configuration only when the policy says so
const lifecycleReadableByTenantB = `{
  "Statement": [
    {"Effect": "Allow", "Principal": {"S3RP": ["tenant-b/bob"]}, "Action": "s3:GetLifecycleConfiguration", "Resource": "shared"}
  ]
}`

func newLifecycleProxy(t *testing.T) (map[string]*s3.Client, *s3gw.Gateway) {
	t.Helper()
	owners := []userSpec{
		{name: "alice", keyID: "ALICEKEY", secret: "alicesecret"},
		{name: "writer", keyID: "WRITERKEY", secret: "writersecret", policy: []policy.ActionStatement{
			{Effect: "Allow", Action: []string{"s3:PutObject"}},
		}},
	}
	foreign := []userSpec{
		{name: "bob", keyID: "BOBKEY", secret: "bobsecret"},
		{name: "carol", keyID: "CAROLKEY", secret: "carolsecret"},
	}
	m := buildStore(t, "tenant-a", owners, []bucketSpec{
		{name: "ruled", lifecycle: expireLogsLifecycle},
		{name: "shared", lifecycle: expireLogsLifecycle, policyText: lifecycleReadableByTenantB},
		{name: "plain"},
	})
	m.addTenant(t, "tenant-b", foreign, nil)
	// a backend that is called fails the test: the answer is the store's
	gw := gatewayFor(t, m, nil)
	return clientsFor(t, gw, append(owners, foreign...)), gw
}

func TestGetBucketLifecycleConfiguration(t *testing.T) {
	clients, _ := newLifecycleProxy(t)
	ctx := t.Context()

	out, err := clients["alice"].GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("ruled")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(out.Rules))
	}
	r := out.Rules[0]
	if aws.ToString(r.ID) != "expire-logs" || r.Status != types.ExpirationStatusEnabled ||
		r.Filter == nil || aws.ToString(r.Filter.Prefix) != "logs/" ||
		r.Expiration == nil || aws.ToInt32(r.Expiration.Days) != 30 {
		t.Errorf("the stored rule did not come back: %+v", r)
	}

	_, err = clients["alice"].GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String("plain")})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NoSuchLifecycleConfiguration" ||
		apiErr.ErrorMessage() != "The lifecycle configuration does not exist" {
		t.Errorf("a bucket without a configuration: got %v", err)
	}
}

func TestGetBucketLifecycleConfigurationAuthorized(t *testing.T) {
	clients, _ := newLifecycleProxy(t)
	get := func(user, bucket string) error {
		_, err := clients[user].GetBucketLifecycleConfiguration(t.Context(), &s3.GetBucketLifecycleConfigurationInput{Bucket: aws.String(bucket)})
		return err
	}
	// the user policy allows s3:PutObject only
	expectAccessDenied(t, get("writer", "ruled"))
	// a foreign tenant reads it with a grant, and not without
	if err := get("bob", "shared"); err != nil {
		t.Errorf("bob has a grant: %v", err)
	}
	expectAccessDenied(t, get("carol", "shared"))
	expectAccessDenied(t, get("bob", "ruled"))
}

func TestBucketLifecycleWritesRefused(t *testing.T) {
	clients, gw := newLifecycleProxy(t)
	var last *s3gw.RequestInfo
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { last = info })
	bucket := aws.String("ruled")
	for name, call := range map[string]func() error{
		s3op.OpPutBucketLifecycleConfiguration: func() error {
			_, err := clients["alice"].PutBucketLifecycleConfiguration(t.Context(), &s3.PutBucketLifecycleConfigurationInput{
				Bucket: bucket,
				LifecycleConfiguration: &types.BucketLifecycleConfiguration{Rules: []types.LifecycleRule{{
					Status: types.ExpirationStatusEnabled, Filter: &types.LifecycleRuleFilter{Prefix: aws.String("")},
					Expiration: &types.LifecycleExpiration{Days: aws.Int32(1)},
				}}},
			})
			return err
		},
		s3op.OpDeleteBucketLifecycle: func() error {
			_, err := clients["alice"].DeleteBucketLifecycle(t.Context(), &s3.DeleteBucketLifecycleInput{Bucket: bucket})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			var apiErr smithy.APIError
			if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NotImplemented" {
				t.Fatalf("expect NotImplemented, got %v", err)
			}
			if last == nil || last.Route != name {
				t.Errorf("the attempt is recorded as %+v, want route %s", last, name)
			}
		})
	}
}
