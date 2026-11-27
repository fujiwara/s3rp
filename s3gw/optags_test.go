package s3gw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/google/go-cmp/cmp"
)

// The tags a request writes are on Op.Request.Tags before the Authorizer
// runs, whichever of the ways S3 has to send them the client used.
func TestOpRequestTags(t *testing.T) {
	stub := &stubBackend{
		putOut:       &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
		copyOut:      &s3.CopyObjectOutput{CopyObjectResult: &types.CopyObjectResult{ETag: aws.String(`"e"`)}},
		createMPUOut: &s3.CreateMultipartUploadOutput{UploadId: aws.String("u")},
	}
	client, _, gw := newTestProxyWithGateway(t, stub)
	rec := &opRecorder{}
	gw.SetAuthorizer(rec)

	ctx := t.Context()
	bucket, key := aws.String("testbucket"), aws.String("a.txt")
	want := map[string]string{"plan": "cold", "team": "a b"}
	const header = "plan=cold&team=a+b"
	tests := []struct {
		name string
		call func() error
		want map[string]string
	}{
		{"PutObject", func() error {
			_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key, Body: strings.NewReader("x"), Tagging: aws.String(header)})
			return err
		}, want},
		{"CreateMultipartUpload", func() error {
			_, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: key, Tagging: aws.String(header)})
			return err
		}, want},
		{"CopyObject", func() error {
			_, err := client.CopyObject(ctx, &s3.CopyObjectInput{
				Bucket: bucket, Key: key, CopySource: aws.String("testbucket/b.txt"),
				Tagging: aws.String(header), TaggingDirective: types.TaggingDirectiveReplace,
			})
			return err
		}, want},
		{"PutObjectTagging", func() error {
			_, err := client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: bucket, Key: key, Tagging: &types.Tagging{TagSet: []types.Tag{
				{Key: aws.String("plan"), Value: aws.String("cold")},
				{Key: aws.String("team"), Value: aws.String("a b")},
			}}})
			return err
		}, want},
		// removing every tag writes none, so there is nothing to report
		{"PutObjectTagging empty", func() error {
			_, err := client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: bucket, Key: key, Tagging: &types.Tagging{TagSet: []types.Tag{}}})
			return err
		}, nil},
		{"PutObject untagged", func() error {
			_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key, Body: strings.NewReader("x"), StorageClass: "PLAN_COLD"})
			return err
		}, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec.ops = nil
			if err := tt.call(); err != nil {
				t.Fatal(err)
			}
			if len(rec.ops) != 1 {
				t.Fatalf("expect one authorization, got %d", len(rec.ops))
			}
			var got map[string]string
			if req := rec.ops[0].Request; req != nil {
				got = req.Tags
			} else if tt.name == "PutObject untagged" {
				t.Fatal("expect the storage class on the request")
			}
			if diff := cmp.Diff(tt.want, got); diff != "" {
				t.Errorf("Tags mismatch (-want +got):\n%s", diff)
			}
			if tt.want == nil && got != nil {
				t.Errorf("expect a nil map, got %v", got)
			}
		})
	}
	// the body read ahead of the hooks is still what the backend is sent
	if in := stub.putTagIn; in == nil || in.Tagging == nil || len(in.Tagging.TagSet) != 0 {
		t.Errorf("expect the empty set to reach the backend, got %+v", in)
	}
}

// reservedTags refuses a tag whose key a service keeps for itself.
type reservedTags struct{ opRecorder }

func (a *reservedTags) Authorize(ctx context.Context, op *s3gw.Op) error {
	_ = a.opRecorder.Authorize(ctx, op)
	if op.Request == nil {
		return nil
	}
	for key := range op.Request.Tags {
		if strings.HasPrefix(key, "sys:") {
			return s3err.New(http.StatusBadRequest, "InvalidTag", "System tags cannot be added/updated by requester")
		}
	}
	return nil
}

// An Authorizer refuses a PutObjectTagging on the tags in its body, which
// the gateway has read by then, and the backend is not called.
func TestAuthorizerRefusesTags(t *testing.T) {
	stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)}}
	client, _, gw := newTestProxyWithGateway(t, stub)
	gw.SetAuthorizer(&reservedTags{})

	ctx := t.Context()
	bucket, key := aws.String("testbucket"), aws.String("a.txt")
	tagging := func(k string) *types.Tagging {
		return &types.Tagging{TagSet: []types.Tag{{Key: aws.String(k), Value: aws.String("v")}}}
	}
	refused := func(t *testing.T, err error) {
		t.Helper()
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InvalidTag" {
			t.Fatalf("expect InvalidTag, got %v", err)
		}
	}

	_, err := client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: bucket, Key: key, Tagging: tagging("sys:owner")})
	refused(t, err)
	if stub.putTagIn != nil {
		t.Error("expect the refused tags not to reach the backend")
	}
	_, err = client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key, Body: strings.NewReader("x"), Tagging: aws.String("sys%3Aowner=v")})
	refused(t, err)
	if stub.putIn != nil {
		t.Error("expect the refused upload not to reach the backend")
	}

	if _, err := client.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{Bucket: bucket, Key: key, Tagging: tagging("owner")}); err != nil {
		t.Fatal(err)
	}
	if in := stub.putTagIn; in == nil || len(in.Tagging.TagSet) != 1 || aws.ToString(in.Tagging.TagSet[0].Key) != "owner" {
		t.Errorf("expect the admitted tags at the backend, got %+v", in)
	}
}

// A form upload's tagging field is reported as the header is on a PUT.
func TestPostObjectRequestTags(t *testing.T) {
	stub := &stubPost{}
	gw := gatewayFor(t, buildStore(t, "testtenant", []userSpec{{name: "testuser", keyID: testAccessKeyID, secret: testSecretAccessKey}}, []bucketSpec{{name: "testbucket"}}), stub)
	rec := &opRecorder{}
	gw.SetAuthorizer(rec)

	form := &postForm{
		conditions: []string{`{"key": "a.txt"}`, `{"x-amz-tagging": "plan=cold"}`},
		fields:     [][2]string{{"key", "a.txt"}, {"x-amz-tagging", "plan=cold"}},
		filename:   "a.txt", content: "x",
	}
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, form.request(t))
	if w.Code != http.StatusNoContent {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}
	if len(rec.ops) != 1 || rec.ops[0].Request == nil {
		t.Fatalf("expect the request attributes on the op, got %+v", rec.ops)
	}
	if diff := cmp.Diff(map[string]string{"plan": "cold"}, rec.ops[0].Request.Tags); diff != "" {
		t.Errorf("Tags mismatch (-want +got):\n%s", diff)
	}
}
