package s3gw_test

import (
	"crypto/md5"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// A present Content-MD5 must be a valid base64 MD5 digest: S3 answers
// InvalidDigest otherwise, and an empty header (indistinguishable from an
// absent one via Header.Get) must not silently vanish.
func TestPutObjectContentMD5(t *testing.T) {
	t.Run("valid digest forwarded", func(t *testing.T) {
		stub := &stubBackend{putOut: &s3.PutObjectOutput{}}
		client, _ := newTestProxy(t, stub)
		sum := md5.Sum([]byte("body"))
		digest := base64.StdEncoding.EncodeToString(sum[:])
		if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket:     aws.String("testbucket"),
			Key:        aws.String("k"),
			Body:       strings.NewReader("body"),
			ContentMD5: aws.String(digest),
		}); err != nil {
			t.Fatal(err)
		}
		if got := aws.ToString(stub.putIn.ContentMD5); got != digest {
			t.Errorf("Content-MD5 not forwarded: %q", got)
		}
	})

	t.Run("garbage refused", func(t *testing.T) {
		stub := &stubBackend{putOut: &s3.PutObjectOutput{}}
		client, _ := newTestProxy(t, stub)
		_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket:     aws.String("testbucket"),
			Key:        aws.String("k"),
			Body:       strings.NewReader("body"),
			ContentMD5: aws.String("not base64!"),
		})
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InvalidDigest" {
			t.Errorf("expected InvalidDigest, got %v", err)
		}
		if stub.putIn != nil {
			t.Error("an invalid digest must not reach the backend")
		}
	})

	t.Run("empty header refused", func(t *testing.T) {
		stub := &stubBackend{putOut: &s3.PutObjectOutput{}}
		client, _ := newTestProxy(t, stub)
		presigner := s3.NewPresignClient(client)
		req, err := presigner.PresignPutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("testbucket"),
			Key:    aws.String("k"),
		})
		if err != nil {
			t.Fatal(err)
		}
		httpReq, err := http.NewRequestWithContext(t.Context(), http.MethodPut, req.URL, strings.NewReader("body"))
		if err != nil {
			t.Fatal(err)
		}
		httpReq.Header.Set("Content-MD5", "")
		resp, err := http.DefaultClient.Do(httpReq)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 for an empty Content-MD5, got %d", resp.StatusCode)
		}
		if stub.putIn != nil {
			t.Error("an empty digest must not reach the backend")
		}
	})
}
