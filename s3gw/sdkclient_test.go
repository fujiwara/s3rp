package s3gw_test

import (
	"net/http/httptest"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/s3gw"
)

// The main end-to-end pattern: a gateway on an httptest server with a stub
// backend, driven by a real aws-sdk-go-v2 client holding front credentials.
// The client signs for real, so this covers verification, routing and
// response mapping the way a tenant's SDK exercises them — without a
// backend, and without the root package's assembly.

// newTestProxy returns an SDK client pointed at a gateway serving
// "testbucket" from stub.
func newTestProxy(t *testing.T, stub *stubBackend) (*s3.Client, *httptest.Server) {
	t.Helper()
	client, ts, _ := newTestProxyWithGateway(t, stub)
	return client, ts
}

// newTestProxyWithGateway also hands back the gateway, for tests that install
// hooks or resize caches.
func newTestProxyWithGateway(t *testing.T, stub *stubBackend) (*s3.Client, *httptest.Server, *s3gw.Gateway) {
	t.Helper()
	gw := newTestGateway(t)
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	return newSDKClientFor(t, gw)
}

// newS3Client returns an SDK client for an endpoint, with the SDK's own
// checksum settings.
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

// newTestServer serves gw for the duration of the test and returns its URL.
func newTestServer(t *testing.T, gw *s3gw.Gateway) string {
	t.Helper()
	ts := httptest.NewServer(gw.Handler())
	t.Cleanup(ts.Close)
	return ts.URL
}

// newSDKClientFor serves gw and returns an SDK client configured for it.
func newSDKClientFor(t *testing.T, gw *s3gw.Gateway) (*s3.Client, *httptest.Server, *s3gw.Gateway) {
	t.Helper()
	ts := httptest.NewServer(gw.Handler())
	t.Cleanup(ts.Close)
	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, ""),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts.URL)
		o.UsePathStyle = true
		// the SDK default would add a CRC32 trailer, re-introducing
		// aws-chunked framing on every request; tests that want that path
		// ask for it with newTestProxyDefaultChecksums
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
	return client, ts, gw
}

// newTestProxyDefaultChecksums is newTestProxy with the SDK's own checksum
// settings, so uploads take the aws-chunked trailer path a real client uses.
func newTestProxyDefaultChecksums(t *testing.T, stub *stubBackend) *s3.Client {
	t.Helper()
	gw := newTestGateway(t)
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(gw.Handler())
	t.Cleanup(ts.Close)
	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, ""),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts.URL)
		o.UsePathStyle = true
	})
}
