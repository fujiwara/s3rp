package s3rp_test

import (
	"errors"
	"io"
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
		Buckets: []*s3rp.BucketConfig{
			{
				Name: "it-bucket",
				Backend: &s3rp.BackendConfig{
					Endpoint:        endpoint,
					Bucket:          backendBucket,
					AccessKeyID:     backendKey,
					SecretAccessKey: s3rp.Password(backendSecret),
				},
				Keys: []*s3rp.KeyConfig{
					{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey},
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
