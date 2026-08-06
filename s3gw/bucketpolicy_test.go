package s3gw_test

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/google/go-cmp/cmp"
)

const batchReadOnlyPolicy = `{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Sid": "BatchIsReadOnly",
      "Effect": "Deny",
      "Principal": {"S3RP": ["poltenant/batch"]},
      "Action": ["s3:PutObject", "s3:DeleteObject"],
      "Resource": ["policied/*"]
    }
  ]
}`

const everyoneReadOnlyPolicy = `{
  "Statement": [
    {
      "Sid": "NobodyWrites",
      "Effect": "Deny",
      "Principal": "*",
      "Action": ["s3:PutObject", "s3:DeleteObject"],
      "Resource": ["frozen/*"]
    }
  ]
}`

const onlyAdminWritesPolicy = `{
  "Statement": [
    {
      "Sid": "OnlyAdminWrites",
      "Effect": "Deny",
      "NotPrincipal": {"S3RP": ["poltenant/admin"]},
      "Action": ["s3:PutObject", "s3:DeleteObject"],
      "Resource": ["locked/*"]
    }
  ]
}`

// newPolicyTestProxy builds a proxy with one tenant, two users
// (admin / batch) and three buckets carrying different policies.
func newPolicyTestProxy(t *testing.T, stub *stubBackend) map[string]*s3.Client {
	t.Helper()
	users := []userSpec{
		{name: "admin", keyID: "ADMINKEY", secret: "adminsecret"},
		{name: "batch", keyID: "BATCHKEY", secret: "batchsecret"},
	}
	buckets := []bucketSpec{
		{name: "policied", policyText: batchReadOnlyPolicy},
		{name: "frozen", policyText: everyoneReadOnlyPolicy},
		{name: "locked", policyText: onlyAdminWritesPolicy},
		{name: "free"},
	}
	gw := gatewayFor(t, buildStore(t, "poltenant", users, buckets), stub)
	return clientsFor(t, gw, users)
}

func expectAccessDenied(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expect AccessDenied, got success")
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "AccessDenied" {
		t.Errorf("expect AccessDenied, got %v", err)
	}
}

func newPolicyStub() *stubBackend {
	return &stubBackend{
		getOut:  &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("content"))},
		putOut:  &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
		delOut:  &s3.DeleteObjectOutput{},
		copyOut: &s3.CopyObjectOutput{CopyObjectResult: &types.CopyObjectResult{ETag: aws.String(`"c"`)}},
	}
}

func TestPolicyDenyUser(t *testing.T) {
	clients := newPolicyTestProxy(t, newPolicyStub())
	ctx := t.Context()

	// batch is read-only on the policied bucket
	_, err := clients["batch"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("policied"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
	})
	expectAccessDenied(t, err)
	_, err = clients["batch"].DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String("policied"), Key: aws.String("a.txt"),
	})
	expectAccessDenied(t, err)
	if out, err := clients["batch"].GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("policied"), Key: aws.String("a.txt"),
	}); err != nil {
		t.Errorf("batch read must succeed: %v", err)
	} else {
		out.Body.Close()
	}

	// admin has full access
	if _, err := clients["admin"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("policied"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
	}); err != nil {
		t.Errorf("admin put must succeed: %v", err)
	}

	// multipart is also s3:PutObject
	_, err = clients["batch"].CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: aws.String("policied"), Key: aws.String("big.bin"),
	})
	expectAccessDenied(t, err)

	// no policy on the free bucket
	if _, err := clients["batch"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("free"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
	}); err != nil {
		t.Errorf("put to free bucket must succeed: %v", err)
	}
}

func TestPolicyDenyEveryone(t *testing.T) {
	clients := newPolicyTestProxy(t, newPolicyStub())
	// principal "*" denies all users, including ones added later
	for user, client := range clients {
		_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("frozen"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
		})
		if err == nil {
			t.Errorf("%s put must be denied", user)
		}
		if out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("frozen"), Key: aws.String("a.txt"),
		}); err != nil {
			t.Errorf("%s read must succeed: %v", user, err)
		} else {
			out.Body.Close()
		}
	}
}

func TestPolicyNotPrincipal(t *testing.T) {
	clients := newPolicyTestProxy(t, newPolicyStub())
	ctx := t.Context()
	// only admin may write to the locked bucket; everyone else
	// (including future users) is denied
	if _, err := clients["admin"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("locked"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
	}); err != nil {
		t.Errorf("admin put must succeed: %v", err)
	}
	_, err := clients["batch"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("locked"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
	})
	expectAccessDenied(t, err)
}

func TestPolicyDeleteObjectsPartialDeny(t *testing.T) {
	stub := newPolicyStub()
	stub.delObjsOut = &s3.DeleteObjectsOutput{
		Deleted: []types.DeletedObject{{Key: aws.String("allowed.txt")}},
	}
	clients := newPolicyTestProxy(t, stub)
	// deletion in the policied bucket is denied for batch per object
	out, err := clients["batch"].DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
		Bucket: aws.String("free"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("allowed.txt")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Errors) != 0 || len(out.Deleted) != 1 {
		t.Errorf("free bucket delete must succeed: %+v", out)
	}

	out, err = clients["batch"].DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
		Bucket: aws.String("policied"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("a.txt")},
			{Key: aws.String("b.txt")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Errors) != 2 {
		t.Errorf("expect 2 errors, got %+v", out.Errors)
	}
	for _, e := range out.Errors {
		if aws.ToString(e.Code) != "AccessDenied" {
			t.Errorf("expect AccessDenied, got %v", e)
		}
	}
	if stub.delObjsIn != nil && aws.ToString(stub.delObjsIn.Bucket) == "backend-policied" {
		t.Error("denied keys must not reach the backend")
	}
}

func TestPolicyCopySourceDenied(t *testing.T) {
	clients := newPolicyTestProxy(t, newPolicyStub())
	// reading from frozen (allowed) into policied as batch is denied by
	// the destination s3:PutObject
	_, err := clients["batch"].CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket:     aws.String("policied"),
		Key:        aws.String("dst.txt"),
		CopySource: aws.String("free/src.txt"),
	})
	expectAccessDenied(t, err)

	// admin copying is fine
	stubOut := clients["admin"]
	if _, err := stubOut.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket:     aws.String("free"),
		Key:        aws.String("dst.txt"),
		CopySource: aws.String("policied/src.txt"),
	}); err != nil {
		t.Errorf("admin copy must succeed: %v", err)
	}
}

func TestGetBucketPolicy(t *testing.T) {
	clients := newPolicyTestProxy(t, newPolicyStub())
	out, err := clients["batch"].GetBucketPolicy(t.Context(), &s3.GetBucketPolicyInput{
		Bucket: aws.String("policied"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// the raw JSON text is returned as-is
	var got, want any
	if err := json.Unmarshal([]byte(aws.ToString(out.Policy)), &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(batchReadOnlyPolicy), &want); err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("policy mismatch (-want +got):\n%s", diff)
	}

	// a bucket without a policy
	_, err = clients["batch"].GetBucketPolicy(t.Context(), &s3.GetBucketPolicyInput{
		Bucket: aws.String("free"),
	})
	if err == nil {
		t.Fatal("expect NoSuchBucketPolicy")
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NoSuchBucketPolicy" {
		t.Errorf("expect NoSuchBucketPolicy, got %v", err)
	}
}
