package s3gw_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/s3op"
	"github.com/google/go-cmp/cmp"
)

// A request naming an object version is authorized for the version action
// instead of the plain one, as on Amazon S3: a policy on s3:GetObject alone
// neither grants nor denies a versioned read.

func TestVersionActions(t *testing.T) {
	stub := newHeaderActionStub()
	stub.getTagOut = &s3.GetObjectTaggingOutput{}
	stub.listVerOut = &s3.ListObjectVersionsOutput{}
	users := []userSpec{
		{name: "admin", keyID: "ADMINKEY", secret: "adminsecret"},
		// may read and delete current objects, never a specific version
		{name: "current", keyID: "CURKEY", secret: "cursecret", policy: []policy.ActionStatement{
			{Effect: "Allow", Action: []string{"s3:*"}},
			{Effect: "Deny", Action: []string{"s3:GetObjectVersion", "s3:DeleteObjectVersion", "s3:ListBucketVersions"}},
		}},
		// the reverse: a user policy on the plain actions does not cover versions
		{name: "plain", keyID: "PLAINKEY", secret: "plainsecret", policy: []policy.ActionStatement{
			{Effect: "Allow", Action: []string{"s3:*"}},
			{Effect: "Deny", Action: []string{"s3:GetObject", "s3:DeleteObject"}},
		}},
	}
	gw := gatewayFor(t, buildStore(t, "acme", users, []bucketSpec{{name: "data"}}), stub)
	var mu sync.Mutex
	var last *s3gw.RequestInfo
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) {
		mu.Lock()
		last = info
		mu.Unlock()
	})
	c := clientsFor(t, gw, users)
	ctx := t.Context()
	bucket, key, ver := aws.String("data"), aws.String("k"), aws.String("v1")

	calls := map[string]func(*s3.Client) error{
		"head": func(cl *s3.Client) error {
			_, err := cl.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key})
			return err
		},
		"head version": func(cl *s3.Client) error {
			_, err := cl.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key, VersionId: ver})
			return err
		},
		"delete": func(cl *s3.Client) error {
			_, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: key})
			return err
		},
		"delete version": func(cl *s3.Client) error {
			_, err := cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: key, VersionId: ver})
			return err
		},
		"copy": func(cl *s3.Client) error {
			_, err := cl.CopyObject(ctx, &s3.CopyObjectInput{Bucket: bucket, Key: key, CopySource: aws.String("data/src")})
			return err
		},
		"copy version": func(cl *s3.Client) error {
			_, err := cl.CopyObject(ctx, &s3.CopyObjectInput{Bucket: bucket, Key: key, CopySource: aws.String("data/src?versionId=v1")})
			return err
		},
		"get tagging version": func(cl *s3.Client) error {
			_, err := cl.GetObjectTagging(ctx, &s3.GetObjectTaggingInput{Bucket: bucket, Key: key, VersionId: ver})
			return err
		},
		"list versions": func(cl *s3.Client) error {
			_, err := cl.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: bucket})
			return err
		},
	}
	tests := []struct {
		user, call string
		// the actions on the op when allowed, the refused action otherwise
		want   []string
		denied string
	}{
		{user: "admin", call: "head", want: []string{"s3:GetObject"}},
		{user: "admin", call: "head version", want: []string{"s3:GetObjectVersion"}},
		{user: "admin", call: "delete version", want: []string{"s3:DeleteObjectVersion"}},
		{user: "admin", call: "copy", want: []string{"s3:PutObject", "s3:GetObject"}},
		{user: "admin", call: "copy version", want: []string{"s3:PutObject", "s3:GetObjectVersion"}},
		{user: "admin", call: "get tagging version", want: []string{"s3:GetObjectVersionTagging"}},
		{user: "admin", call: "list versions", want: []string{"s3:ListBucketVersions"}},

		{user: "current", call: "head", want: []string{"s3:GetObject"}},
		{user: "current", call: "delete", want: []string{"s3:DeleteObject"}},
		{user: "current", call: "head version", denied: "s3:GetObjectVersion"},
		{user: "current", call: "delete version", denied: "s3:DeleteObjectVersion"},
		{user: "current", call: "copy version", denied: "s3:GetObjectVersion"},
		{user: "current", call: "list versions", denied: "s3:ListBucketVersions"},

		{user: "plain", call: "head", denied: "s3:GetObject"},
		{user: "plain", call: "head version", want: []string{"s3:GetObjectVersion"}},
		{user: "plain", call: "delete version", want: []string{"s3:DeleteObjectVersion"}},
	}
	for _, tt := range tests {
		t.Run(tt.user+" "+tt.call, func(t *testing.T) {
			err := calls[tt.call](c[tt.user])
			mu.Lock()
			info := last
			mu.Unlock()
			if tt.denied != "" {
				// a HEAD refusal has no body, so the SDK sees only the status
				var apiErr smithy.APIError
				if !errors.As(err, &apiErr) || (apiErr.ErrorCode() != "AccessDenied" && apiErr.ErrorCode() != "Forbidden") {
					t.Fatalf("expect AccessDenied, got %v", err)
				}
				if info.Code != "AccessDenied" {
					t.Errorf("expect AccessDenied recorded, got %q", info.Code)
				}
				var r *s3gw.DenyReason
				if !errors.As(info.Err, &r) || r.Action != tt.denied {
					t.Errorf("expect the refusal to name %s, got %v", tt.denied, info.Err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tt.want, info.Op.Actions); diff != "" {
				t.Errorf("Actions mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// DeleteObjects authorizes each entry for s3:DeleteObjectVersion when it
// names a version and for s3:DeleteObject otherwise.
func TestDeleteObjectsVersionAction(t *testing.T) {
	for _, tt := range []struct {
		name, denied string
		// the keys that reach the backend
		want []string
	}{
		{name: "deny versions", denied: "s3:DeleteObjectVersion", want: []string{"plain"}},
		{name: "deny plain", denied: "s3:DeleteObject", want: []string{"versioned"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			stub := newHeaderActionStub()
			users := []userSpec{{name: "u", keyID: "UKEY", secret: "usecret"}}
			text := `{"Statement":[{"Effect":"Deny","Principal":"*","Action":"` + tt.denied + `","Resource":"data/*"}]}`
			gw := gatewayFor(t, buildStore(t, "acme", users, []bucketSpec{{name: "data", policyText: text}}), stub)
			var info *s3gw.RequestInfo
			gw.SetObserver(func(_ context.Context, i *s3gw.RequestInfo) { info = i })
			out, err := clientsFor(t, gw, users)["u"].DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
				Bucket: aws.String("data"),
				Delete: &types.Delete{Objects: []types.ObjectIdentifier{
					{Key: aws.String("plain")},
					{Key: aws.String("versioned"), VersionId: aws.String("v1")},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			var sent []string
			for _, o := range stub.delObjsIn.Delete.Objects {
				sent = append(sent, aws.ToString(o.Key))
			}
			if diff := cmp.Diff(tt.want, sent); diff != "" {
				t.Errorf("keys sent to the backend (-want +got):\n%s", diff)
			}
			if len(out.Errors) != 1 {
				t.Errorf("expect one refused entry, got %+v", out.Errors)
			}
			if len(info.Op.Denials) != 1 || info.Op.Denials[0].Reason.Action != tt.denied {
				t.Errorf("expect one denial naming %s, got %+v", tt.denied, info.Op.Denials)
			}
		})
	}
}

// ListBuckets has no bucket policy to consult: the user policy alone decides
// s3:ListAllMyBuckets.
func TestListBucketsUserPolicy(t *testing.T) {
	users := []userSpec{
		{name: "admin", keyID: "ADMINKEY", secret: "adminsecret"},
		{name: "reader", keyID: "READKEY", secret: "readsecret", policy: []policy.ActionStatement{
			{Effect: "Allow", Action: []string{"s3:GetObject"}},
		}},
	}
	gw := gatewayFor(t, buildStore(t, "acme", users, []bucketSpec{{name: "data"}}), &stubBackend{})
	var info *s3gw.RequestInfo
	gw.SetObserver(func(_ context.Context, i *s3gw.RequestInfo) { info = i })
	c := clientsFor(t, gw, users)

	if _, err := c["admin"].ListBuckets(t.Context(), &s3.ListBucketsInput{}); err != nil {
		t.Fatal(err)
	}
	_, err := c["reader"].ListBuckets(t.Context(), &s3.ListBucketsInput{})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "AccessDenied" {
		t.Fatalf("expect AccessDenied, got %v", err)
	}
	var r *s3gw.DenyReason
	if !errors.As(info.Err, &r) || r.Layer != s3gw.LayerUserPolicy || r.Action != s3op.ActionListAllMyBuckets {
		t.Errorf("expect a user-policy refusal of s3:ListAllMyBuckets, got %v", info.Err)
	}
	if info.Op != nil {
		t.Error("a refused ListBuckets must not reach the hooks")
	}
}
