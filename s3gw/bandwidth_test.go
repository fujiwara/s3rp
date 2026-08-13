package s3gw_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/s3gw"
	"golang.org/x/time/rate"
)

// recordingLimiter counts what it was asked to pass, and can fail.
type recordingLimiter struct {
	n     atomic.Int64
	calls atomic.Int64
	err   error
}

func (l *recordingLimiter) WaitN(ctx context.Context, n int) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	l.n.Add(int64(n))
	l.calls.Add(1)
	return l.err
}

// The limiters see exactly the bytes the hooks account: what the in limiter
// passed is Op.BytesIn (the wire form, aws-chunked framing included), what
// the out limiter passed is Op.BytesOut.
func TestBandwidthLimitSeesTheAccountedBytes(t *testing.T) {
	const body = "hello bandwidth"
	stub := &stubBackend{
		getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(body))},
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
	}
	client, _, app := newTestProxyWithGateway(t, stub)

	in, out := &recordingLimiter{}, &recordingLimiter{}
	var hooked []*s3gw.Op
	app.SetBandwidthLimit(func(op *s3gw.Op) (s3gw.BandwidthLimiter, s3gw.BandwidthLimiter) {
		hooked = append(hooked, op)
		return in, out
	})
	var recorded []s3gw.Op
	app.Use(func(ctx context.Context, op *s3gw.Op, next func() error) error {
		err := next()
		recorded = append(recorded, *op)
		return err
	})

	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("b.txt"),
		Body: strings.NewReader("uploaded body"),
	}); err != nil {
		t.Fatal(err)
	}
	got, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, got.Body)
	got.Body.Close()

	if len(hooked) != 2 || len(recorded) != 2 {
		t.Fatalf("expect two hooked and two recorded operations, got %d/%d", len(hooked), len(recorded))
	}
	if hooked[0].Tenant == "" || hooked[0].User == "" {
		t.Errorf("expect the identity on the hooked op, got %+v", hooked[0])
	}
	put, get := recorded[0], recorded[1]
	if put.BytesIn == 0 || in.n.Load() != put.BytesIn {
		t.Errorf("expect the in limiter to see BytesIn=%d, got %d", put.BytesIn, in.n.Load())
	}
	if get.BytesOut != int64(len(body)) || out.n.Load() != get.BytesOut {
		t.Errorf("expect the out limiter to see BytesOut=%d, got %d", get.BytesOut, out.n.Load())
	}
}

// nil limiters mean unlimited: nothing is paced and nothing fails.
func TestBandwidthLimitNilIsUnlimited(t *testing.T) {
	stub := &stubBackend{getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("x"))}}
	client, _, app := newTestProxyWithGateway(t, stub)
	app.SetBandwidthLimit(func(op *s3gw.Op) (s3gw.BandwidthLimiter, s3gw.BandwidthLimiter) {
		return nil, nil
	})
	got, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, got.Body)
	got.Body.Close()
}

// A *rate.Limiter is a BandwidthLimiter, and sharing one across requests
// makes it an aggregate cap: an upload larger than the burst must take at
// least (size-burst)/rate.
func TestBandwidthLimitPacesWithRateLimiter(t *testing.T) {
	stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)}}
	client, _, app := newTestProxyWithGateway(t, stub)

	// 1 MiB/s with a 64 KiB burst: a 256 KiB body (plus chunked framing)
	// needs at least (256-64)/1024 s = 187ms; assert half to stay unflaky
	limiter := rate.NewLimiter(1<<20, 64<<10)
	app.SetBandwidthLimit(func(op *s3gw.Op) (s3gw.BandwidthLimiter, s3gw.BandwidthLimiter) {
		return limiter, nil
	})

	start := time.Now()
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("b.txt"),
		Body: bytes.NewReader(bytes.Repeat([]byte("x"), 256<<10)),
	}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 90*time.Millisecond {
		t.Errorf("expect a paced upload to take at least ~187ms, took %v", elapsed)
	}
}

// A limiter failure aborts the request instead of letting it through unpaced.
func TestBandwidthLimitFailureAborts(t *testing.T) {
	const body = "should not be fully served"
	stub := &stubBackend{
		// ContentLength makes the truncation detectable: without it the
		// response is chunked and an aborted handler still ends it cleanly
		getOut: &s3.GetObjectOutput{
			Body:          io.NopCloser(strings.NewReader(body)),
			ContentLength: aws.Int64(int64(len(body))),
		},
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
	}
	client, _, app := newTestProxyWithGateway(t, stub)

	failing := &recordingLimiter{err: errors.New("limiter refused")}
	app.SetBandwidthLimit(func(op *s3gw.Op) (s3gw.BandwidthLimiter, s3gw.BandwidthLimiter) {
		return failing, failing
	})

	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("b.txt"),
		Body: strings.NewReader("uploaded body"),
	}); err == nil {
		t.Error("expect a failing in limiter to abort the upload")
	}
	// the GET fails on the body: the headers are already out when the
	// response body is paced, so the abort surfaces to the reader
	got, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
	})
	if err == nil {
		_, err = io.Copy(io.Discard, got.Body)
		got.Body.Close()
		if err == nil {
			t.Error("expect a failing out limiter to abort the response body")
		}
	}
}
