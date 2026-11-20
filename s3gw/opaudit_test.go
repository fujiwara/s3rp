package s3gw_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/google/go-cmp/cmp"
)

// observedOp runs fn against a gateway and returns the op its one request
// was observed with.
func observedOp(t *testing.T, stub *stubBackend, fn func(client *s3.Client)) *s3gw.Op {
	t.Helper()
	client, _, gw := newTestProxyWithGateway(t, stub)
	var ops []*s3gw.Op
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { ops = append(ops, info.Op) })
	fn(client)
	if len(ops) != 1 || ops[0] == nil {
		t.Fatalf("expect one observed op, got %v", ops)
	}
	return ops[0]
}

// A copy's destination is Bucket and Key; where it read from is otherwise
// only in a request header.
func TestOpCopySource(t *testing.T) {
	tests := []struct {
		name   string
		source string
		want   *s3gw.CopySource
	}{
		{
			name: "plain", source: "testbucket/dir/src.txt",
			want: &s3gw.CopySource{Bucket: "testbucket", Key: "dir/src.txt"},
		},
		{
			name: "version and escaped key", source: "testbucket/a%20b.txt?versionId=v1",
			want: &s3gw.CopySource{Bucket: "testbucket", Key: "a b.txt", VersionID: "v1"},
		},
		{
			// what the AWS SDK for .NET sends
			name: "slash between bucket and key escaped", source: "testbucket%2Fdir%2Fa%20b.txt",
			want: &s3gw.CopySource{Bucket: "testbucket", Key: "dir/a b.txt"},
		},
		{
			name: "escaped leading slash", source: "%2Ftestbucket%2Fsrc.txt?versionId=v1",
			want: &s3gw.CopySource{Bucket: "testbucket", Key: "src.txt", VersionID: "v1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubBackend{copyOut: &s3.CopyObjectOutput{CopyObjectResult: &types.CopyObjectResult{ETag: aws.String(`"e"`)}}}
			op := observedOp(t, stub, func(client *s3.Client) {
				if _, err := client.CopyObject(t.Context(), &s3.CopyObjectInput{
					Bucket: aws.String("testbucket"), Key: aws.String("dst.txt"),
					CopySource: aws.String(tt.source),
				}); err != nil {
					t.Fatal(err)
				}
			})
			if op.Key != "dst.txt" {
				t.Errorf("expect the destination as Key, got %q", op.Key)
			}
			if diff := cmp.Diff(tt.want, op.CopySource); diff != "" {
				t.Errorf("unexpected copy source (-want +got):\n%s", diff)
			}
			// the backend bucket name must not leak through it
			if b, _ := json.Marshal(op); strings.Contains(string(b), "backend-testbucket") {
				t.Errorf("backend bucket in the op: %s", b)
			}
		})
	}
	t.Run("not a copy", func(t *testing.T) {
		stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)}}
		op := observedOp(t, stub, func(client *s3.Client) {
			if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
				Bucket: aws.String("testbucket"), Key: aws.String("a.txt"), Body: strings.NewReader("x"),
			}); err != nil {
				t.Fatal(err)
			}
		})
		if op.CopySource != nil {
			t.Errorf("expect no copy source, got %+v", op.CopySource)
		}
	})
}

// DeleteObjects has no Key: the summary is the only record of what the
// request removed.
func TestOpDeleteSummary(t *testing.T) {
	objects := []types.ObjectIdentifier{
		{Key: aws.String("a.txt")},
		{Key: aws.String("b.txt"), VersionId: aws.String("v2")},
		{Key: aws.String("c.txt")},
	}
	backendErrors := []types.Error{{Key: aws.String("c.txt"), Code: aws.String("InternalError"), Message: aws.String("x")}}
	want := &s3gw.DeleteSummary{
		Requested: 3, Deleted: 2, Failed: 1,
		Objects: []s3gw.DeletedObject{
			{Key: "a.txt"},
			{Key: "b.txt", VersionID: "v2"},
			{Key: "c.txt", Code: "InternalError"},
		},
	}
	tests := []struct {
		name  string
		quiet bool
		out   *s3.DeleteObjectsOutput
	}{
		{
			name: "verbose",
			out: &s3.DeleteObjectsOutput{
				Deleted: []types.DeletedObject{{Key: aws.String("a.txt")}, {Key: aws.String("b.txt"), VersionId: aws.String("v2")}},
				Errors:  backendErrors,
			},
		},
		{
			// a quiet response lists only the errors
			name: "quiet", quiet: true,
			out: &s3.DeleteObjectsOutput{Errors: backendErrors},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op := observedOp(t, &stubBackend{delObjsOut: tt.out}, func(client *s3.Client) {
				if _, err := client.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
					Bucket: aws.String("testbucket"),
					Delete: &types.Delete{Objects: objects, Quiet: aws.Bool(tt.quiet)},
				}); err != nil {
					t.Fatal(err)
				}
			})
			if diff := cmp.Diff(want, op.Deleted); diff != "" {
				t.Errorf("unexpected summary (-want +got):\n%s", diff)
			}
			b, err := json.Marshal(op)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(b), `"deleted":{"requested":3,"deleted":2,"failed":1}`) {
				t.Errorf("expect the counts and no keys in the JSON, got %s", b)
			}
		})
	}
}

// A backend error is matched to the entry it names. One without a version
// id is taken to cover every entry for its key, since recording a delete
// that did not happen is the worse mistake in an audit trail.
func TestOpDeleteSummaryVersions(t *testing.T) {
	objects := []types.ObjectIdentifier{
		{Key: aws.String("k"), VersionId: aws.String("v1")},
		{Key: aws.String("k"), VersionId: aws.String("v2")},
		{Key: aws.String("other")},
	}
	tests := []struct {
		name string
		errs []types.Error
		want *s3gw.DeleteSummary
	}{
		{
			name: "error naming one version",
			errs: []types.Error{{Key: aws.String("k"), VersionId: aws.String("v2"), Code: aws.String("AccessDenied")}},
			want: &s3gw.DeleteSummary{
				Requested: 3, Deleted: 2, Failed: 1,
				Objects: []s3gw.DeletedObject{
					{Key: "k", VersionID: "v1"},
					{Key: "k", VersionID: "v2", Code: "AccessDenied"},
					{Key: "other"},
				},
			},
		},
		{
			name: "error without a version",
			errs: []types.Error{{Key: aws.String("k"), Code: aws.String("InternalError")}},
			want: &s3gw.DeleteSummary{
				Requested: 3, Deleted: 1, Failed: 2,
				Objects: []s3gw.DeletedObject{
					{Key: "k", VersionID: "v1", Code: "InternalError"},
					{Key: "k", VersionID: "v2", Code: "InternalError"},
					{Key: "other"},
				},
			},
		},
		{
			// the exact entry wins over the fallback
			name: "both kinds for one key",
			errs: []types.Error{
				{Key: aws.String("k"), Code: aws.String("InternalError")},
				{Key: aws.String("k"), VersionId: aws.String("v1"), Code: aws.String("AccessDenied")},
			},
			want: &s3gw.DeleteSummary{
				Requested: 3, Deleted: 1, Failed: 2,
				Objects: []s3gw.DeletedObject{
					{Key: "k", VersionID: "v1", Code: "AccessDenied"},
					{Key: "k", VersionID: "v2", Code: "InternalError"},
					{Key: "other"},
				},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			op := observedOp(t, &stubBackend{delObjsOut: &s3.DeleteObjectsOutput{Errors: tt.errs}}, func(client *s3.Client) {
				if _, err := client.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
					Bucket: aws.String("testbucket"), Delete: &types.Delete{Objects: objects},
				}); err != nil {
					t.Fatal(err)
				}
			})
			if diff := cmp.Diff(tt.want, op.Deleted); diff != "" {
				t.Errorf("unexpected summary (-want +got):\n%s", diff)
			}
		})
	}
}
