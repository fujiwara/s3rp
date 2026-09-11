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

// The KMS key mapper: ToBackend chooses the key id an aws:kms write sends
// the backend in place of the client's, ToClient chooses what the client is
// shown for a key id the backend reported, while Op.Response keeps the
// backend's value.

// keyMapper is a test mapper built from two funcs, recording what each was
// handed. A nil func passes its direction through.
type keyMapper struct {
	toBackend func(op *s3gw.Op, keyID string) string
	toClient  func(op *s3gw.Op, keyID string) string
	requested []string
	reported  []string
}

func (m *keyMapper) ToBackend(op *s3gw.Op, keyID string) string {
	m.requested = append(m.requested, keyID)
	if m.toBackend == nil {
		return keyID
	}
	return m.toBackend(op, keyID)
}

func (m *keyMapper) ToClient(op *s3gw.Op, keyID string) string {
	m.reported = append(m.reported, keyID)
	if m.toClient == nil {
		return keyID
	}
	return m.toClient(op, keyID)
}

// aliases resolves the tenant's key names to the backend's ids and back.
func aliases() *keyMapper {
	return &keyMapper{
		toBackend: func(op *s3gw.Op, keyID string) string {
			switch keyID {
			case "", "default":
				return "vault/" + op.Tenant + "/default"
			}
			return "vault/" + op.Tenant + "/" + keyID
		},
		toClient: func(op *s3gw.Op, keyID string) string {
			return strings.TrimPrefix(keyID, "vault/"+op.Tenant+"/")
		},
	}
}

func TestKMSKeyToBackendOnWrites(t *testing.T) {
	stub := &stubBackend{
		putOut:       &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
		createMPUOut: &s3.CreateMultipartUploadOutput{UploadId: aws.String("u1")},
		copyOut:      &s3.CopyObjectOutput{CopyObjectResult: &types.CopyObjectResult{ETag: aws.String(`"c"`)}},
	}
	client, _, gw := newTestProxyWithGateway(t, stub)
	rec := &opRecorder{}
	gw.SetAuthorizer(rec)
	m := aliases()
	gw.SetKMSKeyMapper(m)
	ctx := t.Context()
	bucket := aws.String("testbucket")

	// a named key
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("a"), Body: strings.NewReader("x"),
		ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: aws.String("archive"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(stub.putIn.SSEKMSKeyId); got != "vault/testtenant/archive" {
		t.Errorf("PutObject sent key %q", got)
	}
	// aws:kms without a key: the mapper picks the tenant's default
	if _, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket: bucket, Key: aws.String("b"), ServerSideEncryption: types.ServerSideEncryptionAwsKms,
	}); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(stub.createMPUIn.SSEKMSKeyId); got != "vault/testtenant/default" {
		t.Errorf("CreateMultipartUpload sent key %q", got)
	}
	if _, err := client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket: bucket, Key: aws.String("c"), CopySource: aws.String("testbucket/a"),
		ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: aws.String("archive"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(stub.copyIn.SSEKMSKeyId); got != "vault/testtenant/archive" {
		t.Errorf("CopyObject sent key %q", got)
	}
	// AES256 and no SSE: the mapper is not consulted at all
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("d"), Body: strings.NewReader("x"),
		ServerSideEncryption: types.ServerSideEncryptionAes256,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: bucket, Key: aws.String("e"), Body: strings.NewReader("x"),
	}); err != nil {
		t.Fatal(err)
	}
	if stub.putIn.SSEKMSKeyId != nil {
		t.Errorf("plain PutObject sent key %q", *stub.putIn.SSEKMSKeyId)
	}
	if diff := cmp.Diff([]string{"archive", "", "archive"}, m.requested); diff != "" {
		t.Errorf("key ids handed to the mapper (-want +got):\n%s", diff)
	}
	// the Authorizer still sees the client's own id
	if op := rec.ops[0]; op.Request == nil || op.Request.SSEKMSKeyID != "archive" {
		t.Errorf("expect the client's key id on the op, got %+v", op.Request)
	}
}

// "" from ToBackend sends no key id, so the backend applies its default.
func TestKMSKeyToBackendEmptySendsNone(t *testing.T) {
	stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)}}
	client, _, gw := newTestProxyWithGateway(t, stub)
	gw.SetKMSKeyMapper(&keyMapper{toBackend: func(*s3gw.Op, string) string { return "" }})
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a"), Body: strings.NewReader("x"),
		ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: aws.String("archive"),
	}); err != nil {
		t.Fatal(err)
	}
	if stub.putIn.ServerSideEncryption != types.ServerSideEncryptionAwsKms {
		t.Errorf("expect aws:kms to be sent, got %q", stub.putIn.ServerSideEncryption)
	}
	if stub.putIn.SSEKMSKeyId != nil {
		t.Errorf("expect no key id, got %q", *stub.putIn.SSEKMSKeyId)
	}
}

// A POST upload is a write like any other.
func TestKMSKeyToBackendOnPostUpload(t *testing.T) {
	gw := newTestGateway(t)
	stub := &stubPost{}
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	gw.SetKMSKeyMapper(aliases())
	form := &postForm{
		conditions: []string{`{"key": "a.txt"}`, `{"x-amz-server-side-encryption": "aws:kms"}`,
			`{"x-amz-server-side-encryption-aws-kms-key-id": "archive"}`},
		fields: [][2]string{{"key", "a.txt"}, {"x-amz-server-side-encryption", "aws:kms"},
			{"x-amz-server-side-encryption-aws-kms-key-id", "archive"}},
		filename: "a.txt", content: "hello",
	}
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, form.request(t))
	if w.Code != http.StatusNoContent {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}
	if stub.putIn == nil || aws.ToString(stub.putIn.SSEKMSKeyId) != "vault/testtenant/archive" {
		t.Errorf("backend got %+v, want the mapped key", stub.putIn)
	}
}

// ToClient decides what the client is shown wherever a key id is reported,
// while Op.Response keeps the backend's.
func TestKMSKeyToClientOnReports(t *testing.T) {
	kms, backendKey := types.ServerSideEncryptionAwsKms, aws.String("vault/testtenant/archive")
	stub := &stubBackend{
		putOut:  &s3.PutObjectOutput{ETag: aws.String(`"e"`), ServerSideEncryption: kms, SSEKMSKeyId: backendKey},
		headOut: &s3.HeadObjectOutput{ContentLength: aws.Int64(1), ServerSideEncryption: kms, SSEKMSKeyId: backendKey},
		getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("x")), ContentLength: aws.Int64(1),
			ServerSideEncryption: kms, SSEKMSKeyId: backendKey},
		copyOut: &s3.CopyObjectOutput{CopyObjectResult: &types.CopyObjectResult{ETag: aws.String(`"c"`)},
			ServerSideEncryption: kms, SSEKMSKeyId: backendKey},
		createMPUOut:   &s3.CreateMultipartUploadOutput{UploadId: aws.String("u1"), ServerSideEncryption: kms, SSEKMSKeyId: backendKey},
		completeMPUOut: &s3.CompleteMultipartUploadOutput{ETag: aws.String(`"m"`), ServerSideEncryption: kms, SSEKMSKeyId: backendKey},
		getEncOut: &s3.GetBucketEncryptionOutput{ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
			Rules: []types.ServerSideEncryptionRule{{ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{
				SSEAlgorithm: kms, KMSMasterKeyID: backendKey}}}}},
	}
	client, _, gw := newTestProxyWithGateway(t, stub)
	m := aliases()
	gw.SetKMSKeyMapper(m)
	var responses []*s3gw.OpResponse
	gw.Use(func(_ context.Context, op *s3gw.Op, next func() error) error {
		err := next()
		responses = append(responses, op.Response)
		return err
	})
	ctx := t.Context()
	bucket := aws.String("testbucket")

	put, err := client.PutObject(ctx, &s3.PutObjectInput{Bucket: bucket, Key: aws.String("a"), Body: strings.NewReader("x"),
		ServerSideEncryption: kms, SSEKMSKeyId: aws.String("archive")})
	if err != nil {
		t.Fatal(err)
	}
	head, err := client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: bucket, Key: aws.String("a")})
	if err != nil {
		t.Fatal(err)
	}
	get, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: bucket, Key: aws.String("a")})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, get.Body)
	get.Body.Close()
	cp, err := client.CopyObject(ctx, &s3.CopyObjectInput{Bucket: bucket, Key: aws.String("c"), CopySource: aws.String("testbucket/a")})
	if err != nil {
		t.Fatal(err)
	}
	mpu, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: bucket, Key: aws.String("m")})
	if err != nil {
		t.Fatal(err)
	}
	done, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{Bucket: bucket, Key: aws.String("m"), UploadId: aws.String("u1"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: aws.String(`"p"`)}}}})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := client.GetBucketEncryption(ctx, &s3.GetBucketEncryptionInput{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}

	for name, got := range map[string]*string{
		"PutObject": put.SSEKMSKeyId, "HeadObject": head.SSEKMSKeyId, "GetObject": get.SSEKMSKeyId,
		"CopyObject": cp.SSEKMSKeyId, "CreateMultipartUpload": mpu.SSEKMSKeyId, "CompleteMultipartUpload": done.SSEKMSKeyId,
		"GetBucketEncryption": enc.ServerSideEncryptionConfiguration.Rules[0].ApplyServerSideEncryptionByDefault.KMSMasterKeyID,
	} {
		if aws.ToString(got) != "archive" {
			t.Errorf("%s shows key %q, want the tenant's name", name, aws.ToString(got))
		}
	}
	// every reported id went through the mapper, and only reported ones
	want := []string{"vault/testtenant/archive", "vault/testtenant/archive", "vault/testtenant/archive",
		"vault/testtenant/archive", "vault/testtenant/archive", "vault/testtenant/archive", "vault/testtenant/archive"}
	if diff := cmp.Diff(want, m.reported); diff != "" {
		t.Errorf("key ids handed to the mapper (-want +got):\n%s", diff)
	}
	// while the hooks saw the backend's id
	if responses[0] == nil || responses[0].SSEKMSKeyID != "vault/testtenant/archive" {
		t.Errorf("PutObject Op.Response = %+v, want the backend's key id", responses[0])
	}
}

// "" from ToClient omits the header; a backend that reported no key never
// consults the mapper.
func TestKMSKeyToClientEmptyOmits(t *testing.T) {
	gw := newTestGateway(t)
	stub := &stubBackend{headOut: &s3.HeadObjectOutput{ContentLength: aws.Int64(1),
		ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: aws.String("vault/x")}}
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	m := &keyMapper{toClient: func(*s3gw.Op, string) string { return "" }}
	gw.SetKMSKeyMapper(m)

	req := signedRequest(t, "HEAD", "http://s3.example.com/testbucket/a", nil, emptyPayloadHash, time.Now(), testCreds(), nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", w.Code)
	}
	if v, ok := w.Header()["X-Amz-Server-Side-Encryption-Aws-Kms-Key-Id"]; ok {
		t.Errorf("expect no key id header, got %q", v)
	}
	if got := w.Header().Get("x-amz-server-side-encryption"); got != "aws:kms" {
		t.Errorf("the mode is still reported, got %q", got)
	}

	// no key reported (SSE-S3): nothing to map
	stub.headOut = &s3.HeadObjectOutput{ContentLength: aws.Int64(1), ServerSideEncryption: types.ServerSideEncryptionAes256}
	w = httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, signedRequest(t, "HEAD", "http://s3.example.com/testbucket/a", nil, emptyPayloadHash, time.Now(), testCreds(), nil))
	if len(m.reported) != 1 {
		t.Errorf("expect the mapper consulted once, got %v", m.reported)
	}
}

// Without a mapper both directions pass through as before.
func TestKMSKeyPassthroughWithoutMapper(t *testing.T) {
	stub := &stubBackend{
		putOut:  &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
		headOut: &s3.HeadObjectOutput{ContentLength: aws.Int64(1), ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: aws.String("vault/x")},
	}
	client, _, _ := newTestProxyWithGateway(t, stub)
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a"), Body: strings.NewReader("x"),
		ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: aws.String("vault/x"),
	}); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(stub.putIn.SSEKMSKeyId) != "vault/x" {
		t.Errorf("backend got key %q, want the client's", aws.ToString(stub.putIn.SSEKMSKeyId))
	}
	head, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String("testbucket"), Key: aws.String("a")})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(head.SSEKMSKeyId) != "vault/x" {
		t.Errorf("HeadObject shows key %q, want the backend's", aws.ToString(head.SSEKMSKeyId))
	}
}
