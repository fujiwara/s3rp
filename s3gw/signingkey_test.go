package s3gw_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"
)

// derivedKeyStore stands in for a store that never hands the secret to the
// gateway: GetKey replaces it with a SigningKeyFunc, as a store backed by a
// key service would. gone makes the derivation report the key deleted.
type derivedKeyStore struct {
	store.Store
	derivations *atomic.Int32
	gone        bool
}

func (s derivedKeyStore) GetKey(ctx context.Context, id, token string) (*store.Key, error) {
	k, err := s.Store.GetKey(ctx, id, token)
	if err != nil {
		return nil, err
	}
	secret := []byte(k.SecretAccessKey)
	dk := *k
	dk.SecretAccessKey = ""
	dk.SecretVersion = "v1"
	dk.SigningKey = func(_ context.Context, date, region string) ([]byte, error) {
		s.derivations.Add(1)
		if s.gone {
			return nil, fmt.Errorf("%w: deleted after lookup", store.ErrNotFound)
		}
		return sigv4.DeriveSigningKey(secret, date, region), nil
	}
	return &dk, nil
}

func newDerivedKeyGateway(gone bool) (*s3gw.Gateway, *atomic.Int32) {
	var n atomic.Int32
	return s3gw.New(derivedKeyStore{Store: newTestStore(), derivations: &n, gone: gone}), &n
}

// TestGatewayDerivedSigningKey checks the wiring of store.Key.SigningKey:
// requests verify without the gateway ever seeing the secret, the key is
// derived once while cached, and aws-chunked bodies verify their chunk
// signatures with the derived key.
func TestGatewayDerivedSigningKey(t *testing.T) {
	gw, derivations := newDerivedKeyGateway(false)
	if err := gw.SetBackend("testbucket", stubGet{body: "x"}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
			nil, emptyPayloadHash, time.Now(), testCreds(), nil)
		w := httptest.NewRecorder()
		gw.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("expect 200, got %d: %s", w.Code, w.Body.String())
		}
	}
	if n := derivations.Load(); n != 1 {
		t.Errorf("derivations = %d, want 1", n)
	}

	gw, _ = newDerivedKeyGateway(false)
	testRawChunkedPut(t, gw)
}

// A key the store reports gone at derivation time is an unknown key to the
// client, as at GetKey.
func TestGatewayDerivedSigningKeyGone(t *testing.T) {
	gw, _ := newDerivedKeyGateway(true)
	req := signedRequest(t, "GET", "http://s3.example.com/testbucket/a.txt",
		nil, emptyPayloadHash, time.Now(), testCreds(), nil)
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, req)
	if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "InvalidAccessKeyId") {
		t.Errorf("expect InvalidAccessKeyId, got %d: %s", w.Code, w.Body.String())
	}
}
