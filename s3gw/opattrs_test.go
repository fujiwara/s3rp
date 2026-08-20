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

// What a hook can tell about the object itself: Op.Request is what the
// client asked for and is there before the Authorizer decides, Op.Response
// is what the backend reported and does not exist until the operation ran.

type opRecorder struct {
	ops []*s3gw.Op
	// hadResponse records, at the moment Authorize ran, whether a response
	// was already attached — the ops themselves are filled in afterwards.
	hadResponse []bool
}

func (r *opRecorder) Authorize(_ context.Context, op *s3gw.Op) error {
	r.ops = append(r.ops, op)
	r.hadResponse = append(r.hadResponse, op.Response != nil)
	return nil
}

func TestOpRequestOnUpload(t *testing.T) {
	stub := &stubBackend{putOut: &s3.PutObjectOutput{
		ETag:                 aws.String(`"put-etag"`),
		VersionId:            aws.String("v1"),
		ServerSideEncryption: types.ServerSideEncryptionAes256,
	}}
	client, _, gw := newTestProxyWithGateway(t, stub)
	rec := &opRecorder{}
	gw.SetAuthorizer(rec)

	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket:       aws.String("testbucket"),
		Key:          aws.String("a.txt"),
		Body:         strings.NewReader("hello"),
		StorageClass: types.StorageClass("PLAN_COLD"),
		Metadata:     map[string]string{"plan": "cold"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.ops) != 1 {
		t.Fatalf("expect one authorization, got %d", len(rec.ops))
	}
	op := rec.ops[0]

	// what the client asked for, in time for the Authorizer to refuse it
	if op.Request == nil {
		t.Fatal("expect the upload's request attributes on the op")
	}
	if op.Request.StorageClass != "PLAN_COLD" {
		t.Errorf("unexpected storage class %q", op.Request.StorageClass)
	}
	if diff := cmp.Diff(map[string]string{"plan": "cold"}, op.Request.Metadata); diff != "" {
		t.Errorf("unexpected request metadata (-want +got):\n%s", diff)
	}
	if rec.hadResponse[0] {
		t.Error("expect no response while the Authorizer runs")
	}

	// what the backend reported, once it had run. A write returns no
	// storage class — the S3 API has no field for it — so the class an
	// upload landed in is not knowable here.
	if op.Response == nil {
		t.Fatal("expect the backend's answer on the op")
	}
	if op.Response.ETag != `"put-etag"` || op.Response.VersionID != "v1" {
		t.Errorf("unexpected response %+v", op.Response)
	}
	if op.Response.SSE != "AES256" {
		t.Errorf("expect the encryption the backend applied, got %q", op.Response.SSE)
	}
	if op.Response.StorageClass != "" {
		t.Errorf("expect no storage class from a write, got %q", op.Response.StorageClass)
	}
}

// A non-nil Request or Response means at least one field is set, never all
// of them, so Metadata stays nil next to a filled storage class. Reading a
// nil map is safe, which is what makes that a usable contract.
func TestOpMetadataNilWhenAbsent(t *testing.T) {
	stub := &stubBackend{
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"put-etag"`)},
		getOut: &s3.GetObjectOutput{
			Body: io.NopCloser(strings.NewReader("hello")),
			ETag: aws.String(`"get-etag"`),
		},
	}
	client, _, gw := newTestProxyWithGateway(t, stub)
	rec := &opRecorder{}
	gw.SetAuthorizer(rec)

	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket:       aws.String("testbucket"),
		Key:          aws.String("a.txt"),
		Body:         strings.NewReader("hello"),
		StorageClass: types.StorageClass("PLAN_COLD"),
	}); err != nil {
		t.Fatal(err)
	}
	put := rec.ops[0]
	if put.Request == nil || put.Request.StorageClass != "PLAN_COLD" {
		t.Fatalf("expect the class on the request, got %+v", put.Request)
	}
	if put.Request.Metadata != nil {
		t.Errorf("expect no metadata map, got %v", put.Request.Metadata)
	}
	if v := put.Request.Metadata["plan"]; v != "" {
		t.Errorf("reading the nil map must answer empty, got %q", v)
	}

	out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, out.Body)
	out.Body.Close()
	get := rec.ops[1]
	if get.Response == nil || get.Response.ETag != `"get-etag"` {
		t.Fatalf("expect the etag on the response, got %+v", get.Response)
	}
	if get.Response.Metadata != nil {
		t.Errorf("expect no metadata map, got %v", get.Response.Metadata)
	}
}

// An operation that ignores a header must not report it as something the
// client asked for: GetObject does nothing with x-amz-server-side-encryption,
// so a signed one leaves Op.Request nil rather than showing an Authorizer an
// encryption request that will never happen. The value is still validated —
// dispatch checks it on every route — it is just not an attribute of this
// operation.
func TestOpRequestIgnoresInertHeader(t *testing.T) {
	gw := newTestGateway(t)
	stub := &stubBackend{getOut: &s3.GetObjectOutput{
		Body: io.NopCloser(strings.NewReader("hello")),
	}}
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	rec := &opRecorder{}
	gw.SetAuthorizer(rec)

	req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
		nil, emptyPayloadHash, time.Now(), testCreds(), func(r *http.Request) {
			r.Header.Set("x-amz-server-side-encryption", "AES256")
		})
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}
	if len(rec.ops) != 1 {
		t.Fatalf("expect one authorization, got %d", len(rec.ops))
	}
	if rec.ops[0].Request != nil {
		t.Errorf("expect no request attributes on a download, got %+v", rec.ops[0].Request)
	}
}

func TestOpResponseOnDownload(t *testing.T) {
	stub := &stubBackend{getOut: &s3.GetObjectOutput{
		Body:         io.NopCloser(strings.NewReader("hello")),
		StorageClass: types.StorageClass("PLAN_COLD"),
		Metadata:     map[string]string{"plan": "cold"},
		ETag:         aws.String(`"get-etag"`),
	}}
	client, _, gw := newTestProxyWithGateway(t, stub)
	rec := &opRecorder{}
	gw.SetAuthorizer(rec)
	var before, after *s3gw.OpResponse
	gw.Use(func(_ context.Context, op *s3gw.Op, next func() error) error {
		before = op.Response
		err := next()
		after = op.Response
		return err
	})

	out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, out.Body)
	out.Body.Close()

	op := rec.ops[0]
	// a download asks for nothing about the object, and nil is how a hook
	// tells that apart from asking for an empty value
	if op.Request != nil {
		t.Errorf("expect no request attributes on a download, got %+v", op.Request)
	}
	if before != nil {
		t.Error("expect no response before the operation runs")
	}
	if after == nil {
		t.Fatal("expect the response once next returned")
	}
	// the class the object is actually in, which is the one a lifecycle
	// transition may have moved it to
	if after.StorageClass != "PLAN_COLD" {
		t.Errorf("unexpected storage class %q", after.StorageClass)
	}
	if diff := cmp.Diff(map[string]string{"plan": "cold"}, after.Metadata); diff != "" {
		t.Errorf("unexpected stored metadata (-want +got):\n%s", diff)
	}
	if after.ETag != `"get-etag"` {
		t.Errorf("unexpected etag %q", after.ETag)
	}
}

// A failed operation reports nothing: there is no backend answer to record,
// and Response must not claim otherwise.
func TestOpResponseAbsentOnFailure(t *testing.T) {
	stub := &stubBackend{getErr: &types.NoSuchKey{}}
	client, _, gw := newTestProxyWithGateway(t, stub)
	rec := &opRecorder{}
	gw.SetAuthorizer(rec)

	_, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("gone.txt"),
	})
	if err == nil {
		t.Fatal("expect the backend's error to reach the client")
	}
	if len(rec.ops) != 1 {
		t.Fatalf("expect one authorization, got %d", len(rec.ops))
	}
	if rec.ops[0].Response != nil {
		t.Errorf("expect no response for a failed operation, got %+v", rec.ops[0].Response)
	}
}
