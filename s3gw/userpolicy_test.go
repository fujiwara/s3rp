package s3gw_test

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp/policy"
)

// newUserPolicyProxy builds a proxy with a read-only user (Get*/List* only),
// an unrestricted admin user, and one bucket carrying a policy that denies
// GetObject, to exercise the user-policy / bucket-policy combination.
func newUserPolicyProxy(t *testing.T, stub *stubBackend) map[string]*s3.Client {
	t.Helper()
	users := []userSpec{
		{
			name: "readonly", keyID: "ROKEY", secret: "rosecret",
			policy: []policy.ActionStatement{
				{Effect: "Allow", Action: []string{"s3:Get*", "s3:List*", "s3:HeadObject", "s3:HeadBucket"}},
			},
		},
		{name: "admin", keyID: "ADMINKEY", secret: "adminsecret"},
	}
	buckets := []bucketSpec{
		{name: "data"},
		{name: "noget", policyText: `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"noget/*"}]}`},
	}
	gw := gatewayFor(t, buildStore(t, "acme", users, buckets), stub)
	return clientsFor(t, gw, users)
}

func expectDenied(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expect AccessDenied, got success")
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "AccessDenied" {
		t.Errorf("expect AccessDenied, got %v", err)
	}
}

func TestUserPolicyReadOnly(t *testing.T) {
	stub := &stubBackend{
		getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("x"))},
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
		delOut: &s3.DeleteObjectOutput{},
	}
	clients := newUserPolicyProxy(t, stub)
	ctx := t.Context()

	// readonly may read but not write
	if out, err := clients["readonly"].GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("data"), Key: aws.String("k"),
	}); err != nil {
		t.Errorf("readonly get must succeed: %v", err)
	} else {
		out.Body.Close()
	}
	_, err := clients["readonly"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("data"), Key: aws.String("k"), Body: strings.NewReader("x"),
	})
	expectDenied(t, err)
	_, err = clients["readonly"].DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("data"), Key: aws.String("k"),
	})
	expectDenied(t, err)

	// admin (no user policy) may do everything
	if _, err := clients["admin"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("data"), Key: aws.String("k"), Body: strings.NewReader("x"),
	}); err != nil {
		t.Errorf("admin put must succeed: %v", err)
	}
}

// TestUserPolicyAndBucketPolicy verifies both layers apply: a user allowed
// by their identity policy can still be denied by the bucket policy.
func TestUserPolicyAndBucketPolicy(t *testing.T) {
	stub := &stubBackend{getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("x"))}}
	clients := newUserPolicyProxy(t, stub)
	// readonly's user policy allows s3:GetObject, but the noget bucket
	// policy denies it -> still denied
	_, err := clients["readonly"].GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("noget"), Key: aws.String("k"),
	})
	expectDenied(t, err)
	// admin is also denied on noget (bucket policy applies to all users)
	_, err = clients["admin"].GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("noget"), Key: aws.String("k"),
	})
	expectDenied(t, err)
}
