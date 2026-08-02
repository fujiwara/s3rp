package s3gw_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
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
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"
	"github.com/google/go-cmp/cmp"
)

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
	gw := newTestGateway(t)
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	tsURL := newTestServer(t, gw)
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
		o.BaseEndpoint = aws.String(tsURL)
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
					// the backend account; it must never reach the client
					Owner: &types.Owner{ID: aws.String("backend-account"), DisplayName: aws.String("backend-account")},
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
	// without fetch-owner no Owner is present, backend-reported or otherwise
	if out.Contents[0].Owner != nil {
		t.Errorf("expect no owner, got %v", out.Contents[0].Owner)
	}
	// with fetch-owner the Owner is the tenant, never the backend account
	out, err = client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
		Bucket:     aws.String("testbucket"),
		FetchOwner: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Contents) == 0 || out.Contents[0].Owner == nil ||
		aws.ToString(out.Contents[0].Owner.ID) != "testtenant" {
		t.Errorf("expect testtenant owner, got %v", out.Contents[0].Owner)
	}
}

func TestProxyHeadBucket(t *testing.T) {
	// the backend's region must not reach the client: x-amz-bucket-region
	// reports the gateway's region (us-east-1 when unpinned)
	stub := &stubBackend{hbOut: &s3.HeadBucketOutput{BucketRegion: aws.String("backend-region-1")}}
	client, _ := newTestProxy(t, stub)
	out, err := client.HeadBucket(t.Context(), &s3.HeadBucketInput{
		Bucket: aws.String("testbucket"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(stub.hbIn.Bucket) != "backend-testbucket" {
		t.Errorf("expect backend-testbucket, got %s", aws.ToString(stub.hbIn.Bucket))
	}
	if aws.ToString(out.BucketRegion) != "us-east-1" {
		t.Errorf("expect us-east-1, got %s", aws.ToString(out.BucketRegion))
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
	// the owner is the tenant name
	if aws.ToString(out.Owner.ID) != "testtenant" {
		t.Errorf("expect owner testtenant, got %s", aws.ToString(out.Owner.ID))
	}
	// the creation date comes from the store's BucketEntry
	if want := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC); !aws.ToTime(out.Buckets[0].CreationDate).Equal(want) {
		t.Errorf("expect %v, got %v", want, out.Buckets[0].CreationDate)
	}
}

func TestProxyListBucketsNoCreatedAt(t *testing.T) {
	// a store that does not track creation reports the Unix epoch
	gw := s3gw.New(memStore{
		keys: map[string]*store.Key{
			testAccessKeyID: {
				AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey,
				Tenant: "testtenant", User: "testuser",
			},
		},
		buckets: map[string]*store.Bucket{
			"nodate": {Tenant: "testtenant", Name: "nodate", Backend: &store.Backend{}},
		},
	})
	client, _, _ := newSDKClientFor(t, gw)
	out, err := client.ListBuckets(t.Context(), &s3.ListBucketsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Buckets) != 1 {
		t.Fatalf("expect 1 bucket, got %d", len(out.Buckets))
	}
	if !aws.ToTime(out.Buckets[0].CreationDate).Equal(time.Unix(0, 0)) {
		t.Errorf("expect the Unix epoch, got %v", out.Buckets[0].CreationDate)
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
			name: "GetBucketWebsite",
			call: func(ctx context.Context, client *s3.Client) error {
				_, err := client.GetBucketWebsite(ctx, &s3.GetBucketWebsiteInput{
					Bucket: aws.String("testbucket"),
				})
				return err
			},
		},
		{
			name: "GetBucketTagging",
			call: func(ctx context.Context, client *s3.Client) error {
				_, err := client.GetBucketTagging(ctx, &s3.GetBucketTaggingInput{
					Bucket: aws.String("testbucket"),
				})
				return err
			},
		},
		{
			name: "GetBucketLifecycleConfiguration",
			call: func(ctx context.Context, client *s3.Client) error {
				_, err := client.GetBucketLifecycleConfiguration(ctx, &s3.GetBucketLifecycleConfigurationInput{
					Bucket: aws.String("testbucket"),
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

// TestProxyMultipartUpload runs the full multipart flow through a real SDK
// client against a stub backend.
func TestProxyMultipartUpload(t *testing.T) {
	stub := &stubBackend{
		createMPUOut: &s3.CreateMultipartUploadOutput{
			UploadId: aws.String("test-upload-id"),
		},
		completeMPUOut: &s3.CompleteMultipartUploadOutput{
			ETag: aws.String(`"complete-etag"`),
		},
		listPartsOut: &s3.ListPartsOutput{
			IsTruncated: aws.Bool(false),
			MaxParts:    aws.Int32(1000),
			Parts: []types.Part{
				{PartNumber: aws.Int32(1), ETag: aws.String(`"part-etag-1"`), Size: aws.Int64(5)},
				{PartNumber: aws.Int32(2), ETag: aws.String(`"part-etag-2"`), Size: aws.Int64(5)},
			},
		},
	}
	client, _ := newTestProxy(t, stub)
	ctx := t.Context()

	create, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
		Bucket:      aws.String("testbucket"),
		Key:         aws.String("multi/part.bin"),
		ContentType: aws.String("application/octet-stream"),
		Metadata:    map[string]string{"origin": "mpu-test"},
	})
	if err != nil {
		t.Fatal(err)
	}
	uploadID := aws.ToString(create.UploadId)
	if uploadID != "test-upload-id" {
		t.Fatalf("unexpected upload id %s", uploadID)
	}
	if aws.ToString(stub.createMPUIn.Bucket) != "backend-testbucket" {
		t.Errorf("expect backend-testbucket, got %s", aws.ToString(stub.createMPUIn.Bucket))
	}
	if aws.ToString(stub.createMPUIn.ContentType) != "application/octet-stream" {
		t.Errorf("unexpected content type %v", stub.createMPUIn.ContentType)
	}
	if diff := cmp.Diff(map[string]string{"origin": "mpu-test"}, stub.createMPUIn.Metadata); diff != "" {
		t.Errorf("metadata mismatch (-want +got):\n%s", diff)
	}

	var completed []types.CompletedPart
	for i, content := range []string{"part1 data", "part2 data"} {
		part, err := client.UploadPart(ctx, &s3.UploadPartInput{
			Bucket:     aws.String("testbucket"),
			Key:        aws.String("multi/part.bin"),
			UploadId:   aws.String(uploadID),
			PartNumber: aws.Int32(int32(i + 1)),
			Body:       strings.NewReader(content),
		})
		if err != nil {
			t.Fatal(err)
		}
		completed = append(completed, types.CompletedPart{
			ETag:       part.ETag,
			PartNumber: aws.Int32(int32(i + 1)),
		})
	}
	if len(stub.uploadPartIns) != 2 {
		t.Fatalf("expect 2 upload parts, got %d", len(stub.uploadPartIns))
	}
	if string(stub.uploadPartData[0]) != "part1 data" || string(stub.uploadPartData[1]) != "part2 data" {
		t.Errorf("part body mismatch: %q %q", stub.uploadPartData[0], stub.uploadPartData[1])
	}
	if aws.ToString(stub.uploadPartIns[0].UploadId) != "test-upload-id" {
		t.Errorf("unexpected upload id %v", stub.uploadPartIns[0].UploadId)
	}
	if aws.ToInt32(stub.uploadPartIns[1].PartNumber) != 2 {
		t.Errorf("unexpected part number %v", stub.uploadPartIns[1].PartNumber)
	}

	parts, err := client.ListParts(ctx, &s3.ListPartsInput{
		Bucket:   aws.String("testbucket"),
		Key:      aws.String("multi/part.bin"),
		UploadId: aws.String(uploadID),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts.Parts) != 2 {
		t.Fatalf("expect 2 parts, got %d", len(parts.Parts))
	}
	// the Bucket must be the front bucket name
	if aws.ToString(parts.Bucket) != "testbucket" {
		t.Errorf("expect testbucket, got %s", aws.ToString(parts.Bucket))
	}
	// Owner and Initiator are the tenant, never the backend account
	if parts.Owner == nil || aws.ToString(parts.Owner.ID) != "testtenant" {
		t.Errorf("expect testtenant owner, got %v", parts.Owner)
	}
	if parts.Initiator == nil || aws.ToString(parts.Initiator.ID) != "testtenant" {
		t.Errorf("expect testtenant initiator, got %v", parts.Initiator)
	}

	complete, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String("testbucket"),
		Key:             aws.String("multi/part.bin"),
		UploadId:        aws.String(uploadID),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(complete.ETag) != `"complete-etag"` {
		t.Errorf("unexpected etag %v", complete.ETag)
	}
	if aws.ToString(complete.Bucket) != "testbucket" {
		t.Errorf("expect testbucket, got %s", aws.ToString(complete.Bucket))
	}
	got := stub.completeMPUIn.MultipartUpload.Parts
	if len(got) != 2 || aws.ToString(got[1].ETag) != `"part-etag-2"` || aws.ToInt32(got[1].PartNumber) != 2 {
		t.Errorf("unexpected completed parts %v", got)
	}
}

func TestProxyAbortMultipartUpload(t *testing.T) {
	stub := &stubBackend{}
	client, _ := newTestProxy(t, stub)
	if _, err := client.AbortMultipartUpload(t.Context(), &s3.AbortMultipartUploadInput{
		Bucket:   aws.String("testbucket"),
		Key:      aws.String("multi/part.bin"),
		UploadId: aws.String("abort-upload-id"),
	}); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(stub.abortMPUIn.UploadId) != "abort-upload-id" {
		t.Errorf("unexpected upload id %v", stub.abortMPUIn.UploadId)
	}
	if aws.ToString(stub.abortMPUIn.Bucket) != "backend-testbucket" {
		t.Errorf("expect backend-testbucket, got %s", aws.ToString(stub.abortMPUIn.Bucket))
	}
}

func TestProxyListMultipartUploads(t *testing.T) {
	stub := &stubBackend{
		listMPUOut: &s3.ListMultipartUploadsOutput{
			IsTruncated: aws.Bool(false),
			MaxUploads:  aws.Int32(1000),
			Uploads: []types.MultipartUpload{
				{Key: aws.String("a.bin"), UploadId: aws.String("upload-a"), Initiated: aws.Time(time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC))},
			},
		},
	}
	client, _ := newTestProxy(t, stub)
	out, err := client.ListMultipartUploads(t.Context(), &s3.ListMultipartUploadsInput{
		Bucket: aws.String("testbucket"),
		Prefix: aws.String("a"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.Bucket) != "testbucket" {
		t.Errorf("expect testbucket, got %s", aws.ToString(out.Bucket))
	}
	if len(out.Uploads) != 1 || aws.ToString(out.Uploads[0].UploadId) != "upload-a" {
		t.Errorf("unexpected uploads %v", out.Uploads)
	}
	if aws.ToString(stub.listMPUIn.Prefix) != "a" {
		t.Errorf("unexpected prefix %v", stub.listMPUIn.Prefix)
	}
	// Owner and Initiator are the tenant, never the backend account
	if out.Uploads[0].Owner == nil || aws.ToString(out.Uploads[0].Owner.ID) != "testtenant" {
		t.Errorf("expect testtenant owner, got %v", out.Uploads[0].Owner)
	}
	if out.Uploads[0].Initiator == nil || aws.ToString(out.Uploads[0].Initiator.ID) != "testtenant" {
		t.Errorf("expect testtenant initiator, got %v", out.Uploads[0].Initiator)
	}
}

// TestProxyRawChunkedPut sends a hand-crafted signed aws-chunked PUT,
// as the aws CLI does over plain http endpoints.
func TestProxyRawChunkedPut(t *testing.T) {
	stub := &stubBackend{
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"chunked-etag"`)},
	}
	gw := newTestGateway(t)
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	tsURL := newTestServer(t, gw)

	data := []byte(strings.Repeat("streaming payload ", 1000))
	signTime := time.Now().UTC()

	// sign the request headers with the STREAMING payload hash first
	req, err := http.NewRequest(http.MethodPut, tsURL+"/testbucket/streamed.txt", nil)
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

	vr := &sigv4.Verified{
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
