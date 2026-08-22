package s3gw_test

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp/s3gw"
)

// TestOpOperationName pins the operation name the observer sees, including
// for operations the gateway refuses: a service counts unsupported
// operations per user from it, so it must be set even when Action is not
// (DeleteObjects) and when the request never reaches a backend
// (DeleteBucket, PutBucketAcl).
func TestOpOperationName(t *testing.T) {
	stub := &stubBackend{
		putOut:     &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
		copyOut:    &s3.CopyObjectOutput{CopyObjectResult: &types.CopyObjectResult{ETag: aws.String(`"e"`)}},
		delObjsOut: &s3.DeleteObjectsOutput{},
	}
	client, _, app := newTestProxyWithGateway(t, stub)

	var ops []*s3gw.Op
	app.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) {
		ops = append(ops, info.Op)
	})

	ctx := t.Context()
	bucket := aws.String("testbucket")
	calls := []struct {
		want string
		call func() error
	}{
		{"PutObject", func() error {
			_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: aws.String("a.txt"), Body: strings.NewReader("x")})
			return err
		}},
		{"CopyObject", func() error {
			_, err := client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: bucket, Key: aws.String("b.txt"), CopySource: aws.String("testbucket/a.txt")})
			return err
		}},
		{"DeleteObjects", func() error {
			_, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: bucket, Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("a.txt")}}}})
			return err
		}},
		{"DeleteBucket", func() error {
			_, err := client.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: bucket})
			return err
		}},
		{"PutBucketPolicy", func() error {
			_, err := client.PutBucketPolicy(ctx, &s3.PutBucketPolicyInput{Bucket: bucket, Policy: aws.String("{}")})
			return err
		}},
		{"PutBucketAcl", func() error {
			_, err := client.PutBucketAcl(ctx, &s3.PutBucketAclInput{Bucket: bucket, ACL: types.BucketCannedACLPrivate})
			return err
		}},
	}
	for _, c := range calls {
		ops = nil
		err := c.call()
		switch c.want {
		case "PutObject", "CopyObject", "DeleteObjects":
			if err != nil {
				t.Fatalf("%s: %v", c.want, err)
			}
		default:
			if err == nil {
				t.Fatalf("%s: expect a refusal", c.want)
			}
		}
		if len(ops) != 1 || ops[0] == nil {
			t.Fatalf("%s: expect one observed op, got %v", c.want, ops)
		}
		if got := ops[0].Operation; got != c.want {
			t.Errorf("expect operation %q, got %q", c.want, got)
		}
	}
}
