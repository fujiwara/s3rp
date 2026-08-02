package s3gw_test

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

// The proxy verifies with a different access key on nearly every request, so
// these cover the per-key signer cache: many keys must all verify correctly
// (including when two keys land in the same cache slot), and a wrong secret
// must still be rejected. They run against the bare gateway — no observer,
// no root-package assembly — so what they measure is the gateway itself.

const signerTestKeys = 32

var signerTestTime = time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)

func newMultiKeyGateway(tb testing.TB) *s3gw.Gateway {
	tb.Helper()
	pathStyle := true
	keys := make(map[string]*store.Key, signerTestKeys)
	for i := range signerTestKeys {
		akid := fmt.Sprintf("KEY%02d", i)
		keys[akid] = &store.Key{
			AccessKeyID:     akid,
			SecretAccessKey: store.Password(fmt.Sprintf("secret%02d", i)),
			Tenant:          "acme",
			User:            fmt.Sprintf("user%02d", i),
		}
	}
	gw := s3gw.New(memStore{
		keys: keys,
		buckets: map[string]*store.Bucket{
			"data": {
				Tenant: "acme", Name: "data",
				Backend: &store.Backend{
					Endpoint: "http://backend.invalid", Region: "us-east-1",
					Bucket: "backend-data", AccessKeyID: "bk", SecretAccessKey: "bs",
					UsePathStyle: &pathStyle,
				},
			},
		},
	})
	// stubGet builds a fresh body per call, so it is safe for the
	// concurrent cases
	if err := gw.SetBackend("data", stubGet{body: "x"}); err != nil {
		tb.Fatal(err)
	}
	gw.SetNow(func() time.Time { return signerTestTime })
	return gw
}

// signedGetFor builds a request signed by key i, or by a wrong secret.
func signedGetFor(tb testing.TB, i int, secret string) *http.Request {
	tb.Helper()
	creds := aws.Credentials{AccessKeyID: fmt.Sprintf("KEY%02d", i), SecretAccessKey: secret}
	return signedRequest(tb, "GET", "http://s3.example.com/data/some/key.txt",
		nil, emptyPayloadHash, signerTestTime, creds, nil)
}

func TestSignerCacheManyKeys(t *testing.T) {
	h := newMultiKeyGateway(t).Handler()

	// every key verifies, repeatedly (so cached signers are reused) and
	// concurrently (so slot installation races are exercised under -race)
	var wg sync.WaitGroup
	for round := range 3 {
		for i := range signerTestKeys {
			wg.Go(func() {
				req := signedGetFor(t, i, fmt.Sprintf("secret%02d", i))
				w := httptest.NewRecorder()
				h.ServeHTTP(w, req)
				if w.Code != http.StatusOK {
					t.Errorf("round %d key %d: status %d: %s", round, i, w.Code, w.Body.String())
				}
			})
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
//
// The manykeys allocs jitter run to run: 32 keys land in one of 512 slots by
// maphash seed luck, a pair collides in roughly six runs out of ten, and a
// collided pair rebuilds its signers on every alternation. That is the
// displacement the signer cache stats report as Evictions.
func BenchmarkVerifyKeyDiversity(b *testing.B) {
	h := newMultiKeyGateway(b).Handler()
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
