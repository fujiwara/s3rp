package s3gw_test

import (
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// crc32("123456789") = 0xCBF43926, base64 of big-endian bytes
const crc32Of123456789 = "y/Q5Jg=="

func TestProxyPutObjectChecksumHeader(t *testing.T) {
	stub := &stubBackend{
		putOut: &s3.PutObjectOutput{
			ETag:          aws.String(`"e"`),
			ChecksumCRC32: aws.String(crc32Of123456789),
		},
	}
	client, _ := newTestProxy(t, stub)
	out, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket:        aws.String("testbucket"),
		Key:           aws.String("sum.txt"),
		Body:          strings.NewReader("123456789"),
		ChecksumCRC32: aws.String(crc32Of123456789),
	})
	if err != nil {
		t.Fatal(err)
	}
	// the precomputed checksum header passes through to the backend
	if got := aws.ToString(stub.putIn.ChecksumCRC32); got != crc32Of123456789 {
		t.Errorf("expect checksum passthrough, got %q", got)
	}
	// and the backend's checksum comes back to the client
	if got := aws.ToString(out.ChecksumCRC32); got != crc32Of123456789 {
		t.Errorf("expect checksum in response, got %q", got)
	}
}

// TestProxyPutObjectTrailerChecksum exercises the SDK default settings:
// the client sends an aws-chunked trailer checksum, the proxy verifies it
// and asks the backend to recompute via ChecksumAlgorithm.
func TestProxyPutObjectTrailerChecksum(t *testing.T) {
	stub := &stubBackend{
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
	}
	// the SDK's own checksum settings compute a trailer checksum
	client := newTestProxyDefaultChecksums(t, stub)
	content := strings.Repeat("trailer checksum content ", 100)
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("trailer.txt"),
		Body:   strings.NewReader(content),
	}); err != nil {
		t.Fatal(err)
	}
	if string(stub.putBody) != content {
		t.Errorf("body mismatch: got %d bytes", len(stub.putBody))
	}
	// the trailer algorithm must be forwarded so the backend stores a checksum
	if stub.putIn.ChecksumAlgorithm == "" && stub.putIn.ChecksumCRC32 == nil && stub.putIn.ChecksumCRC64NVME == nil {
		t.Errorf("expect a checksum algorithm or value on the backend input, got %+v", stub.putIn.ChecksumAlgorithm)
	}
}

func TestProxyGetObjectChecksum(t *testing.T) {
	t.Run("valid checksum validated by client", func(t *testing.T) {
		stub := &stubBackend{
			getOut: &s3.GetObjectOutput{
				Body:          io.NopCloser(strings.NewReader("123456789")),
				ContentLength: aws.Int64(9),
				ChecksumCRC32: aws.String(crc32Of123456789),
				ChecksumType:  types.ChecksumTypeFullObject,
			},
		}
		client, _ := newTestProxy(t, stub)
		out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("testbucket"),
			Key:    aws.String("sum.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer out.Body.Close()
		// reading the body triggers the client SDK's checksum validation
		body, err := io.ReadAll(out.Body)
		if err != nil {
			t.Fatalf("client-side checksum validation failed: %v", err)
		}
		if string(body) != "123456789" {
			t.Errorf("unexpected body %q", body)
		}
		if aws.ToString(out.ChecksumCRC32) != crc32Of123456789 {
			t.Errorf("expect checksum header, got %v", out.ChecksumCRC32)
		}
		// the client SDK (WhenSupported validation) asks for checksums
		if stub.getIn.ChecksumMode != types.ChecksumModeEnabled {
			t.Errorf("expect ChecksumMode ENABLED on backend input, got %q", stub.getIn.ChecksumMode)
		}
	})
	t.Run("corrupted checksum rejected by client", func(t *testing.T) {
		stub := &stubBackend{
			getOut: &s3.GetObjectOutput{
				Body:          io.NopCloser(strings.NewReader("123456789")),
				ContentLength: aws.Int64(9),
				ChecksumCRC32: aws.String("AAAAAA=="),
			},
		}
		client, _ := newTestProxy(t, stub)
		out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("testbucket"),
			Key:    aws.String("sum.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer out.Body.Close()
		if _, err := io.ReadAll(out.Body); err == nil {
			t.Error("expect client-side checksum validation to fail")
		}
	})
}
