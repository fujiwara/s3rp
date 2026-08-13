package s3gw_test

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/s3gw"
)

// Zero rates mean unlimited: nothing is paced and nothing fails.
func TestBandwidthLimitZeroIsUnlimited(t *testing.T) {
	stub := &stubBackend{getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("x"))}}
	client, _, app := newTestProxyWithGateway(t, stub)
	app.SetBandwidthLimit(func(op *s3gw.Op) (float64, float64) {
		return 0, 0
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

// The in rate paces an upload: shapeio spends its initial burst, so a
// 256 KiB body at 1 MiB/s must take at least ~250ms.
func TestBandwidthLimitPacesUpload(t *testing.T) {
	stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)}}
	client, _, app := newTestProxyWithGateway(t, stub)

	var hooked []*s3gw.Op
	app.SetBandwidthLimit(func(op *s3gw.Op) (float64, float64) {
		hooked = append(hooked, op)
		return 1 << 20, 0
	})

	start := time.Now()
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("b.txt"),
		Body: bytes.NewReader(bytes.Repeat([]byte("x"), 256<<10)),
	}); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("expect a paced upload to take at least ~250ms, took %v", elapsed)
	}
	if len(hooked) != 1 {
		t.Fatalf("expect one hooked operation, got %d", len(hooked))
	}
	if hooked[0].Tenant == "" || hooked[0].User == "" || hooked[0].Action != "s3:PutObject" {
		t.Errorf("unexpected hooked op %+v", hooked[0])
	}
}

// The out rate paces a download independently of the in rate.
func TestBandwidthLimitPacesDownload(t *testing.T) {
	body := bytes.Repeat([]byte("y"), 256<<10)
	// hide bytes.Reader's WriterTo so io.Copy writes in 32 KiB chunks, as a
	// real backend body would: shapeio waits after each write, so a source
	// io.Copy can WriteTo in a single call would go out entirely unpaced
	stub := &stubBackend{getOut: &s3.GetObjectOutput{
		Body:          io.NopCloser(struct{ io.Reader }{bytes.NewReader(body)}),
		ContentLength: aws.Int64(int64(len(body))),
	}}
	client, _, app := newTestProxyWithGateway(t, stub)
	app.SetBandwidthLimit(func(op *s3gw.Op) (float64, float64) {
		return 0, 1 << 20
	})

	start := time.Now()
	got, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("a.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	n, err := io.Copy(io.Discard, got.Body)
	got.Body.Close()
	if err != nil || n != int64(len(body)) {
		t.Fatalf("expect the full body, got %d bytes, err %v", n, err)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Errorf("expect a paced download to take at least ~250ms, took %v", elapsed)
	}
}

// The byte counts keep measuring the wire with pacing inserted: the paced
// wrappers sit inside the counting ones.
func TestBandwidthLimitKeepsCounting(t *testing.T) {
	const body = "hello bandwidth"
	stub := &stubBackend{
		getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader(body))},
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
	}
	client, _, app := newTestProxyWithGateway(t, stub)
	app.SetBandwidthLimit(func(op *s3gw.Op) (float64, float64) {
		return 1 << 30, 1 << 30 // fast enough not to slow the test
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

	if len(recorded) != 2 {
		t.Fatalf("expect two operations recorded, got %d", len(recorded))
	}
	if recorded[0].BytesIn == 0 {
		t.Errorf("expect the paced upload to keep counting BytesIn, got %+v", recorded[0])
	}
	if recorded[1].BytesOut != int64(len(body)) {
		t.Errorf("expect the paced download to count BytesOut=%d, got %d", len(body), recorded[1].BytesOut)
	}
}
