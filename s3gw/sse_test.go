package s3gw_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp/s3gw"
)

// TestSSEHeaderValidation covers what the SDK cannot be made to send:
// malformed SSE values on the wire.
func TestSSEHeaderValidation(t *testing.T) {
	cases := []struct {
		name    string
		method  string
		headers map[string]string
		code    string
	}{
		{
			name:    "unsupported encryption method",
			method:  "PUT",
			headers: map[string]string{"x-amz-server-side-encryption": "aws:kms:dsse"},
			code:    "InvalidArgument",
		},
		{
			name:    "kms key id without aws:kms",
			method:  "PUT",
			headers: map[string]string{"x-amz-server-side-encryption-aws-kms-key-id": "some-key"},
			code:    "InvalidArgument",
		},
		{
			name:   "kms key id with AES256",
			method: "PUT",
			headers: map[string]string{
				"x-amz-server-side-encryption":                "AES256",
				"x-amz-server-side-encryption-aws-kms-key-id": "some-key",
			},
			code: "InvalidArgument",
		},
		{
			// the check runs in dispatch, so a bogus value is refused on
			// any operation, not only uploads
			name:    "unsupported method on GET",
			method:  "GET",
			headers: map[string]string{"x-amz-server-side-encryption": "rot13"},
			code:    "InvalidArgument",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gw := newTestGateway(t)
			stub := &stubPost{}
			if err := gw.SetBackend("testbucket", stub); err != nil {
				t.Fatal(err)
			}
			// the refusal happens before the hooks, so an Authorizer or
			// Interceptor never sees an unsupported value on Op
			var ops []*s3gw.Op
			gw.Use(func(ctx context.Context, op *s3gw.Op, next func() error) error {
				ops = append(ops, op)
				return next()
			})
			var body []byte
			if tc.method == "PUT" {
				body = []byte("x")
			}
			req := signedRequest(t, tc.method, "http://s3.example.com/testbucket/a.txt",
				body, "UNSIGNED-PAYLOAD", time.Now(), testCreds(), func(r *http.Request) {
					for k, v := range tc.headers {
						r.Header.Set(k, v)
					}
				})
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, req)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), tc.code) {
				t.Errorf("expect %s, got %d: %s", tc.code, w.Code, w.Body.String())
			}
			if stub.putIn != nil {
				t.Error("a refused request must not reach the backend")
			}
			if len(ops) != 0 {
				t.Errorf("the hooks must not see an unsupported SSE value, got %+v", ops)
			}
		})
	}
}

// TestPostObjectSSE checks the POST form path forwards the SSE fields.
func TestPostObjectSSE(t *testing.T) {
	gw := newTestGateway(t)
	stub := &stubPost{
		putOut: &s3.PutObjectOutput{
			ETag:                 aws.String(`"post-etag"`),
			ServerSideEncryption: types.ServerSideEncryptionAwsKms,
			SSEKMSKeyId:          aws.String("tenant-key-1"),
		},
	}
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	form := &postForm{
		conditions: []string{
			`{"key": "a.txt"}`,
			`{"x-amz-server-side-encryption": "aws:kms"}`,
			`{"x-amz-server-side-encryption-aws-kms-key-id": "tenant-key-1"}`,
		},
		fields: [][2]string{
			{"key", "a.txt"},
			{"x-amz-server-side-encryption", "aws:kms"},
			{"x-amz-server-side-encryption-aws-kms-key-id", "tenant-key-1"},
		},
		filename: "x", content: "secret",
	}
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, form.request(t))
	if w.Code != http.StatusNoContent {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}
	if stub.putIn.ServerSideEncryption != types.ServerSideEncryptionAwsKms ||
		aws.ToString(stub.putIn.SSEKMSKeyId) != "tenant-key-1" {
		t.Errorf("expect the SSE fields at the backend, got %q %v",
			stub.putIn.ServerSideEncryption, stub.putIn.SSEKMSKeyId)
	}
	if got := w.Header().Get("x-amz-server-side-encryption"); got != "aws:kms" {
		t.Errorf("expect the encryption result header, got %q", got)
	}
	if got := w.Header().Get("x-amz-server-side-encryption-aws-kms-key-id"); got != "tenant-key-1" {
		t.Errorf("expect the key id header, got %q", got)
	}
}

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
	client, _, app := newTestProxyWithGateway(t, stub)
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
	if ops[0].Request == nil || ops[0].Request.SSE != "aws:kms" || ops[0].Request.SSEKMSKeyID != "tenant-key-1" {
		t.Errorf("expect the encryption request on the put op, got %+v", ops[0].Request)
	}
	// a request that asked for nothing about the object has no Request at
	// all, so a hook never reads a zero value as an answer
	if ops[1].Request != nil {
		t.Errorf("expect no encryption request on the get op, got %+v", ops[1].Request)
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
