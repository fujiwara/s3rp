package s3gw_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/google/go-cmp/cmp"
)

// The storage class mapper: ToBackend chooses the class the backend is told
// (from the op and the object size) in place of the client's request, and
// ToClient chooses what the client is shown for the class the backend
// reported, while Op.Response keeps the backend's value.

// classMapper is a test mapper built from two funcs, recording what each
// was handed. A nil func passes its direction through, which is what a
// production mapper writes explicitly when it means to.
type classMapper struct {
	toBackend func(op *s3gw.Op, size int64) string
	toClient  func(op *s3gw.Op, backendClass string) string
	sizes     []int64
	classes   []string
}

func (m *classMapper) ToBackend(op *s3gw.Op, size int64) string {
	m.sizes = append(m.sizes, size)
	if m.toBackend == nil {
		if op.Request == nil {
			return ""
		}
		return op.Request.StorageClass
	}
	return m.toBackend(op, size)
}

func (m *classMapper) ToClient(op *s3gw.Op, backendClass string) string {
	m.classes = append(m.classes, backendClass)
	if m.toClient == nil {
		return backendClass
	}
	return m.toClient(op, backendClass)
}

// bySize sends objects of at least the threshold to the large-object class,
// everything else to the small one; an unknown size is treated as large.
func bySize(threshold int64) func(*s3gw.Op, int64) string {
	return func(op *s3gw.Op, size int64) string {
		if size == s3gw.SizeUnknown || size >= threshold {
			return "hdd"
		}
		return "ssd"
	}
}

func TestStorageClassToBackendBySize(t *testing.T) {
	tests := []struct {
		name string
		body string
		want types.StorageClass
	}{
		{"small", "hello", "ssd"},
		{"large", strings.Repeat("x", 1024), "hdd"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)}}
			client, _, gw := newTestProxyWithGateway(t, stub)
			m := &classMapper{toBackend: func(op *s3gw.Op, size int64) string {
				if op.Bucket != "testbucket" || op.Key != "a.txt" {
					t.Errorf("unexpected op %s/%s", op.Bucket, op.Key)
				}
				return bySize(100)(op, size)
			}}
			gw.SetStorageClassMapper(m)
			_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
				Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
				Body: strings.NewReader(tt.body),
			})
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff([]int64{int64(len(tt.body))}, m.sizes); diff != "" {
				t.Errorf("sizes handed to the mapper (-want +got):\n%s", diff)
			}
			if stub.putIn.StorageClass != tt.want {
				t.Errorf("backend got class %q, want %q", stub.putIn.StorageClass, tt.want)
			}
		})
	}
}

// The mapper decides even when the client named a class: the request stays
// on Op.Request for the Authorizer, and what the backend is told is the
// mapper's answer — "" sends none.
func TestStorageClassToBackendOverridesRequest(t *testing.T) {
	stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)}}
	client, _, gw := newTestProxyWithGateway(t, stub)
	rec := &opRecorder{}
	gw.SetAuthorizer(rec)
	gw.SetStorageClassMapper(&classMapper{toBackend: func(*s3gw.Op, int64) string { return "" }})

	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
		Body:         strings.NewReader("hello"),
		StorageClass: types.StorageClass("GLACIER"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stub.putIn.StorageClass != "" {
		t.Errorf("backend got class %q, want none", stub.putIn.StorageClass)
	}
	if op := rec.ops[0]; op.Request == nil || op.Request.StorageClass != "GLACIER" {
		t.Errorf("expect the client's request on the op, got %+v", op.Request)
	}
}

// Without a mapper the client's class is forwarded and the backend's class
// reported, as before.
func TestStorageClassPassthroughWithoutMapper(t *testing.T) {
	stub := &stubBackend{
		putOut:  &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
		headOut: &s3.HeadObjectOutput{ContentLength: aws.Int64(5), StorageClass: "hdd"},
	}
	client, _, _ := newTestProxyWithGateway(t, stub)
	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
		Body:         strings.NewReader("hello"),
		StorageClass: types.StorageClass("GLACIER"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if stub.putIn.StorageClass != "GLACIER" {
		t.Errorf("backend got class %q, want the client's", stub.putIn.StorageClass)
	}
	head, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if head.StorageClass != "hdd" {
		t.Errorf("HeadObject shows class %q, want the backend's", head.StorageClass)
	}
}

// The writes whose size is not known when the class must be chosen hand the
// mapper SizeUnknown; its answer still reaches the backend.
func TestStorageClassSizeUnknown(t *testing.T) {
	stub := &stubBackend{
		createMPUOut: &s3.CreateMultipartUploadOutput{UploadId: aws.String("u1")},
		copyOut:      &s3.CopyObjectOutput{CopyObjectResult: &types.CopyObjectResult{ETag: aws.String(`"c"`)}},
	}
	client, _, gw := newTestProxyWithGateway(t, stub)
	m := &classMapper{toBackend: func(*s3gw.Op, int64) string { return "hdd" }}
	gw.SetStorageClassMapper(m)

	if _, err := client.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String("testbucket"), Key: aws.String("big.bin"),
	}); err != nil {
		t.Fatal(err)
	}
	if stub.createMPUIn.StorageClass != "hdd" {
		t.Errorf("CreateMultipartUpload sent class %q", stub.createMPUIn.StorageClass)
	}
	if _, err := client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("copy.bin"),
		CopySource: aws.String("testbucket/big.bin"),
	}); err != nil {
		t.Fatal(err)
	}
	if stub.copyIn.StorageClass != "hdd" {
		t.Errorf("CopyObject sent class %q", stub.copyIn.StorageClass)
	}
	if diff := cmp.Diff([]int64{s3gw.SizeUnknown, s3gw.SizeUnknown}, m.sizes); diff != "" {
		t.Errorf("sizes handed to the mapper (-want +got):\n%s", diff)
	}
}

// A POST upload is a write like any other: the mapper chooses the class,
// with the size unknown, and the form's own x-amz-storage-class does not
// reach the backend.
func TestStorageClassOnPostUpload(t *testing.T) {
	gw := newTestGateway(t)
	stub := &stubPost{}
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	m := &classMapper{toBackend: func(*s3gw.Op, int64) string { return "hdd" }}
	gw.SetStorageClassMapper(m)
	form := &postForm{
		conditions: []string{`{"key": "a.txt"}`, `{"x-amz-storage-class": "GLACIER"}`},
		fields:     [][2]string{{"key", "a.txt"}, {"x-amz-storage-class", "GLACIER"}},
		filename:   "a.txt",
		content:    "hello post",
	}
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, form.request(t))
	if w.Code != http.StatusNoContent {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}
	if stub.putIn == nil || stub.putIn.StorageClass != "hdd" {
		t.Errorf("backend got %+v, want class hdd", stub.putIn)
	}
	if diff := cmp.Diff([]int64{s3gw.SizeUnknown}, m.sizes); diff != "" {
		t.Errorf("sizes handed to the mapper (-want +got):\n%s", diff)
	}
}

// ToClient decides what the client is shown, per reported value, while
// Op.Response keeps the backend's own class.
func TestStorageClassToClientOnReads(t *testing.T) {
	now := time.Now()
	stub := &stubBackend{
		headOut: &s3.HeadObjectOutput{ContentLength: aws.Int64(5), StorageClass: "hdd"},
		getOut: &s3.GetObjectOutput{
			Body: io.NopCloser(strings.NewReader("hello")), ContentLength: aws.Int64(5), StorageClass: "ssd",
		},
		listOut: &s3.ListObjectsV2Output{Contents: []types.Object{
			{Key: aws.String("a"), Size: aws.Int64(1), LastModified: &now, StorageClass: "ssd"},
			{Key: aws.String("b"), Size: aws.Int64(1), LastModified: &now, StorageClass: "hdd"},
			{Key: aws.String("c"), Size: aws.Int64(1), LastModified: &now},
		}},
		listVerOut: &s3.ListObjectVersionsOutput{Versions: []types.ObjectVersion{
			{Key: aws.String("a"), VersionId: aws.String("v1"), StorageClass: "hdd"},
		}},
		getAttrOut: &s3.GetObjectAttributesOutput{ObjectSize: aws.Int64(1), StorageClass: "hdd"},
		listMPUOut: &s3.ListMultipartUploadsOutput{Uploads: []types.MultipartUpload{
			{Key: aws.String("m"), UploadId: aws.String("u1"), StorageClass: "hdd"},
		}},
		listPartsOut: &s3.ListPartsOutput{StorageClass: "hdd"},
	}
	client, _, gw := newTestProxyWithGateway(t, stub)
	// the tenant sees one class, and the backend's names never
	m := &classMapper{toClient: func(*s3gw.Op, string) string { return "STANDARD" }}
	gw.SetStorageClassMapper(m)
	var responses []*s3gw.OpResponse
	gw.Use(func(_ context.Context, op *s3gw.Op, next func() error) error {
		err := next()
		responses = append(responses, op.Response)
		return err
	})
	ctx := t.Context()
	bucket := aws.String("testbucket")

	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("a")})
	if err != nil {
		t.Fatal(err)
	}
	if head.StorageClass != "STANDARD" {
		t.Errorf("HeadObject shows class %q", head.StorageClass)
	}
	get, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: aws.String("a")})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, get.Body)
	get.Body.Close()
	if get.StorageClass != "STANDARD" {
		t.Errorf("GetObject shows class %q", get.StorageClass)
	}
	list, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range list.Contents {
		if o.StorageClass != "STANDARD" {
			t.Errorf("ListObjectsV2 shows %s in class %q", *o.Key, o.StorageClass)
		}
	}
	vers, err := client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if vers.Versions[0].StorageClass != "STANDARD" {
		t.Errorf("ListObjectVersions shows class %q", vers.Versions[0].StorageClass)
	}
	attrs, err := client.GetObjectAttributes(ctx, &s3.GetObjectAttributesInput{
		Bucket: bucket, Key: aws.String("a"),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesStorageClass},
	})
	if err != nil {
		t.Fatal(err)
	}
	if attrs.StorageClass != "STANDARD" {
		t.Errorf("GetObjectAttributes shows class %q", attrs.StorageClass)
	}
	mpus, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if mpus.Uploads[0].StorageClass != "STANDARD" {
		t.Errorf("ListMultipartUploads shows class %q", mpus.Uploads[0].StorageClass)
	}
	parts, err := client.ListParts(ctx, &s3.ListPartsInput{
		Bucket: bucket, Key: aws.String("m"), UploadId: aws.String("u1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if parts.StorageClass != "STANDARD" {
		t.Errorf("ListParts shows class %q", parts.StorageClass)
	}

	// every reported value went through the mapper, the default class ("")
	// of the listing's third object included
	want := []string{"hdd", "ssd", "ssd", "hdd", "", "hdd", "hdd", "hdd", "hdd"}
	if diff := cmp.Diff(want, m.classes); diff != "" {
		t.Errorf("classes handed to the mapper (-want +got):\n%s", diff)
	}
	// while the hooks saw the backend's names
	if responses[0] == nil || responses[0].StorageClass != "hdd" {
		t.Errorf("HeadObject Op.Response = %+v, want the backend's class", responses[0])
	}
	if responses[1] == nil || responses[1].StorageClass != "ssd" {
		t.Errorf("GetObject Op.Response = %+v, want the backend's class", responses[1])
	}
}

// Returning "" hides the class entirely: no header, no element, which is
// how S3 itself reports the default class.
func TestStorageClassToClientEmptyOmits(t *testing.T) {
	gw := newTestGateway(t)
	stub := &stubBackend{
		headOut: &s3.HeadObjectOutput{ContentLength: aws.Int64(5), StorageClass: "hdd"},
	}
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	gw.SetStorageClassMapper(&classMapper{toClient: func(*s3gw.Op, string) string { return "" }})

	req := signedRequest(t, "HEAD", "http://s3.example.com/testbucket/a", nil, emptyPayloadHash, time.Now(), testCreds(), nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", w.Code)
	}
	if v, ok := w.Header()["X-Amz-Storage-Class"]; ok {
		t.Errorf("expect no x-amz-storage-class header, got %q", v)
	}
}
