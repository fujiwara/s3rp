package s3gw_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func TestProxyGetBucketAcl(t *testing.T) {
	client, _ := newTestProxy(t, &stubBackend{})
	out, err := client.GetBucketAcl(t.Context(), &s3.GetBucketAclInput{
		Bucket: aws.String("testbucket"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.Owner.ID) != "testtenant" {
		t.Errorf("expect owner testtenant, got %s", aws.ToString(out.Owner.ID))
	}
	if len(out.Grants) != 1 {
		t.Fatalf("expect 1 grant, got %d", len(out.Grants))
	}
	g := out.Grants[0]
	if g.Permission != types.PermissionFullControl {
		t.Errorf("expect FULL_CONTROL, got %s", g.Permission)
	}
	if g.Grantee == nil || g.Grantee.Type != types.TypeCanonicalUser || aws.ToString(g.Grantee.ID) != "testtenant" {
		t.Errorf("unexpected grantee %+v", g.Grantee)
	}
}

func TestProxyGetObjectAcl(t *testing.T) {
	t.Run("existing object", func(t *testing.T) {
		stub := &stubBackend{headOut: &s3.HeadObjectOutput{}}
		client, _ := newTestProxy(t, stub)
		out, err := client.GetObjectAcl(t.Context(), &s3.GetObjectAclInput{
			Bucket: aws.String("testbucket"),
			Key:    aws.String("key.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToString(out.Owner.ID) != "testtenant" {
			t.Errorf("expect owner testtenant, got %s", aws.ToString(out.Owner.ID))
		}
		if len(out.Grants) != 1 || out.Grants[0].Permission != types.PermissionFullControl {
			t.Errorf("unexpected grants %v", out.Grants)
		}
		if aws.ToString(stub.headIn.Key) != "key.txt" {
			t.Errorf("expect existence check, got %v", stub.headIn)
		}
	})
	t.Run("missing object", func(t *testing.T) {
		stub := &stubBackend{
			headErr: &smithy.GenericAPIError{Code: "NotFound", Message: "Not Found"},
		}
		client, _ := newTestProxy(t, stub)
		_, err := client.GetObjectAcl(t.Context(), &s3.GetObjectAclInput{
			Bucket: aws.String("testbucket"),
			Key:    aws.String("missing.txt"),
		})
		if err == nil {
			t.Fatal("expect error for missing object")
		}
	})
}

func TestProxyPutAclRejected(t *testing.T) {
	client, _ := newTestProxy(t, &stubBackend{})
	expectACLNotSupported := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatal("expect error")
		}
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "AccessControlListNotSupported" {
			t.Errorf("expect AccessControlListNotSupported, got %v", err)
		}
	}
	t.Run("PutBucketAcl", func(t *testing.T) {
		_, err := client.PutBucketAcl(t.Context(), &s3.PutBucketAclInput{
			Bucket: aws.String("testbucket"),
			ACL:    types.BucketCannedACLPublicRead,
		})
		expectACLNotSupported(t, err)
	})
	t.Run("PutObjectAcl", func(t *testing.T) {
		_, err := client.PutObjectAcl(t.Context(), &s3.PutObjectAclInput{
			Bucket: aws.String("testbucket"),
			Key:    aws.String("key.txt"),
			ACL:    types.ObjectCannedACLPublicRead,
		})
		expectACLNotSupported(t, err)
	})
	t.Run("PutObject with public-read", func(t *testing.T) {
		_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("testbucket"),
			Key:    aws.String("key.txt"),
			Body:   strings.NewReader("x"),
			ACL:    types.ObjectCannedACLPublicRead,
		})
		expectACLNotSupported(t, err)
	})
}

func TestProxyPutObjectWithAllowedACL(t *testing.T) {
	stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)}}
	client, _ := newTestProxy(t, stub)
	// canned ACLs that are no-ops on an ACL-disabled bucket are accepted
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("key.txt"),
		Body:   strings.NewReader("x"),
		ACL:    types.ObjectCannedACLBucketOwnerFullControl,
	}); err != nil {
		t.Fatal(err)
	}
	if len(stub.putBody) != 1 {
		t.Errorf("unexpected body %q", stub.putBody)
	}
}
