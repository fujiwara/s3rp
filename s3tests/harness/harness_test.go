package harness_test

import (
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/fujiwara/s3rp/s3tests/harness"
)

func TestNewRequiresBackendConfig(t *testing.T) {
	if _, err := harness.New(t.Context(), harness.Config{}); err == nil {
		t.Error("expected an error for a missing endpoint")
	}
	if _, err := harness.New(t.Context(), harness.Config{
		BackendEndpoint: "http://127.0.0.1:7480",
	}); err == nil {
		t.Error("expected an error for missing credentials")
	}
}

// With InProcess the harness needs no backend: buckets are created on,
// and served from, the in-memory backend.
func TestInProcess(t *testing.T) {
	h, err := harness.New(t.Context(), harness.Config{InProcess: true, LogWriter: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	client := sdkClient(t, ts.URL, harness.MainAccessKeyID, harness.MainSecretAccessKey)
	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("inproc")}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("inproc"), Key: aws.String("k"), Body: strings.NewReader("v"),
	}); err != nil {
		t.Fatal(err)
	}
	out, err := client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("inproc"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	if b, _ := io.ReadAll(out.Body); string(b) != "v" {
		t.Errorf("body = %q", b)
	}
	if _, err := client.DeleteBucket(t.Context(), &s3.DeleteBucketInput{Bucket: aws.String("inproc")}); errorCode(t, err) != "BucketNotEmpty" {
		t.Errorf("deleting a bucket holding an object: %v", err)
	}
}

// With InProcessDir the buckets and objects are there again for the next
// harness on the same directory, once this one was closed.
func TestInProcessDirectory(t *testing.T) {
	dir := t.TempDir()
	serve := func() (*s3.Client, harness.Handler) {
		h, err := harness.New(t.Context(), harness.Config{InProcess: true, InProcessDir: dir, LogWriter: io.Discard})
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(h)
		t.Cleanup(ts.Close)
		return sdkClient(t, ts.URL, harness.MainAccessKeyID, harness.MainSecretAccessKey), h
	}
	client, h := serve()
	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("kept")}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("kept"), Key: aws.String("k"), Body: strings.NewReader("v"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}

	// the harness's own bucket registry is in memory, so the bucket is
	// created anew; the backend still holding it is not an error, and the
	// object is there
	client, h = serve()
	defer h.Close()
	if _, err := client.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("kept")}); err != nil {
		t.Fatal(err)
	}
	out, err := client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("kept"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	if b, _ := io.ReadAll(out.Body); string(b) != "v" {
		t.Errorf("body after reopening = %q", b)
	}
}
