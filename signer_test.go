package s3rp_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp"
)

// The proxy verifies with a different access key on nearly every request, so
// these cover the per-key signer cache: many keys must all verify correctly
// (including when two keys land in the same cache slot), and a wrong secret
// must still be rejected.

const signerTestKeys = 32

var signerTestTime = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func newMultiKeyApp(tb testing.TB) *s3rp.S3RP {
	tb.Helper()
	users := make([]*s3rp.UserConfig, signerTestKeys)
	for i := range users {
		users[i] = &s3rp.UserConfig{
			Name: fmt.Sprintf("user%02d", i),
			Keys: []*s3rp.KeyConfig{{
				AccessKeyID:     fmt.Sprintf("KEY%02d", i),
				SecretAccessKey: s3rp.Password(fmt.Sprintf("secret%02d", i)),
			}},
		}
	}
	cfg := &s3rp.Config{Tenants: []*s3rp.TenantConfig{{
		Name:  "acme",
		Users: users,
		Buckets: []*s3rp.BucketConfig{{
			Name: "data",
			Backend: &s3rp.BackendConfig{
				Endpoint: "http://backend.invalid", Bucket: "backend-data",
				AccessKeyID: "bk", SecretAccessKey: "bs",
			},
		}},
	}}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		tb.Fatal(err)
	}
	app, err := s3rp.NewWithStore(context.Background(), cfg, s3rp.NewConfigStore(cfg))
	if err != nil {
		tb.Fatal(err)
	}
	app.SetBackend("data", concurrentStub{&stubBackend{}})
	app.SetNow(func() time.Time { return signerTestTime })
	return app
}

// concurrentStub is a backend safe for concurrent requests: it records nothing
// and hands each caller its own body, unlike stubBackend which is written for
// single-threaded assertions.
type concurrentStub struct{ *stubBackend }

func (concurrentStub) GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	return &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("x"))}, nil
}

// signedGetFor builds a request signed by key i, or by a wrong secret.
func signedGetFor(tb testing.TB, i int, secret string) *http.Request {
	tb.Helper()
	const payloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	const url = "http://s3.example.com/data/some/key.txt"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		tb.Fatal(err)
	}
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: fmt.Sprintf("KEY%02d", i), SecretAccessKey: secret}
	if err := signer.SignHTTP(context.Background(), creds, req, payloadHash, "s3", "us-east-1", signerTestTime); err != nil {
		tb.Fatal(err)
	}
	sr := httptest.NewRequest("GET", url, bytes.NewReader(nil))
	sr.Header = req.Header
	sr.Host = req.Host
	return sr
}

func TestSignerCacheManyKeys(t *testing.T) {
	app := newMultiKeyApp(t)
	h := app.Handler()

	// every key verifies, repeatedly (so cached signers are reused) and
	// concurrently (so slot installation races are exercised under -race)
	var wg sync.WaitGroup
	for round := range 3 {
		for i := range signerTestKeys {
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := signedGetFor(t, i, fmt.Sprintf("secret%02d", i))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Errorf("round %d key %d: status %d: %s", round, i, w.Code, w.Body.String())
				}
			}()
		}
	}
	wg.Wait()

	// a wrong secret is still rejected, and does not poison the cached signer
	bad := signedGetFor(t, 0, "wrongsecret")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, bad)
	if w.Code != http.StatusForbidden {
		t.Errorf("expect 403 for a bad signature, got %d", w.Code)
	}
	good := signedGetFor(t, 0, "secret00")
	w = httptest.NewRecorder()
	h.ServeHTTP(w, good)
	if w.Code != http.StatusOK {
		t.Errorf("valid request after a bad one must still pass, got %d: %s", w.Code, w.Body.String())
	}
}

// BenchmarkVerifyKeyDiversity guards the per-key signer cache. The SDK's own
// derived-key cache holds one entry per (service, region) and takes a write
// lock on every miss, so sharing a single signer across access keys both
// re-derives the signing key per request and serializes verification — the
// many-keys parallel case then runs *slower* than serial. Both diversity
// cases should stay close to their single-key counterpart.
func BenchmarkVerifyKeyDiversity(b *testing.B) {
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(io.Discard, nil)))
	b.Cleanup(func() { slog.SetDefault(old) })

	app := newMultiKeyApp(b)
	h := app.Handler()
	reqs := make([]*http.Request, signerTestKeys)
	for i := range reqs {
		reqs[i] = signedGetFor(b, i, fmt.Sprintf("secret%02d", i))
	}
	serve := func(b *testing.B, req *http.Request) {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			b.Fatalf("status %d: %s", w.Code, w.Body.String())
		}
	}

	b.Run("serial/1key", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			serve(b, reqs[0])
		}
	})
	b.Run("serial/manykeys", func(b *testing.B) {
		b.ReportAllocs()
		i := 0
		for b.Loop() {
			serve(b, reqs[i%signerTestKeys])
			i++
		}
	})
	b.Run("parallel/1key", func(b *testing.B) {
		b.ReportAllocs()
		b.RunParallel(func(pb *testing.PB) {
			for pb.Next() {
				serve(b, reqs[0])
			}
		})
	})
	b.Run("parallel/manykeys", func(b *testing.B) {
		b.ReportAllocs()
		var n atomic.Int64
		b.RunParallel(func(pb *testing.PB) {
			i := int(n.Add(1))
			for pb.Next() {
				serve(b, reqs[i%signerTestKeys])
				i++
			}
		})
	})
}
