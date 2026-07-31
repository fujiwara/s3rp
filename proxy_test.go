package s3rp_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp"
	"github.com/google/go-cmp/cmp"
)

// stubBackend implements s3rp.BackendClient recording inputs and returning
// canned outputs.
type stubBackend struct {
	getIn   *s3.GetObjectInput
	getOut  *s3.GetObjectOutput
	getErr  error
	putIn   *s3.PutObjectInput
	putBody []byte
	putOut  *s3.PutObjectOutput
	headIn  *s3.HeadObjectInput
	headOut *s3.HeadObjectOutput
	delIn   *s3.DeleteObjectInput
	delOut  *s3.DeleteObjectOutput
	listIn  *s3.ListObjectsV2Input
	listOut *s3.ListObjectsV2Output
	hbIn    *s3.HeadBucketInput
	hbOut   *s3.HeadBucketOutput
}

func (b *stubBackend) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	b.getIn = in
	if b.getErr != nil {
		return nil, b.getErr
	}
	return b.getOut, nil
}

func (b *stubBackend) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	b.putIn = in
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	b.putBody = body
	return b.putOut, nil
}

func (b *stubBackend) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	b.headIn = in
	return b.headOut, nil
}

func (b *stubBackend) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	b.delIn = in
	return b.delOut, nil
}

func (b *stubBackend) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	b.listIn = in
	return b.listOut, nil
}

func (b *stubBackend) HeadBucket(ctx context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	b.hbIn = in
	return b.hbOut, nil
}

// newTestProxy boots the proxy on an httptest server with a stub backend and
// returns a real aws-sdk-go-v2 S3 client pointed at it with front credentials.
func newTestProxy(t *testing.T, stub *stubBackend) (*s3.Client, *httptest.Server) {
	t.Helper()
	app := newTestApp(t)
	app.SetBackend("testbucket", stub)
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)
	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, ""),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts.URL)
		o.UsePathStyle = true
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return client, ts
}

func TestProxyGetObject(t *testing.T) {
	lastModified := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	stub := &stubBackend{
		getOut: &s3.GetObjectOutput{
			Body:          io.NopCloser(strings.NewReader("hello world")),
			ContentLength: aws.Int64(11),
			ContentType:   aws.String("text/plain"),
			ETag:          aws.String(`"abc123"`),
			LastModified:  aws.Time(lastModified),
			Metadata:      map[string]string{"foo": "bar"},
		},
	}
	client, _ := newTestProxy(t, stub)
	out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("dir/key.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	body, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "hello world" {
		t.Errorf("expect hello world, got %s", body)
	}
	if aws.ToString(out.ETag) != `"abc123"` {
		t.Errorf("unexpected etag %v", out.ETag)
	}
	if aws.ToString(out.ContentType) != "text/plain" {
		t.Errorf("unexpected content type %v", out.ContentType)
	}
	if !aws.ToTime(out.LastModified).Equal(lastModified) {
		t.Errorf("unexpected last modified %v", out.LastModified)
	}
	if diff := cmp.Diff(map[string]string{"foo": "bar"}, out.Metadata); diff != "" {
		t.Errorf("metadata mismatch (-want +got):\n%s", diff)
	}
	// the backend must receive the renamed bucket
	if aws.ToString(stub.getIn.Bucket) != "backend-testbucket" {
		t.Errorf("expect backend-testbucket, got %s", aws.ToString(stub.getIn.Bucket))
	}
	if aws.ToString(stub.getIn.Key) != "dir/key.txt" {
		t.Errorf("unexpected key %s", aws.ToString(stub.getIn.Key))
	}
}

func TestProxyGetObjectNoSuchKey(t *testing.T) {
	stub := &stubBackend{
		getErr: &types.NoSuchKey{Message: aws.String("The specified key does not exist.")},
	}
	client, _ := newTestProxy(t, stub)
	_, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("missing.txt"),
	})
	if err == nil {
		t.Fatal("expect error")
	}
	var nsk *types.NoSuchKey
	if !errors.As(err, &nsk) {
		t.Errorf("expect NoSuchKey, got %v", err)
	}
}

func TestProxyPutObject(t *testing.T) {
	stub := &stubBackend{
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"put-etag"`)},
	}
	client, _ := newTestProxy(t, stub)
	content := strings.Repeat("0123456789", 1000)
	out, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket:      aws.String("testbucket"),
		Key:         aws.String("upload.txt"),
		Body:        strings.NewReader(content),
		ContentType: aws.String("text/plain"),
		Metadata:    map[string]string{"uploader": "s3rp-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.ETag) != `"put-etag"` {
		t.Errorf("unexpected etag %v", out.ETag)
	}
	if string(stub.putBody) != content {
		t.Errorf("body mismatch: got %d bytes", len(stub.putBody))
	}
	if aws.ToString(stub.putIn.Bucket) != "backend-testbucket" {
		t.Errorf("expect backend-testbucket, got %s", aws.ToString(stub.putIn.Bucket))
	}
	if aws.ToString(stub.putIn.ContentType) != "text/plain" {
		t.Errorf("unexpected content type %v", stub.putIn.ContentType)
	}
	if diff := cmp.Diff(map[string]string{"uploader": "s3rp-test"}, stub.putIn.Metadata); diff != "" {
		t.Errorf("metadata mismatch (-want +got):\n%s", diff)
	}
	if aws.ToInt64(stub.putIn.ContentLength) != int64(len(content)) {
		t.Errorf("unexpected content length %v", stub.putIn.ContentLength)
	}
}

// TestProxyPutObjectDefaultChecksum exercises the aws-chunked path: the SDK's
// default checksum settings send trailing checksums over plain http.
func TestProxyPutObjectDefaultChecksum(t *testing.T) {
	stub := &stubBackend{
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"put-etag"`)},
	}
	app := newTestApp(t)
	app.SetBackend("testbucket", stub)
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)
	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, ""),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	// default RequestChecksumCalculation (WhenSupported)
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts.URL)
		o.UsePathStyle = true
	})
	content := strings.Repeat("checksummed content ", 100)
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("checksummed.txt"),
		Body:   strings.NewReader(content),
	}); err != nil {
		t.Fatal(err)
	}
	if string(stub.putBody) != content {
		t.Errorf("body mismatch: got %d bytes, want %d", len(stub.putBody), len(content))
	}
}

func TestProxyHeadObject(t *testing.T) {
	stub := &stubBackend{
		headOut: &s3.HeadObjectOutput{
			ContentLength: aws.Int64(42),
			ContentType:   aws.String("application/octet-stream"),
			ETag:          aws.String(`"head-etag"`),
		},
	}
	client, _ := newTestProxy(t, stub)
	out, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("key.bin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToInt64(out.ContentLength) != 42 {
		t.Errorf("unexpected content length %v", out.ContentLength)
	}
	if aws.ToString(out.ETag) != `"head-etag"` {
		t.Errorf("unexpected etag %v", out.ETag)
	}
}

func TestProxyDeleteObject(t *testing.T) {
	stub := &stubBackend{
		delOut: &s3.DeleteObjectOutput{},
	}
	client, _ := newTestProxy(t, stub)
	if _, err := client.DeleteObject(t.Context(), &s3.DeleteObjectInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("gone.txt"),
	}); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(stub.delIn.Key) != "gone.txt" {
		t.Errorf("unexpected key %v", stub.delIn.Key)
	}
}

func TestProxyListObjectsV2(t *testing.T) {
	stub := &stubBackend{
		listOut: &s3.ListObjectsV2Output{
			KeyCount:    aws.Int32(2),
			MaxKeys:     aws.Int32(1000),
			IsTruncated: aws.Bool(false),
			Prefix:      aws.String("dir/"),
			Contents: []types.Object{
				{
					Key:          aws.String("dir/a.txt"),
					Size:         aws.Int64(10),
					ETag:         aws.String(`"etag-a"`),
					LastModified: aws.Time(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
				},
				{
					Key:          aws.String("dir/b.txt"),
					Size:         aws.Int64(20),
					ETag:         aws.String(`"etag-b"`),
					LastModified: aws.Time(time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)),
				},
			},
			CommonPrefixes: []types.CommonPrefix{
				{Prefix: aws.String("dir/sub/")},
			},
		},
	}
	client, _ := newTestProxy(t, stub)
	out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket:    aws.String("testbucket"),
		Prefix:    aws.String("dir/"),
		Delimiter: aws.String("/"),
		MaxKeys:   aws.Int32(1000),
	})
	if err != nil {
		t.Fatal(err)
	}
	// the Name must be the front bucket name, not the backend one
	if aws.ToString(out.Name) != "testbucket" {
		t.Errorf("expect testbucket, got %s", aws.ToString(out.Name))
	}
	if len(out.Contents) != 2 {
		t.Fatalf("expect 2 contents, got %d", len(out.Contents))
	}
	if aws.ToString(out.Contents[0].Key) != "dir/a.txt" {
		t.Errorf("unexpected key %v", out.Contents[0].Key)
	}
	if aws.ToInt64(out.Contents[1].Size) != 20 {
		t.Errorf("unexpected size %v", out.Contents[1].Size)
	}
	if len(out.CommonPrefixes) != 1 || aws.ToString(out.CommonPrefixes[0].Prefix) != "dir/sub/" {
		t.Errorf("unexpected common prefixes %v", out.CommonPrefixes)
	}
	if aws.ToString(stub.listIn.Bucket) != "backend-testbucket" {
		t.Errorf("expect backend-testbucket, got %s", aws.ToString(stub.listIn.Bucket))
	}
	if aws.ToString(stub.listIn.Prefix) != "dir/" {
		t.Errorf("unexpected prefix %v", stub.listIn.Prefix)
	}
}

func TestProxyHeadBucket(t *testing.T) {
	stub := &stubBackend{hbOut: &s3.HeadBucketOutput{}}
	client, _ := newTestProxy(t, stub)
	if _, err := client.HeadBucket(t.Context(), &s3.HeadBucketInput{
		Bucket: aws.String("testbucket"),
	}); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(stub.hbIn.Bucket) != "backend-testbucket" {
		t.Errorf("expect backend-testbucket, got %s", aws.ToString(stub.hbIn.Bucket))
	}
}

func TestProxyListBuckets(t *testing.T) {
	client, _ := newTestProxy(t, &stubBackend{})
	out, err := client.ListBuckets(t.Context(), &s3.ListBucketsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Buckets) != 1 {
		t.Fatalf("expect 1 bucket, got %d", len(out.Buckets))
	}
	if aws.ToString(out.Buckets[0].Name) != "testbucket" {
		t.Errorf("expect testbucket, got %s", aws.ToString(out.Buckets[0].Name))
	}
}

func TestProxyAccessDeniedBucket(t *testing.T) {
	client, _ := newTestProxy(t, &stubBackend{})
	_, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("otherbucket"),
		Key:    aws.String("key.txt"),
	})
	if err == nil {
		t.Fatal("expect error")
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "AccessDenied" {
		t.Errorf("expect AccessDenied, got %v", err)
	}
}

func TestProxyNotImplemented(t *testing.T) {
	testCases := []struct {
		name string
		call func(ctx context.Context, client *s3.Client) error
	}{
		{
			name: "GetObjectAcl",
			call: func(ctx context.Context, client *s3.Client) error {
				_, err := client.GetObjectAcl(ctx, &s3.GetObjectAclInput{
					Bucket: aws.String("testbucket"), Key: aws.String("key.txt"),
				})
				return err
			},
		},
		{
			name: "CreateMultipartUpload",
			call: func(ctx context.Context, client *s3.Client) error {
				_, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
					Bucket: aws.String("testbucket"), Key: aws.String("key.txt"),
				})
				return err
			},
		},
		{
			name: "DeleteObjects",
			call: func(ctx context.Context, client *s3.Client) error {
				_, err := client.DeleteObjects(ctx, &s3.DeleteObjectsInput{
					Bucket: aws.String("testbucket"),
					Delete: &types.Delete{Objects: []types.ObjectIdentifier{{Key: aws.String("k")}}},
				})
				return err
			},
		},
		{
			name: "CopyObject",
			call: func(ctx context.Context, client *s3.Client) error {
				_, err := client.CopyObject(ctx, &s3.CopyObjectInput{
					Bucket:     aws.String("testbucket"),
					Key:        aws.String("dst.txt"),
					CopySource: aws.String("testbucket/src.txt"),
				})
				return err
			},
		},
	}
	client, _ := newTestProxy(t, &stubBackend{})
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call(t.Context(), client)
			if err == nil {
				t.Fatal("expect error")
			}
			var apiErr smithy.APIError
			if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NotImplemented" {
				t.Errorf("expect NotImplemented, got %v", err)
			}
		})
	}
}

// TestProxyRawChunkedPut sends a hand-crafted signed aws-chunked PUT,
// as the aws CLI does over plain http endpoints.
func TestProxyRawChunkedPut(t *testing.T) {
	stub := &stubBackend{
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"chunked-etag"`)},
	}
	app := newTestApp(t)
	app.SetBackend("testbucket", stub)
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)

	data := []byte(strings.Repeat("streaming payload ", 1000))
	signTime := time.Now().UTC()

	// sign the request headers with the STREAMING payload hash first
	req, err := http.NewRequest(http.MethodPut, ts.URL+"/testbucket/streamed.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Amz-Content-Sha256", "STREAMING-AWS4-HMAC-SHA256-PAYLOAD")
	req.Header.Set("X-Amz-Decoded-Content-Length", fmt.Sprint(len(data)))
	req.Header.Set("Content-Encoding", "aws-chunked")
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	if err := signer.SignHTTP(t.Context(), testCreds(), req, "STREAMING-AWS4-HMAC-SHA256-PAYLOAD", "s3", "us-east-1", signTime); err != nil {
		t.Fatal(err)
	}
	auth := req.Header.Get("Authorization")
	_, sig, _ := strings.Cut(auth, "Signature=")

	vr := &s3rp.VerifiedRequest{
		SecretAccessKey: testSecretAccessKey,
		Signature:       sig,
		SigningTime:     signTime,
		Scope:           strings.Join([]string{signTime.Format("20060102"), "us-east-1", "s3", "aws4_request"}, "/"),
		Region:          "us-east-1",
		PayloadHash:     "STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
	}
	encoded := encodeSignedChunks(t, vr, data, 8192)
	req.Body = io.NopCloser(bytes.NewReader(encoded))
	req.ContentLength = int64(len(encoded))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expect 200, got %d: %s", resp.StatusCode, respBody)
	}
	if !bytes.Equal(stub.putBody, data) {
		t.Errorf("body mismatch: got %d bytes, want %d", len(stub.putBody), len(data))
	}
	if aws.ToInt64(stub.putIn.ContentLength) != int64(len(data)) {
		t.Errorf("unexpected decoded content length %v", stub.putIn.ContentLength)
	}
}
