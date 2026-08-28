package s3gw_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
	"github.com/google/go-cmp/cmp"
)

const reasonSharedPolicy = `{
  "Statement": [
    {"Sid": "GrantBobReads", "Effect": "Allow", "Principal": {"S3RP": ["tenant-b/bob"]}, "Action": "s3:GetObject", "Resource": "shared/*"},
    {"Sid": "GrantBobTmpDeletes", "Effect": "Allow", "Principal": {"S3RP": ["tenant-b/bob"]}, "Action": "s3:DeleteObject", "Resource": "shared/tmp/*"},
    {"Sid": "NobodyReadsSecrets", "Effect": "Deny", "Principal": "*", "Action": "s3:GetObject", "Resource": "shared/secret/*"}
  ]
}`

const reasonNoSidPolicy = `{
  "Statement": [
    {"Effect": "Allow", "Principal": "*", "Action": "s3:GetObject", "Resource": "nosid/*"},
    {"Effect": "Deny", "Principal": {"S3RP": ["tenant-a/batch"]}, "Action": "s3:PutObject", "Resource": "nosid/*"}
  ]
}`

// reasonProxy serves tenant-a (admin, batch, limited) and tenant-b (bob)
// against buckets that exercise every refusal layer, recording what the
// observer is told.
type reasonProxy struct {
	clients map[string]*s3.Client
	mu      sync.Mutex
	last    *s3gw.RequestInfo
}

func newReasonProxy(t *testing.T, stub *stubBackend) *reasonProxy {
	t.Helper()
	owner := []userSpec{
		{name: "admin", keyID: "ADMINKEY", secret: "adminsecret"},
		{name: "batch", keyID: "BATCHKEY", secret: "batchsecret"},
		{name: "limited", keyID: "LIMITEDKEY", secret: "limitedsecret", policy: []policy.ActionStatement{
			{Effect: "Allow", Action: []string{"s3:Get*", "s3:List*", "s3:DeleteObject"}},
			{Effect: "Deny", Action: []string{"s3:GetObjectTagging"}},
		}},
	}
	foreign := []userSpec{{name: "bob", keyID: "BOBKEY", secret: "bobsecret"}}
	m := buildStore(t, "tenant-a", owner, []bucketSpec{
		{name: "policied", policyText: batchReadOnlyPolicyFor("tenant-a")},
		{name: "nosid", policyText: reasonNoSidPolicy},
		{name: "shared", policyText: reasonSharedPolicy},
		{name: "private"},
	})
	m.addTenant(t, "tenant-b", foreign, nil)
	gw := gatewayFor(t, m, stub)
	p := &reasonProxy{}
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) {
		p.mu.Lock()
		p.last = info
		p.mu.Unlock()
	})
	p.clients = clientsFor(t, gw, append(owner, foreign...))
	return p
}

func batchReadOnlyPolicyFor(tenant string) string {
	return strings.ReplaceAll(batchReadOnlyPolicy, "poltenant/", tenant+"/")
}

// reason returns the DenyReason behind the last observed request.
func (p *reasonProxy) reason(t *testing.T) *s3gw.DenyReason {
	t.Helper()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		t.Fatal("nothing observed")
	}
	if p.last.Code != "AccessDenied" {
		t.Fatalf("expect an AccessDenied, observed %+v", p.last)
	}
	var r *s3gw.DenyReason
	if !errors.As(p.last.Err, &r) {
		t.Fatalf("expect a DenyReason cause, got %v", p.last.Err)
	}
	return r
}

func TestDenyReason(t *testing.T) {
	p := newReasonProxy(t, newPolicyStub())
	c := p.clients
	ctx := t.Context()
	type call func() error
	tests := []struct {
		name string
		call call
		want s3gw.DenyReason
		is   error // wrapped error, when one is expected
	}{
		{
			name: "user policy deny statement",
			call: func() error {
				_, err := c["limited"].GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: aws.String("private"), Key: aws.String("a")})
				return err
			},
			want: s3gw.DenyReason{Layer: s3gw.LayerUserPolicy, Statement: 1, Principal: "tenant-a/limited", Action: "s3:GetObjectTagging"},
		},
		{
			name: "user policy implicit deny",
			call: func() error {
				_, err := c["limited"].PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("private"), Key: aws.String("a"), Body: strings.NewReader("x")})
				return err
			},
			want: s3gw.DenyReason{Layer: s3gw.LayerUserPolicy, Statement: -1, Principal: "tenant-a/limited", Action: "s3:PutObject"},
		},
		{
			name: "bucket policy deny with sid",
			call: func() error {
				_, err := c["batch"].PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("policied"), Key: aws.String("a.txt"), Body: strings.NewReader("x")})
				return err
			},
			want: s3gw.DenyReason{Layer: s3gw.LayerBucketPolicy, Statement: 0, Sid: "BatchIsReadOnly", Principal: "tenant-a/batch", Action: "s3:PutObject", Resource: "policied/a.txt"},
		},
		{
			name: "bucket policy deny without sid",
			call: func() error {
				_, err := c["batch"].PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("nosid"), Key: aws.String("a.txt"), Body: strings.NewReader("x")})
				return err
			},
			want: s3gw.DenyReason{Layer: s3gw.LayerBucketPolicy, Statement: 1, Principal: "tenant-a/batch", Action: "s3:PutObject", Resource: "nosid/a.txt"},
		},
		{
			name: "cross-tenant without an allow",
			call: func() error {
				_, err := c["bob"].PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("shared"), Key: aws.String("a.txt"), Body: strings.NewReader("x")})
				return err
			},
			want: s3gw.DenyReason{Layer: s3gw.LayerCrossTenant, Statement: -1, Principal: "tenant-b/bob", Action: "s3:PutObject", Resource: "shared/a.txt"},
		},
		{
			name: "cross-tenant grant cut by a deny",
			call: func() error {
				_, err := c["bob"].GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("shared"), Key: aws.String("secret/a.txt")})
				return err
			},
			want: s3gw.DenyReason{Layer: s3gw.LayerBucketPolicy, Statement: 2, Sid: "NobodyReadsSecrets", Principal: "tenant-b/bob", Action: "s3:GetObject", Resource: "shared/secret/a.txt"},
		},
		{
			name: "visibility gate",
			call: func() error {
				_, err := c["bob"].GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("private"), Key: aws.String("a.txt")})
				return err
			},
			want: s3gw.DenyReason{Layer: s3gw.LayerVisibility, Statement: -1, Principal: "tenant-b/bob", Resource: "private"},
		},
		{
			name: "copy source outside the tenant",
			call: func() error {
				_, err := c["admin"].CopyObject(ctx, &s3.CopyObjectInput{
					Bucket: aws.String("private"), Key: aws.String("k"), CopySource: aws.String("nosuchbucket/k"),
				})
				return err
			},
			want: s3gw.DenyReason{Layer: s3gw.LayerCopySource, Statement: -1, Principal: "tenant-a/admin", Action: "s3:GetObject", Resource: "nosuchbucket/k"},
			is:   store.ErrNotFound,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			expectAccessDenied(t, err)
			// the client learns nothing beyond the code
			for _, secret := range []string{"statement", "Sid", tc.want.Sid, tc.want.Layer} {
				if secret != "" && strings.Contains(err.Error(), secret) {
					t.Errorf("client error leaks %q: %v", secret, err)
				}
			}
			got := p.reason(t)
			if diff := cmp.Diff(tc.want, *got, cmp.AllowUnexported(s3gw.DenyReason{}), cmp.FilterPath(func(p cmp.Path) bool {
				return p.Last().String() == ".err"
			}, cmp.Ignore())); diff != "" {
				t.Errorf("reason mismatch (-want +got):\n%s", diff)
			}
			if tc.is != nil && !errors.Is(got, tc.is) {
				t.Errorf("expect the reason to wrap %v", tc.is)
			}
			if got.Error() == "" {
				t.Error("expect a rendered reason")
			}
		})
	}
}

func TestDenyReasonDeleteObjects(t *testing.T) {
	stub := newPolicyStub()
	stub.delObjsOut = &s3.DeleteObjectsOutput{}
	p := newReasonProxy(t, stub)
	ctx := t.Context()
	keys := func(ks ...string) *types.Delete {
		d := &types.Delete{}
		for _, k := range ks {
			d.Objects = append(d.Objects, types.ObjectIdentifier{Key: aws.String(k)})
		}
		return d
	}
	denials := func(t *testing.T) []s3gw.Denial {
		t.Helper()
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.last == nil || p.last.Op == nil {
			t.Fatalf("expect an observed op, got %+v", p.last)
		}
		if p.last.Code != "" {
			t.Fatalf("DeleteObjects itself must succeed, got %s", p.last.Code)
		}
		return p.last.Op.Denials
	}

	t.Run("one statement, many keys", func(t *testing.T) {
		out, err := p.clients["batch"].DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String("policied"), Delete: keys("a", "b", "c"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Errors) != 3 {
			t.Fatalf("expect 3 per-key errors, got %+v", out.Errors)
		}
		want := []s3gw.Denial{{
			Reason: s3gw.DenyReason{Layer: s3gw.LayerBucketPolicy, Statement: 0, Sid: "BatchIsReadOnly",
				Principal: "tenant-a/batch", Action: "s3:DeleteObject", Resource: "policied/a"},
			Keys: 3,
		}}
		if diff := cmp.Diff(want, denials(t), cmp.AllowUnexported(s3gw.DenyReason{})); diff != "" {
			t.Errorf("denials mismatch (-want +got):\n%s", diff)
		}
	})
	t.Run("cross-tenant mixed keys", func(t *testing.T) {
		out, err := p.clients["bob"].DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String("shared"), Delete: keys("tmp/a", "x", "secret/y"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Errors) != 2 {
			t.Fatalf("expect 2 per-key errors, got %+v", out.Errors)
		}
		want := []s3gw.Denial{{
			Reason: s3gw.DenyReason{Layer: s3gw.LayerCrossTenant, Statement: -1,
				Principal: "tenant-b/bob", Action: "s3:DeleteObject", Resource: "shared/x"},
			Keys: 2,
		}}
		if diff := cmp.Diff(want, denials(t), cmp.AllowUnexported(s3gw.DenyReason{})); diff != "" {
			t.Errorf("denials mismatch (-want +got):\n%s", diff)
		}
	})
	t.Run("nothing denied", func(t *testing.T) {
		if _, err := p.clients["admin"].DeleteObjects(ctx, &s3.DeleteObjectsInput{
			Bucket: aws.String("policied"), Delete: keys("a"),
		}); err != nil {
			t.Fatal(err)
		}
		if d := denials(t); d != nil {
			t.Errorf("expect no denials, got %+v", d)
		}
	})
}

// The reason renders through RequestInfo's JSON like any other cause.
func TestDenyReasonJSON(t *testing.T) {
	info := &s3gw.RequestInfo{
		Status: 403, Code: "AccessDenied",
		Err: &s3gw.DenyReason{Layer: s3gw.LayerBucketPolicy, Statement: 2, Sid: "NoLogs",
			Principal: "acme/app1", Action: "s3:DeleteObject", Resource: "photos/logs/a"},
	}
	b, err := json.Marshal(info)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := `bucket policy statement[2] "NoLogs" denies s3:DeleteObject on photos/logs/a (acme/app1)`
	if got["error"] != want {
		t.Errorf("error %q, want %q", got["error"], want)
	}
}
