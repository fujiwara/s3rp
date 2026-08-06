package s3gw_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Bucket policies gating statements by the client's source address. The
// httptest clients arrive from 127.0.0.1, so ranges covering and excluding
// the loopback exercise both sides of a condition through the full
// signed-request path (RemoteAddr → verifiedRequest.SourceIP → evaluation).

// writes are denied unless the client is on the loopback range — the test
// client is, so the Deny is lifted
const loopbackOnlyWritePolicy = `{
  "Statement": [
    {
      "Sid": "WritesOnlyFromLoopback",
      "Effect": "Deny",
      "Principal": "*",
      "Action": ["s3:PutObject", "s3:DeleteObject"],
      "Resource": ["ipopen/*"],
      "Condition": {"NotIpAddress": {"aws:SourceIp": "127.0.0.0/8"}}
    }
  ]
}`

// writes are denied unless the client is on 10.0.0.0/8 — the test client
// is not, so the Deny applies
const officeOnlyWritePolicy = `{
  "Statement": [
    {
      "Sid": "WritesOnlyFromOffice",
      "Effect": "Deny",
      "Principal": "*",
      "Action": ["s3:PutObject", "s3:DeleteObject"],
      "Resource": ["ipshut/*"],
      "Condition": {"NotIpAddress": {"aws:SourceIp": "10.0.0.0/8"}}
    }
  ]
}`

func newIPPolicyProxy(t *testing.T, stub *stubBackend) map[string]*s3.Client {
	t.Helper()
	users := []userSpec{
		{name: "alice", keyID: "IPALICEKEY", secret: "alicesecret"},
	}
	buckets := []bucketSpec{
		{name: "ipopen", policyText: loopbackOnlyWritePolicy},
		{name: "ipshut", policyText: officeOnlyWritePolicy},
	}
	gw := gatewayFor(t, buildStore(t, "iptenant", users, buckets), stub)
	return clientsFor(t, gw, users)
}

func TestPolicyIPCondition(t *testing.T) {
	stub := newPolicyStub()
	stub.delObjsOut = &s3.DeleteObjectsOutput{
		Deleted: []types.DeletedObject{{Key: aws.String("a.txt")}},
	}
	clients := newIPPolicyProxy(t, stub)
	ctx := t.Context()

	// the client is inside the allowed range: the Deny is lifted
	if _, err := clients["alice"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("ipopen"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
	}); err != nil {
		t.Errorf("put from an allowed address must succeed: %v", err)
	}

	// outside the allowed range: writes are denied, reads unaffected
	_, err := clients["alice"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("ipshut"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
	})
	expectAccessDenied(t, err)
	if out, err := clients["alice"].GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("ipshut"), Key: aws.String("a.txt"),
	}); err != nil {
		t.Errorf("read must stay unaffected: %v", err)
	} else {
		out.Body.Close()
	}

	// DeleteObjects goes through the per-object authorizer: the lifted Deny
	// takes the skip-fast path, the applying one denies each key
	out, err := clients["alice"].DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("ipopen"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("a.txt")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Errors) != 0 || len(out.Deleted) != 1 {
		t.Errorf("delete from an allowed address must succeed: %+v", out)
	}
	out, err = clients["alice"].DeleteObjects(ctx, &s3.DeleteObjectsInput{
		Bucket: aws.String("ipshut"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("a.txt")}, {Key: aws.String("b.txt")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Errors) != 2 {
		t.Errorf("expect 2 per-key errors, got %+v", out.Errors)
	}
	for _, e := range out.Errors {
		if aws.ToString(e.Code) != "AccessDenied" {
			t.Errorf("expect AccessDenied, got %v", e)
		}
	}
}

// a cross-tenant grant gated by source address: the bucket stays visible to
// the mentioned principal (MentionsPrincipal ignores conditions) but the
// Allow only takes effect from the listed range
const ipGatedSharePolicy = `{
  "Statement": [
    {
      "Sid": "GrantBobReadsFromLoopback",
      "Effect": "Allow",
      "Principal": {"S3RP": ["tenant-b/bob"]},
      "Action": "s3:GetObject",
      "Resource": "ipshared/*",
      "Condition": {"IpAddress": {"aws:SourceIp": "127.0.0.0/8"}}
    }
  ]
}`

const ipGatedFarSharePolicy = `{
  "Statement": [
    {
      "Sid": "GrantBobReadsFromOffice",
      "Effect": "Allow",
      "Principal": {"S3RP": ["tenant-b/bob"]},
      "Action": "s3:GetObject",
      "Resource": "ipfarshared/*",
      "Condition": {"IpAddress": {"aws:SourceIp": "10.0.0.0/8"}}
    }
  ]
}`

func TestCrossTenantIPCondition(t *testing.T) {
	ownerUsers := []userSpec{
		{name: "alice", keyID: "XIPALICEKEY", secret: "alicesecret"},
	}
	foreignUsers := []userSpec{
		{name: "bob", keyID: "XIPBOBKEY", secret: "bobsecret"},
	}
	buckets := []bucketSpec{
		{name: "ipshared", policyText: ipGatedSharePolicy},
		{name: "ipfarshared", policyText: ipGatedFarSharePolicy},
	}
	m := buildStore(t, "tenant-a", ownerUsers, buckets)
	m.addTenant(t, "tenant-b", foreignUsers, nil)
	gw := gatewayFor(t, m, newPolicyStub())
	clients := clientsFor(t, gw, append(ownerUsers, foreignUsers...))
	ctx := t.Context()

	// the grant holds from the client's address
	if out, err := clients["bob"].GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("ipshared"), Key: aws.String("a.txt"),
	}); err != nil {
		t.Errorf("granted cross-tenant read from an allowed address must succeed: %v", err)
	} else {
		out.Body.Close()
	}
	// the grant names bob but requires an address he is not at: the bucket
	// resolves, authorization denies
	_, err := clients["bob"].GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String("ipfarshared"), Key: aws.String("a.txt"),
	})
	expectAccessDenied(t, err)

	// the owner tenant's baseline is unaffected by the conditioned grant
	if _, err := clients["alice"].PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("ipfarshared"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
	}); err != nil {
		t.Errorf("owner-tenant write must succeed: %v", err)
	}
}
