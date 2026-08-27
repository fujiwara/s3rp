package s3gw_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

const vhostSuffix = "s3.example.com"

// newVirtualHostProxy serves gw with virtual-hosted-style addressing under
// vhostSuffix and returns an SDK client that addresses buckets as
// "<bucket>.s3.example.com", dialing the test server whatever the host.
func newVirtualHostProxy(t *testing.T, stub *stubBackend) (*s3.Client, *httptest.Server) {
	t.Helper()
	gw := newTestGateway(t)
	gw.SetVirtualHostSuffix(vhostSuffix)
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(gw.Handler())
	t.Cleanup(ts.Close)
	return newVirtualHostClient(t, ts, "us-east-1"), ts
}

func newVirtualHostClient(t *testing.T, ts *httptest.Server, region string) *s3.Client {
	t.Helper()
	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion(region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, ""),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	addr := ts.Listener.Addr().String()
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}
	t.Cleanup(transport.CloseIdleConnections)
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://" + vhostSuffix)
		o.UsePathStyle = false
		o.HTTPClient = &http.Client{Transport: transport}
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
	})
}

func TestVirtualHostSDK(t *testing.T) {
	stub := &stubBackend{
		putOut:  &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
		getOut:  &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("hello")), ETag: aws.String(`"e"`)},
		listOut: &s3.ListObjectsV2Output{KeyCount: aws.Int32(0)},
		hbOut:   &s3.HeadBucketOutput{},
		createMPUOut: &s3.CreateMultipartUploadOutput{
			UploadId: aws.String("u1"),
		},
		completeMPUOut: &s3.CompleteMultipartUploadOutput{ETag: aws.String(`"m"`)},
	}
	client, ts := newVirtualHostProxy(t, stub)

	t.Run("put with slash key", func(t *testing.T) {
		_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("testbucket"), Key: aws.String("dir/a b.txt"),
			Body: strings.NewReader("hello"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := aws.ToString(stub.putIn.Bucket); got != "backend-testbucket" {
			t.Errorf("backend bucket %q", got)
		}
		if got := aws.ToString(stub.putIn.Key); got != "dir/a b.txt" {
			t.Errorf("key %q", got)
		}
	})
	t.Run("get", func(t *testing.T) {
		out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket: aws.String("testbucket"), Key: aws.String("dir/a b.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer out.Body.Close()
		if b, _ := io.ReadAll(out.Body); string(b) != "hello" {
			t.Errorf("body %q", b)
		}
	})
	t.Run("bucket-level operations", func(t *testing.T) {
		if _, err := client.HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String("testbucket")}); err != nil {
			t.Fatal(err)
		}
		out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String("testbucket")})
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToString(out.Name) != "testbucket" {
			t.Errorf("listing names %q, want the front bucket", aws.ToString(out.Name))
		}
		if got := aws.ToString(stub.listIn.Bucket); got != "backend-testbucket" {
			t.Errorf("backend bucket %q", got)
		}
	})
	t.Run("list buckets on the bare endpoint", func(t *testing.T) {
		out, err := client.ListBuckets(t.Context(), &s3.ListBucketsInput{})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Buckets) != 1 || aws.ToString(out.Buckets[0].Name) != "testbucket" {
			t.Errorf("buckets %+v", out.Buckets)
		}
	})
	t.Run("multipart location mirrors the addressing", func(t *testing.T) {
		out, err := client.CompleteMultipartUpload(t.Context(), &s3.CompleteMultipartUploadInput{
			Bucket: aws.String("testbucket"), Key: aws.String("dir/big.bin"), UploadId: aws.String("u1"),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
				{PartNumber: aws.Int32(1), ETag: aws.String(`"p1"`)},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := aws.ToString(out.Location); got != "http://testbucket.s3.example.com/dir/big.bin" {
			t.Errorf("location %q", got)
		}
		if aws.ToString(out.Bucket) != "testbucket" {
			t.Errorf("bucket %q", aws.ToString(out.Bucket))
		}
	})
	t.Run("unknown bucket", func(t *testing.T) {
		_, err := client.HeadBucket(t.Context(), &s3.HeadBucketInput{Bucket: aws.String("nosuchbucket")})
		if err == nil {
			t.Fatal("expect an error")
		}
	})
	t.Run("path style still served", func(t *testing.T) {
		pathClient := newS3Client(t, ts.URL, testAccessKeyID, testSecretAccessKey)
		out, err := pathClient.CompleteMultipartUpload(t.Context(), &s3.CompleteMultipartUploadInput{
			Bucket: aws.String("testbucket"), Key: aws.String("dir/big.bin"), UploadId: aws.String("u1"),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
				{PartNumber: aws.Int32(1), ETag: aws.String(`"p1"`)},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := aws.ToString(out.Location); got != ts.URL+"/testbucket/dir/big.bin" {
			t.Errorf("location %q", got)
		}
	})
}

// TestVirtualHostResolution checks which Host values name a bucket, through
// the unauthenticated preflight (200 when the bucket resolved, 403 when not).
func TestVirtualHostResolution(t *testing.T) {
	ts, gw := newCORSTestProxy(t)
	gw.SetVirtualHostSuffix(vhostSuffix)
	tests := []struct {
		name   string
		host   string
		path   string
		status int
	}{
		{"vhost", "webbucket.s3.example.com", "/key.txt", http.StatusOK},
		{"vhost with port", "webbucket.s3.example.com:8080", "/key.txt", http.StatusOK},
		{"vhost is case-insensitive", "WebBucket.S3.Example.COM", "/key.txt", http.StatusOK},
		{"vhost root path", "webbucket.s3.example.com", "/", http.StatusOK},
		{"bare endpoint is path style", "s3.example.com", "/webbucket/key.txt", http.StatusOK},
		{"bare endpoint without bucket", "s3.example.com", "/key.txt", http.StatusForbidden},
		{"other host is path style", "proxy.example.net", "/webbucket/key.txt", http.StatusOK},
		{"dotted label falls back to path style", "a.webbucket.s3.example.com", "/webbucket/key.txt", http.StatusOK},
		{"dotted label names no bucket", "a.webbucket.s3.example.com", "/key.txt", http.StatusForbidden},
		{"suffix needs a dot boundary", "notwebbuckets3.example.com", "/webbucket/key.txt", http.StatusOK},
		{"suffix needs a dot boundary, no bucket in path", "webbuckets3.example.com", "/key.txt", http.StatusForbidden},
		{"vhost bucket wins over path", "webbucket.s3.example.com", "/plainbucket/key.txt", http.StatusOK},
		{"unknown vhost bucket", "nosuchbucket.s3.example.com", "/key.txt", http.StatusForbidden},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, ts+tc.path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = tc.host
			req.Header.Set("Origin", "https://app.example.com")
			req.Header.Set("Access-Control-Request-Method", "GET")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != tc.status {
				t.Errorf("expect %d, got %d", tc.status, resp.StatusCode)
			}
		})
	}
}

// TestVirtualHostSuffixSpellings checks that the suffix is accepted however
// an operator is likely to write the endpoint's host name.
func TestVirtualHostSuffixSpellings(t *testing.T) {
	for _, suffix := range []string{"s3.example.com", ".s3.example.com", "S3.Example.COM.", "s3.example.com:8080"} {
		t.Run(suffix, func(t *testing.T) {
			ts, gw := newCORSTestProxy(t)
			gw.SetVirtualHostSuffix(suffix)
			req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, ts+"/key.txt", nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Host = "webbucket.s3.example.com"
			req.Header.Set("Origin", "https://app.example.com")
			req.Header.Set("Access-Control-Request-Method", "GET")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Errorf("expect 200, got %d", resp.StatusCode)
			}
		})
	}
}

func TestVirtualHostDisabledByDefault(t *testing.T) {
	ts, _ := newCORSTestProxy(t)
	req, err := http.NewRequestWithContext(t.Context(), http.MethodOptions, ts+"/key.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "webbucket.s3.example.com"
	req.Header.Set("Origin", "https://app.example.com")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expect 403 without a suffix, got %d", resp.StatusCode)
	}
}

func TestVirtualHostPostObject(t *testing.T) {
	gw := newTestGateway(t)
	gw.SetVirtualHostSuffix(vhostSuffix)
	stub := &stubPost{}
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	form := &postForm{
		conditions: []string{`["starts-with", "$key", "user/"]`, `{"success_action_status": "201"}`},
		fields:     [][2]string{{"key", "user/${filename}"}, {"success_action_status", "201"}},
		filename:   "hello.txt",
		content:    "hello post",
	}
	req := form.request(t)
	req.Host = "testbucket." + vhostSuffix
	req.URL.Path = "/"
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("unexpected status %d: %s", w.Code, w.Body.String())
	}
	want := "http://testbucket.s3.example.com/user/hello.txt"
	if got := w.Header().Get("Location"); got != want {
		t.Errorf("Location %q, want %q", got, want)
	}
	if !strings.Contains(w.Body.String(), "<Location>"+want+"</Location>") {
		t.Errorf("body lacks the location: %s", w.Body.String())
	}
	if got := aws.ToString(stub.putIn.Key); got != "user/hello.txt" {
		t.Errorf("key %q", got)
	}
}
