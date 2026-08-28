package s3gw_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"
)

// An authentication failure explains itself to the observer the way a
// policy refusal does: the reason and the key the request claimed.
func TestAuthFailureObserved(t *testing.T) {
	gw := newTestGateway(t)
	if err := gw.SetBackend("testbucket", stubGet{body: "hello"}); err != nil {
		t.Fatal(err)
	}
	var last *s3gw.RequestInfo
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { last = info })
	ts := newTestServer(t, gw)
	failure := func(t *testing.T, code string) *sigv4.AuthFailure {
		t.Helper()
		if last == nil || last.Code != code {
			t.Fatalf("expect %s observed, got %+v", code, last)
		}
		var f *sigv4.AuthFailure
		if !errors.As(last.Err, &f) {
			t.Fatalf("expect an AuthFailure cause, got %v", last.Err)
		}
		if last.Tenant != "" || last.User != "" {
			t.Errorf("an unverified request must not carry an identity: %+v", last)
		}
		return f
	}

	t.Run("wrong secret", func(t *testing.T) {
		c := newS3Client(t, ts, testAccessKeyID, "wrong")
		_, err := c.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("testbucket"), Key: aws.String("a")})
		if err == nil {
			t.Fatal("expect failure")
		}
		f := failure(t, "SignatureDoesNotMatch")
		if f.Reason != sigv4.ReasonSignatureMismatch || f.AccessKeyID != testAccessKeyID {
			t.Errorf("unexpected failure %+v", f)
		}
		if last.AccessKeyID != testAccessKeyID {
			t.Errorf("expect the presented key recorded, got %q", last.AccessKeyID)
		}
	})
	t.Run("unknown key", func(t *testing.T) {
		c := newS3Client(t, ts, "NOSUCHKEY", "whatever")
		_, err := c.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("testbucket"), Key: aws.String("a")})
		if err == nil {
			t.Fatal("expect failure")
		}
		f := failure(t, "InvalidAccessKeyId")
		if f.Reason != sigv4.ReasonUnknownKey || f.AccessKeyID != "NOSUCHKEY" || !errors.Is(f, store.ErrNotFound) {
			t.Errorf("unexpected failure %+v", f)
		}
		if last.AccessKeyID != "NOSUCHKEY" {
			t.Errorf("expect the presented key recorded, got %q", last.AccessKeyID)
		}
	})
	t.Run("no credentials at all", func(t *testing.T) {
		resp, err := http.Get(ts + "/testbucket/a")
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		f := failure(t, "AccessDenied")
		if f.Reason != sigv4.ReasonNoAuth || f.AccessKeyID != "" || last.AccessKeyID != "" {
			t.Errorf("unexpected failure %+v (recorded key %q)", f, last.AccessKeyID)
		}
	})
	t.Run("unsigned x-amz header", func(t *testing.T) {
		c := newS3Client(t, ts, testAccessKeyID, testSecretAccessKey)
		_, err := c.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("testbucket"), Key: aws.String("a")},
			func(o *s3.Options) {
				o.HTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
					r.Header.Set("x-amz-storage-class", "GLACIER") // added after signing
					return http.DefaultTransport.RoundTrip(r)
				})}
			})
		if err == nil {
			t.Fatal("expect failure")
		}
		f := failure(t, "AccessDenied")
		if f.Reason != sigv4.ReasonUnsignedHeaders || f.AccessKeyID != testAccessKeyID || !strings.Contains(f.Detail, "x-amz-storage-class") {
			t.Errorf("unexpected failure %+v", f)
		}
	})
	t.Run("expired presigned url", func(t *testing.T) {
		c := newS3Client(t, ts, testAccessKeyID, testSecretAccessKey)
		ps := s3.NewPresignClient(c)
		u, err := ps.PresignGetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("testbucket"), Key: aws.String("a")},
			func(o *s3.PresignOptions) { o.Expires = time.Minute })
		if err != nil {
			t.Fatal(err)
		}
		gw.SetNow(func() time.Time { return time.Now().Add(2 * time.Minute) })
		t.Cleanup(func() { gw.SetNow(time.Now) })
		resp, err := http.Get(u.URL)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		f := failure(t, "AccessDenied")
		if f.Reason != sigv4.ReasonExpired || f.AccessKeyID != testAccessKeyID {
			t.Errorf("unexpected failure %+v", f)
		}
	})
	t.Run("clock skew", func(t *testing.T) {
		gw.SetNow(func() time.Time { return time.Now().Add(time.Hour) })
		t.Cleanup(func() { gw.SetNow(time.Now) })
		c := newS3Client(t, ts, testAccessKeyID, testSecretAccessKey)
		_, err := c.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("testbucket"), Key: aws.String("a")})
		if err == nil {
			t.Fatal("expect failure")
		}
		f := failure(t, "RequestTimeTooSkewed")
		if f.Reason != sigv4.ReasonTimeSkewed || f.AccessKeyID != testAccessKeyID {
			t.Errorf("unexpected failure %+v", f)
		}
	})
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
