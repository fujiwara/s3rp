package s3rp_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp/s3gw"
)

// SSE-S3/SSE-KMS pass through: the backend encrypts, the gateway forwards
// the request fields and reports the backend's result. SSE-C is refused
// loudly — silently dropping the customer key would store the object
// without the encryption the client believes it requested.

func TestProxySSEKMSPassThrough(t *testing.T) {
	stub := &stubBackend{
		putOut: &s3.PutObjectOutput{
			ETag:                 aws.String(`"e"`),
			ServerSideEncryption: types.ServerSideEncryptionAwsKms,
			SSEKMSKeyId:          aws.String("tenant-key-1"),
		},
		getOut: &s3.GetObjectOutput{
			Body:                 io.NopCloser(strings.NewReader("secret")),
			ServerSideEncryption: types.ServerSideEncryptionAwsKms,
			SSEKMSKeyId:          aws.String("tenant-key-1"),
		},
	}
	client, _, app := newTestProxyWithApp(t, stub)
	var ops []*s3gw.Op
	app.Use(func(ctx context.Context, op *s3gw.Op, next func() error) error {
		ops = append(ops, op)
		return next()
	})

	put, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket:               aws.String("testbucket"),
		Key:                  aws.String("enc.txt"),
		Body:                 strings.NewReader("secret"),
		ServerSideEncryption: types.ServerSideEncryptionAwsKms,
		SSEKMSKeyId:          aws.String("tenant-key-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	// the request fields reached the backend
	if stub.putIn.ServerSideEncryption != types.ServerSideEncryptionAwsKms {
		t.Errorf("expect aws:kms at the backend, got %q", stub.putIn.ServerSideEncryption)
	}
	if aws.ToString(stub.putIn.SSEKMSKeyId) != "tenant-key-1" {
		t.Errorf("expect the key id at the backend, got %v", stub.putIn.SSEKMSKeyId)
	}
	// and the backend's result reached the client
	if put.ServerSideEncryption != types.ServerSideEncryptionAwsKms || aws.ToString(put.SSEKMSKeyId) != "tenant-key-1" {
		t.Errorf("expect the encryption result on the response, got %q %v", put.ServerSideEncryption, put.SSEKMSKeyId)
	}

	get, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("enc.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, get.Body)
	get.Body.Close()
	if get.ServerSideEncryption != types.ServerSideEncryptionAwsKms || aws.ToString(get.SSEKMSKeyId) != "tenant-key-1" {
		t.Errorf("expect the encryption state on the download, got %q %v", get.ServerSideEncryption, get.SSEKMSKeyId)
	}

	// the key id is on the Op, so an Authorizer can decide whether this
	// tenant may use this key
	if len(ops) != 2 {
		t.Fatalf("expect two ops, got %d", len(ops))
	}
	if ops[0].SSEKMSKeyID != "tenant-key-1" {
		t.Errorf("expect the key id on the put op, got %q", ops[0].SSEKMSKeyID)
	}
	if ops[1].SSEKMSKeyID != "" {
		t.Errorf("expect no key id on the get op, got %q", ops[1].SSEKMSKeyID)
	}
}

func TestProxySSECRefused(t *testing.T) {
	stub := &stubBackend{
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
		getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("x"))},
	}
	client, _ := newTestProxy(t, stub)

	sseKey := strings.Repeat("k", 32)
	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket:               aws.String("testbucket"),
		Key:                  aws.String("c.txt"),
		Body:                 strings.NewReader("x"),
		SSECustomerAlgorithm: aws.String("AES256"),
		SSECustomerKey:       aws.String(sseKey),
	})
	assertCode := func(err error, code string) {
		t.Helper()
		var apiErr smithy.APIError
		if err == nil || !errors.As(err, &apiErr) || apiErr.ErrorCode() != code {
			t.Errorf("expect %s, got %v", code, err)
		}
	}
	assertCode(err, "NotImplemented")
	if stub.putIn != nil {
		t.Error("a refused SSE-C upload must not reach the backend")
	}

	_, err = client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket:               aws.String("testbucket"),
		Key:                  aws.String("c.txt"),
		SSECustomerAlgorithm: aws.String("AES256"),
		SSECustomerKey:       aws.String(sseKey),
	})
	assertCode(err, "NotImplemented")
	if stub.getIn != nil {
		t.Error("a refused SSE-C download must not reach the backend")
	}
}
