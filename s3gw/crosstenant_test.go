package s3gw_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Cross-tenant access: a bucket policy grants another tenant's user by its
// qualified principal ("tenant/user"). The baseline for a foreign requester
// is deny — only explicit Allow statements grant, and Deny still wins.

const sharedBucketPolicy = `{
  "Statement": [
    {
      "Sid": "GrantBobReads",
      "Effect": "Allow",
      "Principal": {"S3RP": ["tenant-b/bob"]},
      "Action": ["s3:GetObject", "s3:ListBucket"],
      "Resource": ["shared", "shared/*"]
    },
    {
      "Sid": "GrantBobTmpDeletes",
      "Effect": "Allow",
      "Principal": {"S3RP": ["tenant-b/bob"]},
      "Action": "s3:DeleteObject",
      "Resource": "shared/tmp/*"
    },
    {
      "Sid": "NobodyReadsSecrets",
      "Effect": "Deny",
      "Principal": "*",
      "Action": "s3:GetObject",
      "Resource": "shared/secret/*"
    }
  ]
}`

// "*" is every authenticated user of any tenant: this bucket is readable
// across tenants
const openBucketPolicy = `{
  "Statement": [
    {"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "open/*"}
  ]
}`

// newCrossTenantProxy builds a proxy with tenant-a owning the buckets and
// tenant-b's users trying to reach them.
func newCrossTenantProxy(t *testing.T, stub *stubBackend) map[string]*s3.Client {
	t.Helper()
	ownerUsers := []userSpec{
		{name: "alice", keyID: "ALICEKEY", secret: "alicesecret"},
	}
	foreignUsers := []userSpec{
		{name: "bob", keyID: "BOBKEY", secret: "bobsecret"},
		{name: "carol", keyID: "CAROLKEY", secret: "carolsecret"},
	}
	buckets := []bucketSpec{
		{name: "shared", policyText: sharedBucketPolicy},
		{name: "open", policyText: openBucketPolicy},
		{name: "private"},
	}
	m := buildStore(t, "tenant-a", ownerUsers, buckets)
	m.addTenant(t, "tenant-b", foreignUsers, nil)
	gw := gatewayFor(t, m, stub)
	return clientsFor(t, gw, append(ownerUsers, foreignUsers...))
}

func TestCrossTenantAllow(t *testing.T) {
	clients := newCrossTenantProxy(t, newPolicyStub())
	ctx := t.Context()

	// bob is granted s3:GetObject on shared/*
	if out, err := clients["bob"].GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("shared"), Key: aws.String("a.txt"),
	}); err != nil {
		t.Errorf("granted cross-tenant read must succeed: %v", err)
	} else {
		out.Body.Close()
	}
	// s3:ListBucket on the bucket resource covers HeadBucket
	if _, err := clients["bob"].HeadBucket(ctx, &s3.HeadBucketInput{
		Bucket: aws.String("shared"),
	}); err != nil {
		t.Errorf("granted cross-tenant HeadBucket must succeed: %v", err)
	}
	// actions the policy does not allow stay denied (default-deny baseline)
	_, err := clients["bob"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("shared"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
	})
	expectAccessDenied(t, err)
	// the blanket Deny catches the cross-tenant principal too
	_, err = clients["bob"].GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("shared"), Key: aws.String("secret/x"),
	})
	expectAccessDenied(t, err)

	// the owner tenant's baseline is unchanged: alice writes freely and the
	// blanket Deny still applies to her
	if _, err := clients["alice"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("shared"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
	}); err != nil {
		t.Errorf("owner-tenant write must succeed: %v", err)
	}
	_, err = clients["alice"].GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("shared"), Key: aws.String("secret/x"),
	})
	expectAccessDenied(t, err)
}

func TestCrossTenantInvisibleWithoutMention(t *testing.T) {
	clients := newCrossTenantProxy(t, newPolicyStub())
	ctx := t.Context()

	// carol is not covered by any grant: these buckets answer the same
	// AccessDenied a nonexistent bucket does
	for _, bucket := range []string{"shared", "private", "nosuchbucket"} {
		_, err := clients["carol"].GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String(bucket), Key: aws.String("a.txt"),
		})
		expectAccessDenied(t, err)
	}
	// a bucket without a policy is unreachable across tenants
	_, err := clients["bob"].GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("private"), Key: aws.String("a.txt"),
	})
	expectAccessDenied(t, err)
}

func TestCrossTenantStarGrant(t *testing.T) {
	clients := newCrossTenantProxy(t, newPolicyStub())
	ctx := t.Context()

	// "Principal": "*" grants every authenticated user, whatever the tenant
	for _, user := range []string{"alice", "bob", "carol"} {
		if out, err := clients[user].GetObject(ctx, &s3.GetObjectInput{
			Bucket: aws.String("open"), Key: aws.String("a.txt"),
		}); err != nil {
			t.Errorf("%s must read the star-granted bucket: %v", user, err)
		} else {
			out.Body.Close()
		}
	}
	// but only the granted action: writes stay cross-tenant denied
	_, err := clients["bob"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("open"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
	})
	expectAccessDenied(t, err)
}

func TestCrossTenantDeleteObjectsPerKey(t *testing.T) {
	stub := newPolicyStub()
	stub.delObjsOut = &s3.DeleteObjectsOutput{
		Deleted: []types.DeletedObject{{Key: aws.String("tmp/a")}},
	}
	clients := newCrossTenantProxy(t, stub)

	// bob may delete only under shared/tmp/: the other key is refused
	// per-key without reaching the backend
	out, err := clients["bob"].DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
		Bucket: aws.String("shared"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("tmp/a")},
			{Key: aws.String("keep/b")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Deleted) != 1 || aws.ToString(out.Deleted[0].Key) != "tmp/a" {
		t.Errorf("unexpected deleted %+v", out.Deleted)
	}
	if len(out.Errors) != 1 || aws.ToString(out.Errors[0].Key) != "keep/b" ||
		aws.ToString(out.Errors[0].Code) != "AccessDenied" {
		t.Errorf("unexpected errors %+v", out.Errors)
	}
	if got := stub.delObjsIn.Delete.Objects; len(got) != 1 || aws.ToString(got[0].Key) != "tmp/a" {
		t.Errorf("backend must receive only the allowed key, got %+v", got)
	}
}

func TestCrossTenantOwnerIsBucketTenant(t *testing.T) {
	stub := newPolicyStub()
	stub.listOut = &s3.ListObjectsV2Output{
		Contents: []types.Object{{
			Key:          aws.String("a.txt"),
			LastModified: aws.Time(time.Unix(0, 0).UTC()),
		}},
	}
	clients := newCrossTenantProxy(t, stub)

	out, err := clients["bob"].ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket:     aws.String("shared"),
		FetchOwner: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Contents) != 1 || out.Contents[0].Owner == nil {
		t.Fatalf("expect one object with an owner, got %+v", out.Contents)
	}
	// the owner is the bucket's tenant, not the requester's
	if got := aws.ToString(out.Contents[0].Owner.ID); got != "tenant-a" {
		t.Errorf("owner must be the bucket's tenant, got %q", got)
	}
	if got := aws.ToString(out.Name); got != "shared" {
		t.Errorf("unexpected bucket name %q", got)
	}
}
