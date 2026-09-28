package s3gw_test

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

// A client talking to the gateway over TLS sends its checksum as an
// aws-chunked trailer (the SDK and the CLI do so for every https upload).
// The proxy verifies the trailer and must still be able to upload to the
// backend whatever its scheme: the SDK toward the backend can only recompute
// a checksum over an unseekable body as a trailer, which it sends over https
// only. These tests drive the real backend client (no stub) against a
// recording backend server, once plain http and once https.

// recordingBackend records the PutObject / UploadPart requests it receives.
type recordingBackend struct {
	mu   sync.Mutex
	reqs []recordedRequest
}

type recordedRequest struct {
	header http.Header
	body   []byte
}

func (b *recordingBackend) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == http.MethodPost && r.URL.Query().Has("uploads"):
		w.Header().Set("Content-Type", "application/xml")
		_, _ = io.WriteString(w, `<InitiateMultipartUploadResult><Bucket>b</Bucket><Key>k</Key><UploadId>upload-1</UploadId></InitiateMultipartUploadResult>`)
		return
	case r.Method == http.MethodPut:
		b.mu.Lock()
		b.reqs = append(b.reqs, recordedRequest{header: r.Header.Clone(), body: body})
		b.mu.Unlock()
		w.Header().Set("ETag", `"etag-1"`)
		return
	}
	http.Error(w, "unexpected "+r.Method+" "+r.URL.String(), http.StatusNotImplemented)
}

func (b *recordingBackend) last(t *testing.T) recordedRequest {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.reqs) == 0 {
		t.Fatal("backend received no PUT")
	}
	return b.reqs[len(b.reqs)-1]
}

// trailerSetup serves a gateway over TLS (so the client sends trailer
// checksums) whose "trailerbucket" lives on a recording backend served over
// http or https, reached through the gateway's real SDK client.
func trailerSetup(t *testing.T, backendTLS bool, configure ...func(*s3gw.Gateway)) (*s3.Client, *recordingBackend) {
	t.Helper()
	rec := &recordingBackend{}
	var backendTS *httptest.Server
	if backendTLS {
		backendTS = httptest.NewTLSServer(rec)
	} else {
		backendTS = httptest.NewServer(rec)
	}
	t.Cleanup(backendTS.Close)

	m := buildStore(t, "trailertenant",
		[]userSpec{{name: "u", keyID: testAccessKeyID, secret: testSecretAccessKey}},
		[]bucketSpec{{name: "trailerbucket", endpoint: backendTS.URL}})
	gw := s3gw.New(m)
	if backendTLS {
		// trust the test server's certificate in the backend client
		gw.SetClientOptions(func(*store.Backend) []func(*s3.Options) {
			return []func(*s3.Options){func(o *s3.Options) { o.HTTPClient = backendTS.Client() }}
		})
	}
	for _, f := range configure {
		f(gw)
	}
	proxyTS := httptest.NewTLSServer(gw.Handler())
	t.Cleanup(proxyTS.Close)

	cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, "")),
		awsconfig.WithHTTPClient(proxyTS.Client()),
	)
	if err != nil {
		t.Fatal(err)
	}
	// the SDK's own checksum settings: over https this is a CRC32 trailer
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(proxyTS.URL)
		o.UsePathStyle = true
	})
	return client, rec
}

func TestTrailerChecksumHTTPBackend(t *testing.T) {
	client, rec := trailerSetup(t, false)
	content := strings.Repeat("trailer over http backend ", 50)
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("trailerbucket"), Key: aws.String("o.txt"), Body: strings.NewReader(content),
	}); err != nil {
		t.Fatalf("PutObject through an http backend: %v", err)
	}
	got := rec.last(t)
	// the body reaches the backend decoded, as a plain payload
	if string(got.body) != content {
		t.Errorf("backend body mismatch: %d bytes", len(got.body))
	}
	for _, h := range []string{"x-amz-trailer", "x-amz-sdk-checksum-algorithm", "x-amz-checksum-crc32"} {
		if v := got.header.Get(h); v != "" {
			t.Errorf("backend request must not carry %s over http, got %q", h, v)
		}
	}
	if v := got.header.Get("x-amz-content-sha256"); v != "UNSIGNED-PAYLOAD" {
		t.Errorf("x-amz-content-sha256 = %q", v)
	}

	// the same for a multipart part
	mp, err := client.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String("trailerbucket"), Key: aws.String("big.bin"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.UploadPart(t.Context(), &s3.UploadPartInput{
		Bucket: aws.String("trailerbucket"), Key: aws.String("big.bin"), UploadId: mp.UploadId,
		PartNumber: aws.Int32(1), Body: strings.NewReader(content),
	}); err != nil {
		t.Fatalf("UploadPart through an http backend: %v", err)
	}
	if got := rec.last(t); string(got.body) != content || got.header.Get("x-amz-trailer") != "" {
		t.Errorf("UploadPart backend request: %d bytes, trailer %q", len(got.body), got.header.Get("x-amz-trailer"))
	}
}

func TestTrailerChecksumHTTPSBackend(t *testing.T) {
	client, rec := trailerSetup(t, true)
	content := strings.Repeat("trailer over https backend ", 50)
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("trailerbucket"), Key: aws.String("o.txt"), Body: strings.NewReader(content),
	}); err != nil {
		t.Fatalf("PutObject through an https backend: %v", err)
	}
	got := rec.last(t)
	// the algorithm is forwarded: the SDK recomputes the checksum as a
	// trailer of its own, so the backend can store it
	if v := got.header.Get("x-amz-trailer"); !strings.EqualFold(v, "x-amz-checksum-crc32") {
		t.Errorf("x-amz-trailer = %q, want x-amz-checksum-crc32", v)
	}
	if v := got.header.Get("x-amz-content-sha256"); v != "STREAMING-UNSIGNED-PAYLOAD-TRAILER" {
		t.Errorf("x-amz-content-sha256 = %q", v)
	}
	if !strings.Contains(string(got.body), content) || !strings.Contains(string(got.body), "x-amz-checksum-crc32:") {
		t.Errorf("backend body is not an aws-chunked payload with a checksum trailer (%d bytes)", len(got.body))
	}
}

// A trailer the proxy cannot verify must be refused, not dropped: over TLS
// the SDK sends a SHA512 checksum as a trailer, and accepting the upload
// would report an integrity check nobody made.
func TestUnsupportedTrailerChecksum(t *testing.T) {
	client, rec := trailerSetup(t, true)
	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket:            aws.String("trailerbucket"),
		Key:               aws.String("sha512.txt"),
		Body:              strings.NewReader("sha512 trailer content"),
		ChecksumAlgorithm: types.ChecksumAlgorithmSha512,
	})
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != "NotImplemented" {
		t.Fatalf("expect NotImplemented, got %v", err)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.reqs) != 0 {
		t.Error("the refused upload reached the backend")
	}
}

// Offered as verified, a SHA512 trailer is verified by the gateway and the
// algorithm handed to an https backend to recompute; offered only as
// forwarded, the gateway cannot vouch for a trailer and refuses it.
func TestSHA512TrailerBySupport(t *testing.T) {
	withSHA512 := func(s s3gw.ChecksumSupport) func(*s3gw.Gateway) {
		return func(gw *s3gw.Gateway) {
			support := s3gw.DefaultChecksumSupport()
			support["SHA512"] = s
			if err := gw.SetChecksumSupport(support); err != nil {
				t.Fatal(err)
			}
		}
	}
	put := func(client *s3.Client) error {
		_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket:            aws.String("trailerbucket"),
			Key:               aws.String("sha512.txt"),
			Body:              strings.NewReader("sha512 trailer content"),
			ChecksumAlgorithm: types.ChecksumAlgorithmSha512,
		})
		return err
	}
	t.Run("verified", func(t *testing.T) {
		client, rec := trailerSetup(t, true, withSHA512(s3gw.ChecksumVerified))
		if err := put(client); err != nil {
			t.Fatal(err)
		}
		if got := rec.last(t).header.Get("X-Amz-Trailer"); got != "x-amz-checksum-sha512" {
			t.Errorf("expect the backend SDK to send its own SHA512 trailer, got %q", got)
		}
	})
	t.Run("forwarded", func(t *testing.T) {
		client, rec := trailerSetup(t, true, withSHA512(s3gw.ChecksumForwarded))
		var ae smithy.APIError
		if err := put(client); !errors.As(err, &ae) || ae.ErrorCode() != "NotImplemented" {
			t.Fatalf("expect NotImplemented, got %v", err)
		}
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if len(rec.reqs) != 0 {
			t.Error("the refused upload reached the backend")
		}
	})
}
