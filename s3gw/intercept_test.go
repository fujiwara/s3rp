package s3gw_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3gw"
)

// These cover the two things a service needs from the hooks: being able to
// refuse an operation the policies would have allowed, and being able to
// account for what was actually served.

type blocker struct {
	err  error
	seen []*s3gw.Op
}

func (b *blocker) Authorize(_ context.Context, op *s3gw.Op) error {
	b.seen = append(b.seen, op)
	return b.err
}

func TestAuthorizerCanRefuse(t *testing.T) {
	stub := &stubBackend{getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("x"))}}
	client, _, app := newTestProxyWithGateway(t, stub)

	// the quota this service enforces is not expressible as a policy
	// a retryable status would have the client send the request again, and
	// each attempt is hooked on its own; 403 keeps this to a single call
	block := &blocker{err: s3err.New(http.StatusForbidden, "QuotaExceeded", "Quota exceeded")}
	app.SetAuthorizer(block)

	_, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
	})
	if err == nil {
		t.Fatal("expect the authorizer to refuse the request")
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "QuotaExceeded" {
		t.Errorf("expect the authorizer's error to reach the client, got %v", err)
	}
	if len(block.seen) != 1 {
		t.Fatalf("expect one authorization, got %d", len(block.seen))
	}
	op := block.seen[0]
	if !slices.Equal(op.Actions, []string{"s3:GetObject"}) || op.Bucket != "testbucket" || op.Key != "a.txt" {
		t.Errorf("unexpected op %+v", op)
	}
	if op.Tenant == "" || op.User == "" {
		t.Errorf("expect the identity on the op, got %+v", op)
	}
	// the refusal must have stopped the operation
	if stub.getIn != nil {
		t.Error("a refused operation must not reach the backend")
	}
}

func TestInterceptorMeters(t *testing.T) {
	const body = "hello metering"
	stub := &stubBackend{
		getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(body))},
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
	}
	client, _, app := newTestProxyWithGateway(t, stub)

	var recorded []s3gw.Op
	app.Use(func(ctx context.Context, op *s3gw.Op, next func() error) error {
		err := next()
		recorded = append(recorded, *op) // byte counts are filled in by now
		return err
	})

	out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, out.Body)
	out.Body.Close()

	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("b.txt"),
		Body: strings.NewReader("uploaded"),
	}); err != nil {
		t.Fatal(err)
	}

	if len(recorded) != 2 {
		t.Fatalf("expect two operations recorded, got %d", len(recorded))
	}
	get, put := recorded[0], recorded[1]
	if get.Method != "GET" || !slices.Equal(get.Actions, []string{"s3:GetObject"}) {
		t.Errorf("unexpected get op %+v", get)
	}
	if get.BytesOut != int64(len(body)) {
		t.Errorf("expect %d bytes served, got %d", len(body), get.BytesOut)
	}
	if !slices.Equal(put.Actions, []string{"s3:PutObject"}) || put.BytesIn == 0 {
		t.Errorf("expect the uploaded bytes to be counted, got %+v", put)
	}
}

func TestInterceptorCanRefuseWithoutRunning(t *testing.T) {
	stub := &stubBackend{getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("x"))}}
	client, _, app := newTestProxyWithGateway(t, stub)
	app.Use(func(ctx context.Context, op *s3gw.Op, next func() error) error {
		return s3err.New(http.StatusForbidden, "AccessDenied", "draining")
	})
	if _, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
	}); err == nil {
		t.Fatal("expect the interceptor to refuse the request")
	}
	if stub.getIn != nil {
		t.Error("not calling next must not reach the backend")
	}
}
