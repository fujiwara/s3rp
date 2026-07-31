package s3rp_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp"
)

// Integration test against a real S3-compatible backend (versitygw).
// Run: docker compose up -d && S3RP_TEST_BACKEND_ENDPOINT=http://localhost:7070 go test ./...
func TestIntegration(t *testing.T) {
	endpoint := os.Getenv("S3RP_TEST_BACKEND_ENDPOINT")
	if endpoint == "" {
		t.Skip("S3RP_TEST_BACKEND_ENDPOINT is not set")
	}
	backendKey := envOr("S3RP_TEST_BACKEND_ACCESS_KEY_ID", "backendkey")
	backendSecret := envOr("S3RP_TEST_BACKEND_SECRET_ACCESS_KEY", "backendsecret")
	const backendBucket = "s3rp-it-backend"

	// create the backend bucket directly (s3rp does not proxy CreateBucket)
	backendClient := newS3Client(t, endpoint, backendKey, backendSecret)
	_, err := backendClient.CreateBucket(t.Context(), &s3.CreateBucketInput{
		Bucket: aws.String(backendBucket),
	})
	if err != nil {
		var exists *types.BucketAlreadyOwnedByYou
		if !errors.As(err, &exists) {
			t.Fatalf("failed to create backend bucket: %v", err)
		}
	}

	cfg := &s3rp.Config{
		Tenants: []*s3rp.TenantConfig{
			{
				Name: "it-tenant",
				Keys: []*s3rp.KeyConfig{
					{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey},
				},
				Buckets: []*s3rp.BucketConfig{
					{
						Name: "it-bucket",
						Backend: &s3rp.BackendConfig{
							Endpoint:        endpoint,
							Bucket:          backendBucket,
							AccessKeyID:     backendKey,
							SecretAccessKey: s3rp.Password(backendSecret),
						},
					},
				},
			},
		},
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := s3rp.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)
	client := newS3Client(t, ts.URL, testAccessKeyID, testSecretAccessKey)

	content := strings.Repeat("s3rp integration test payload ", 10000) // ~300KB
	t.Run("PutObject", func(t *testing.T) {
		// the SDK default checksum settings exercise the aws-chunked path
		out, err := client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket:      aws.String("it-bucket"),
			Key:         aws.String("dir/test.txt"),
			Body:        strings.NewReader(content),
			ContentType: aws.String("text/plain"),
			Metadata:    map[string]string{"origin": "s3rp-integration"},
		})
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToString(out.ETag) == "" {
			t.Error("expect ETag")
		}
	})
	t.Run("GetObject", func(t *testing.T) {
		out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/test.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer out.Body.Close()
		body, err := io.ReadAll(out.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != content {
			t.Errorf("content mismatch: got %d bytes, want %d", len(body), len(content))
		}
		if got := out.Metadata["origin"]; got != "s3rp-integration" {
			t.Errorf("metadata mismatch: %v", out.Metadata)
		}
	})
	t.Run("HeadObject", func(t *testing.T) {
		out, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/test.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToInt64(out.ContentLength) != int64(len(content)) {
			t.Errorf("unexpected content length %v", out.ContentLength)
		}
	})
	t.Run("RangeGet", func(t *testing.T) {
		out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/test.txt"),
			Range:  aws.String("bytes=0-9"),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer out.Body.Close()
		body, _ := io.ReadAll(out.Body)
		if string(body) != content[:10] {
			t.Errorf("unexpected range body %q", body)
		}
	})
	t.Run("ListObjectsV2", func(t *testing.T) {
		out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
			Bucket: aws.String("it-bucket"),
			Prefix: aws.String("dir/"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToString(out.Name) != "it-bucket" {
			t.Errorf("expect it-bucket, got %s", aws.ToString(out.Name))
		}
		found := false
		for _, obj := range out.Contents {
			if aws.ToString(obj.Key) == "dir/test.txt" {
				found = true
			}
		}
		if !found {
			t.Errorf("dir/test.txt not found in %v", out.Contents)
		}
	})
	t.Run("HeadBucket", func(t *testing.T) {
		if _, err := client.HeadBucket(t.Context(), &s3.HeadBucketInput{
			Bucket: aws.String("it-bucket"),
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("ListBuckets", func(t *testing.T) {
		out, err := client.ListBuckets(t.Context(), &s3.ListBucketsInput{})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Buckets) != 1 || aws.ToString(out.Buckets[0].Name) != "it-bucket" {
			t.Errorf("unexpected buckets %v", out.Buckets)
		}
	})
	t.Run("UploadPartCopy", func(t *testing.T) {
		// source object of at least 5MiB for a non-last copied part
		src := strings.Repeat("s", 5*1024*1024)
		if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/upc-src.bin"),
			Body:   strings.NewReader(src),
		}); err != nil {
			t.Fatal(err)
		}
		create, err := client.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/upc-dst.bin"),
		})
		if err != nil {
			t.Fatal(err)
		}
		part1, err := client.UploadPartCopy(t.Context(), &s3.UploadPartCopyInput{
			Bucket:     aws.String("it-bucket"),
			Key:        aws.String("dir/upc-dst.bin"),
			UploadId:   create.UploadId,
			PartNumber: aws.Int32(1),
			CopySource: aws.String("it-bucket/dir/upc-src.bin"),
		})
		if err != nil {
			t.Fatal(err)
		}
		part2, err := client.UploadPart(t.Context(), &s3.UploadPartInput{
			Bucket:     aws.String("it-bucket"),
			Key:        aws.String("dir/upc-dst.bin"),
			UploadId:   create.UploadId,
			PartNumber: aws.Int32(2),
			Body:       strings.NewReader("tail"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.CompleteMultipartUpload(t.Context(), &s3.CompleteMultipartUploadInput{
			Bucket:   aws.String("it-bucket"),
			Key:      aws.String("dir/upc-dst.bin"),
			UploadId: create.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
				{ETag: part1.CopyPartResult.ETag, PartNumber: aws.Int32(1)},
				{ETag: part2.ETag, PartNumber: aws.Int32(2)},
			}},
		}); err != nil {
			t.Fatal(err)
		}
		head, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/upc-dst.bin"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToInt64(head.ContentLength) != int64(len(src)+4) {
			t.Errorf("unexpected size %v", head.ContentLength)
		}
		if _, err := client.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
			Bucket: aws.String("it-bucket"),
			Delete: &types.Delete{Objects: []types.ObjectIdentifier{
				{Key: aws.String("dir/upc-src.bin")},
				{Key: aws.String("dir/upc-dst.bin")},
			}},
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("MultipartUpload", func(t *testing.T) {
		// non-last parts must be at least 5MiB
		part1 := strings.Repeat("p", 5*1024*1024)
		part2 := "tail part"
		create, err := client.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{
			Bucket:      aws.String("it-bucket"),
			Key:         aws.String("dir/multipart.bin"),
			ContentType: aws.String("application/octet-stream"),
		})
		if err != nil {
			t.Fatal(err)
		}
		uploadID := create.UploadId
		var completed []types.CompletedPart
		for i, content := range []string{part1, part2} {
			part, err := client.UploadPart(t.Context(), &s3.UploadPartInput{
				Bucket:     aws.String("it-bucket"),
				Key:        aws.String("dir/multipart.bin"),
				UploadId:   uploadID,
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
		parts, err := client.ListParts(t.Context(), &s3.ListPartsInput{
			Bucket:   aws.String("it-bucket"),
			Key:      aws.String("dir/multipart.bin"),
			UploadId: uploadID,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(parts.Parts) != 2 {
			t.Errorf("expect 2 parts, got %d", len(parts.Parts))
		}
		if _, err := client.CompleteMultipartUpload(t.Context(), &s3.CompleteMultipartUploadInput{
			Bucket:          aws.String("it-bucket"),
			Key:             aws.String("dir/multipart.bin"),
			UploadId:        uploadID,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: completed},
		}); err != nil {
			t.Fatal(err)
		}
		out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/multipart.bin"),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer out.Body.Close()
		body, err := io.ReadAll(out.Body)
		if err != nil {
			t.Fatal(err)
		}
		if string(body) != part1+part2 {
			t.Errorf("content mismatch: got %d bytes, want %d", len(body), len(part1)+len(part2))
		}
		if _, err := client.DeleteObject(t.Context(), &s3.DeleteObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/multipart.bin"),
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("ListObjectsV1", func(t *testing.T) {
		out, err := client.ListObjects(t.Context(), &s3.ListObjectsInput{
			Bucket: aws.String("it-bucket"),
			Prefix: aws.String("dir/"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToString(out.Name) != "it-bucket" {
			t.Errorf("expect it-bucket, got %s", aws.ToString(out.Name))
		}
		if len(out.Contents) == 0 {
			t.Error("expect some contents")
		}
	})
	t.Run("GetBucketLocation", func(t *testing.T) {
		out, err := client.GetBucketLocation(t.Context(), &s3.GetBucketLocationInput{
			Bucket: aws.String("it-bucket"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.LocationConstraint != "" { // us-east-1 is empty
			t.Errorf("unexpected location %v", out.LocationConstraint)
		}
	})
	t.Run("CopyObject", func(t *testing.T) {
		out, err := client.CopyObject(t.Context(), &s3.CopyObjectInput{
			Bucket:     aws.String("it-bucket"),
			Key:        aws.String("dir/copied.txt"),
			CopySource: aws.String("it-bucket/dir/test.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToString(out.CopyObjectResult.ETag) == "" {
			t.Error("expect ETag in CopyObjectResult")
		}
		got, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/copied.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer got.Body.Close()
		body, _ := io.ReadAll(got.Body)
		if string(body) != content {
			t.Errorf("copied content mismatch: %d bytes", len(body))
		}
	})
	t.Run("DeleteObjects", func(t *testing.T) {
		for _, key := range []string{"del/1.txt", "del/2.txt"} {
			if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
				Bucket: aws.String("it-bucket"),
				Key:    aws.String(key),
				Body:   strings.NewReader("to be deleted"),
			}); err != nil {
				t.Fatal(err)
			}
		}
		out, err := client.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
			Bucket: aws.String("it-bucket"),
			Delete: &types.Delete{
				Objects: []types.ObjectIdentifier{
					{Key: aws.String("del/1.txt")},
					{Key: aws.String("del/2.txt")},
					{Key: aws.String("dir/copied.txt")},
				},
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Deleted) != 3 {
			t.Errorf("expect 3 deleted, got %v", out.Deleted)
		}
		list, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
			Bucket: aws.String("it-bucket"),
			Prefix: aws.String("del/"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(list.Contents) != 0 {
			t.Errorf("expect no del/ objects, got %v", list.Contents)
		}
	})
	t.Run("ObjectTagging", func(t *testing.T) {
		if _, err := client.PutObjectTagging(t.Context(), &s3.PutObjectTaggingInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/test.txt"),
			Tagging: &types.Tagging{TagSet: []types.Tag{
				{Key: aws.String("env"), Value: aws.String("integration")},
			}},
		}); err != nil {
			t.Fatal(err)
		}
		got, err := client.GetObjectTagging(t.Context(), &s3.GetObjectTaggingInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/test.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.TagSet) != 1 || aws.ToString(got.TagSet[0].Value) != "integration" {
			t.Errorf("unexpected tag set %v", got.TagSet)
		}
		if _, err := client.DeleteObjectTagging(t.Context(), &s3.DeleteObjectTaggingInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/test.txt"),
		}); err != nil {
			t.Fatal(err)
		}
		got, err = client.GetObjectTagging(t.Context(), &s3.GetObjectTaggingInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/test.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.TagSet) != 0 {
			t.Errorf("expect empty tag set after delete, got %v", got.TagSet)
		}
	})
	t.Run("Versioning", func(t *testing.T) {
		// requires versitygw started with --versioning-dir
		if _, err := client.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{
			Bucket: aws.String("it-bucket"),
			VersioningConfiguration: &types.VersioningConfiguration{
				Status: types.BucketVersioningStatusEnabled,
			},
		}); err != nil {
			t.Fatal(err)
		}
		ver, err := client.GetBucketVersioning(t.Context(), &s3.GetBucketVersioningInput{
			Bucket: aws.String("it-bucket"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if ver.Status != types.BucketVersioningStatusEnabled {
			t.Fatalf("expect Enabled, got %v", ver.Status)
		}
		// two versions of the same key
		for _, content := range []string{"version one", "version two"} {
			if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
				Bucket: aws.String("it-bucket"),
				Key:    aws.String("ver/key.txt"),
				Body:   strings.NewReader(content),
			}); err != nil {
				t.Fatal(err)
			}
		}
		versions, err := client.ListObjectVersions(t.Context(), &s3.ListObjectVersionsInput{
			Bucket: aws.String("it-bucket"),
			Prefix: aws.String("ver/"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(versions.Versions) != 2 {
			t.Fatalf("expect 2 versions, got %d", len(versions.Versions))
		}
		var oldVersionID string
		for _, v := range versions.Versions {
			if !aws.ToBool(v.IsLatest) {
				oldVersionID = aws.ToString(v.VersionId)
			}
		}
		if oldVersionID == "" {
			t.Fatal("old version id not found")
		}
		got, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket:    aws.String("it-bucket"),
			Key:       aws.String("ver/key.txt"),
			VersionId: aws.String(oldVersionID),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer got.Body.Close()
		body, _ := io.ReadAll(got.Body)
		if string(body) != "version one" {
			t.Errorf("expect old version content, got %q", body)
		}
		// clean up all versions
		versions, err = client.ListObjectVersions(t.Context(), &s3.ListObjectVersionsInput{
			Bucket: aws.String("it-bucket"),
			Prefix: aws.String("ver/"),
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range versions.Versions {
			if _, err := client.DeleteObject(t.Context(), &s3.DeleteObjectInput{
				Bucket:    aws.String("it-bucket"),
				Key:       v.Key,
				VersionId: v.VersionId,
			}); err != nil {
				t.Fatal(err)
			}
		}
		// suspend versioning to avoid affecting other subtests
		if _, err := client.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{
			Bucket: aws.String("it-bucket"),
			VersioningConfiguration: &types.VersioningConfiguration{
				Status: types.BucketVersioningStatusSuspended,
			},
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("PresignedURL", func(t *testing.T) {
		presigner := s3.NewPresignClient(client)
		content := "presigned integration content"
		put, err := presigner.PresignPutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/presigned.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequestWithContext(t.Context(), put.Method, put.URL, strings.NewReader(content))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("presigned PUT: expect 200, got %d", resp.StatusCode)
		}
		get, err := presigner.PresignGetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/presigned.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		resp, err = http.Get(get.URL)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("presigned GET: expect 200, got %d: %s", resp.StatusCode, body)
		}
		if string(body) != content {
			t.Errorf("content mismatch: %q", body)
		}
		if _, err := client.DeleteObject(t.Context(), &s3.DeleteObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/presigned.txt"),
		}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("AbortMultipartUpload", func(t *testing.T) {
		create, err := client.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/aborted.bin"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := client.AbortMultipartUpload(t.Context(), &s3.AbortMultipartUploadInput{
			Bucket:   aws.String("it-bucket"),
			Key:      aws.String("dir/aborted.bin"),
			UploadId: create.UploadId,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := client.ListParts(t.Context(), &s3.ListPartsInput{
			Bucket:   aws.String("it-bucket"),
			Key:      aws.String("dir/aborted.bin"),
			UploadId: create.UploadId,
		}); err == nil {
			t.Error("expect error listing parts of an aborted upload")
		}
	})
	t.Run("DeleteObject", func(t *testing.T) {
		if _, err := client.DeleteObject(t.Context(), &s3.DeleteObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/test.txt"),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("it-bucket"),
			Key:    aws.String("dir/test.txt"),
		})
		if err == nil {
			t.Error("expect NoSuchKey after delete")
		}
	})
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func newS3Client(t *testing.T, endpoint, key, secret string) *s3.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(key, secret, ""),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
}
