package s3gw_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/google/go-cmp/cmp"
)

// A header on an upload can require an action beyond the operation's own,
// as on Amazon S3: tags need s3:PutObjectTagging, Object Lock attributes
// s3:PutObjectRetention / s3:PutObjectLegalHold. That is what lets a policy
// (or an Authorizer, through Op.Actions) refuse tags or a lock on a plain
// PutObject, CopyObject or CreateMultipartUpload without reading headers.

func newHeaderActionStub() *stubBackend {
	return &stubBackend{
		putOut:       &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
		copyOut:      &s3.CopyObjectOutput{CopyObjectResult: &types.CopyObjectResult{ETag: aws.String(`"e"`)}},
		createMPUOut: &s3.CreateMultipartUploadOutput{UploadId: aws.String("u1")},
		upcOut:       &s3.UploadPartCopyOutput{CopyPartResult: &types.CopyPartResult{ETag: aws.String(`"p"`)}},
		headOut:      &s3.HeadObjectOutput{},
		delOut:       &s3.DeleteObjectOutput{},
		delObjsOut:   &s3.DeleteObjectsOutput{},
	}
}

func TestHeaderActions(t *testing.T) {
	stub := newHeaderActionStub()
	users := []userSpec{
		{name: "notagger", keyID: "NOTAGKEY", secret: "notagsecret", policy: []policy.ActionStatement{
			{Effect: "Allow", Action: []string{"s3:*"}},
			{Effect: "Deny", Action: []string{"s3:PutObjectTagging"}},
		}},
		{name: "nolocker", keyID: "NOLOCKKEY", secret: "nolocksecret", policy: []policy.ActionStatement{
			{Effect: "Allow", Action: []string{"s3:*"}},
			{Effect: "Deny", Action: []string{"s3:PutObjectRetention", "s3:PutObjectLegalHold"}},
		}},
		{name: "admin", keyID: "ADMINKEY", secret: "adminsecret"},
	}
	gw := gatewayFor(t, buildStore(t, "acme", users, []bucketSpec{{name: "data"}, {name: "src"}}), stub)
	var mu sync.Mutex
	var last *s3gw.RequestInfo
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) {
		mu.Lock()
		last = info
		mu.Unlock()
	})
	c := clientsFor(t, gw, users)
	ctx := t.Context()

	bucket, key, body := aws.String("data"), aws.String("k"), strings.NewReader("x")
	lockMode, until := types.ObjectLockModeGovernance, aws.Time(time.Now().Add(time.Hour).UTC().Truncate(time.Second))
	tests := []struct {
		name   string
		call   func(*s3.Client) error
		denied map[string]string // user -> the action the observer's DenyReason names
	}{
		{
			name: "put without tags or lock",
			call: func(c *s3.Client) error {
				_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key, Body: body})
				return err
			},
		},
		{
			name: "put with tags",
			call: func(c *s3.Client) error {
				_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key, Body: body, Tagging: aws.String("k=v")})
				return err
			},
			denied: map[string]string{"notagger": "s3:PutObjectTagging"},
		},
		{
			name: "put with retention",
			call: func(c *s3.Client) error {
				_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key, Body: body, ObjectLockMode: lockMode, ObjectLockRetainUntilDate: until})
				return err
			},
			denied: map[string]string{"nolocker": "s3:PutObjectRetention"},
		},
		{
			name: "put with legal hold",
			call: func(c *s3.Client) error {
				_, err := c.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key, Body: body, ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOn})
				return err
			},
			denied: map[string]string{"nolocker": "s3:PutObjectLegalHold"},
		},
		{
			name: "create multipart upload with tags",
			call: func(c *s3.Client) error {
				_, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: key, Tagging: aws.String("k=v")})
				return err
			},
			denied: map[string]string{"notagger": "s3:PutObjectTagging"},
		},
		{
			name: "create multipart upload with retention",
			call: func(c *s3.Client) error {
				_, err := c.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: key, ObjectLockRetainUntilDate: until})
				return err
			},
			denied: map[string]string{"nolocker": "s3:PutObjectRetention"},
		},
		{
			name: "copy without tags",
			call: func(c *s3.Client) error {
				_, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: bucket, Key: key, CopySource: aws.String("src/a")})
				return err
			},
		},
		{
			name: "copy with replaced tags",
			call: func(c *s3.Client) error {
				_, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: bucket, Key: key, CopySource: aws.String("src/a"), TaggingDirective: types.TaggingDirectiveReplace, Tagging: aws.String("k=v")})
				return err
			},
			denied: map[string]string{"notagger": "s3:PutObjectTagging"},
		},
		{
			name: "copy with legal hold",
			call: func(c *s3.Client) error {
				_, err := c.CopyObject(ctx, &s3.CopyObjectInput{Bucket: bucket, Key: key, CopySource: aws.String("src/a"), ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOn})
				return err
			},
			denied: map[string]string{"nolocker": "s3:PutObjectLegalHold"},
		},
	}
	for _, tt := range tests {
		for _, u := range []string{"notagger", "nolocker", "admin"} {
			t.Run(tt.name+"/"+u, func(t *testing.T) {
				err := tt.call(c[u])
				want, denied := tt.denied[u]
				if !denied {
					if err != nil {
						t.Fatalf("expect success, got %v", err)
					}
					return
				}
				expectDenied(t, err)
				mu.Lock()
				info := last
				mu.Unlock()
				var r *s3gw.DenyReason
				if info == nil || !errors.As(info.Err, &r) {
					t.Fatalf("expect a DenyReason cause on the observed request, got %+v", info)
				}
				if r.Action != want || r.Layer != s3gw.LayerUserPolicy {
					t.Errorf("expect the reason to name %s in the user policy, got %+v", want, r)
				}
			})
		}
	}

	// the Object Lock headers a copy carries reach the backend, so the
	// authorization above guards something that is actually applied
	if in := stub.copyIn; in == nil || in.ObjectLockLegalHoldStatus != types.ObjectLockLegalHoldStatusOn {
		t.Errorf("expect the legal hold on the copy input, got %+v", in)
	}
}

func TestOpActions(t *testing.T) {
	stub := newHeaderActionStub()
	client, _, gw := newTestProxyWithGateway(t, stub)
	rec := &opRecorder{}
	gw.SetAuthorizer(rec)
	ctx := t.Context()
	bucket, key := aws.String("testbucket"), aws.String("k")
	until := aws.Time(time.Now().Add(time.Hour).UTC().Truncate(time.Second))

	tests := []struct {
		name string
		call func() error
		want []string
	}{
		{
			name: "get",
			call: func() error {
				_, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: key})
				return err
			},
			want: []string{"s3:GetObject"},
		},
		{
			name: "plain put",
			call: func() error {
				_, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: key, Body: strings.NewReader("x")})
				return err
			},
			want: []string{"s3:PutObject"},
		},
		{
			// the two retention headers add the action once
			name: "put with tags and lock",
			call: func() error {
				_, err := client.PutObject(ctx, &s3.PutObjectInput{
					Bucket: bucket, Key: key, Body: strings.NewReader("x"),
					Tagging:        aws.String("k=v"),
					ObjectLockMode: types.ObjectLockModeCompliance, ObjectLockRetainUntilDate: until,
					ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOn,
				})
				return err
			},
			want: []string{"s3:PutObject", "s3:PutObjectTagging", "s3:PutObjectRetention", "s3:PutObjectLegalHold"},
		},
		{
			name: "create multipart upload with tags",
			call: func() error {
				_, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: key, Tagging: aws.String("k=v")})
				return err
			},
			want: []string{"s3:PutObject", "s3:PutObjectTagging"},
		},
		{
			// the source is read under s3:GetObject, authorized before the hooks
			name: "copy with tags",
			call: func() error {
				_, err := client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: bucket, Key: key, CopySource: aws.String("testbucket/a"), TaggingDirective: types.TaggingDirectiveReplace, Tagging: aws.String("k=v")})
				return err
			},
			want: []string{"s3:PutObject", "s3:PutObjectTagging", "s3:GetObject"},
		},
		{
			name: "upload part copy",
			call: func() error {
				_, err := client.UploadPartCopy(ctx, &s3.UploadPartCopyInput{Bucket: bucket, Key: key, UploadId: aws.String("u1"), PartNumber: aws.Int32(1), CopySource: aws.String("testbucket/a")})
				return err
			},
			want: []string{"s3:PutObject", "s3:GetObject"},
		},
		{
			name: "delete with governance bypass",
			call: func() error {
				_, err := client.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: bucket, Key: key, BypassGovernanceRetention: aws.Bool(true)})
				return err
			},
			want: []string{"s3:DeleteObject", "s3:BypassGovernanceRetention"},
		},
		{
			// authorized per object, so nothing is listed — like Action
			name: "delete objects",
			call: func() error {
				_, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{Bucket: bucket, Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: key}}}})
				return err
			},
			want: nil,
		},
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
			op := rec.ops[0]
			if diff := cmp.Diff(tt.want, op.Actions); diff != "" {
				t.Errorf("Actions mismatch (-want +got):\n%s", diff)
			}
			if len(op.Actions) > 0 && op.Actions[0] != op.Action {
				t.Errorf("expect Action %q first in Actions %v", op.Action, op.Actions)
			}
		})
	}
}

// A form upload carries tags as a field, and needs the same
// s3:PutObjectTagging as the header would on a PUT.
func TestPostObjectTagging(t *testing.T) {
	users := []userSpec{{name: "testuser", keyID: testAccessKeyID, secret: testSecretAccessKey, policy: []policy.ActionStatement{
		{Effect: "Allow", Action: []string{"s3:*"}},
		{Effect: "Deny", Action: []string{"s3:PutObjectTagging"}},
	}}}
	stub := &stubPost{}
	gw := gatewayFor(t, buildStore(t, "testtenant", users, []bucketSpec{{name: "testbucket"}}), stub)
	rec := &opRecorder{}
	gw.SetAuthorizer(rec)

	form := func(tagged bool) *postForm {
		f := &postForm{
			conditions: []string{`{"key": "a.txt"}`},
			fields:     [][2]string{{"key", "a.txt"}},
			filename:   "a.txt", content: "x",
		}
		if tagged {
			f.conditions = append(f.conditions, `{"x-amz-tagging": "k=v"}`)
			f.fields = append(f.fields, [2]string{"x-amz-tagging", "k=v"})
		}
		return f
	}

	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, form(false).request(t))
	if w.Code != http.StatusNoContent {
		t.Fatalf("expect the untagged upload to succeed, got %d: %s", w.Code, w.Body.String())
	}
	if diff := cmp.Diff([]string{"s3:PutObject"}, rec.ops[0].Actions); diff != "" {
		t.Errorf("Actions mismatch (-want +got):\n%s", diff)
	}

	w = httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, form(true).request(t))
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "<Code>AccessDenied</Code>") {
		t.Fatalf("expect the tagged upload to be refused, got %d: %s", w.Code, w.Body.String())
	}
	if len(rec.ops) != 1 {
		t.Errorf("expect the refusal before the Authorizer, saw %d ops", len(rec.ops))
	}
}
