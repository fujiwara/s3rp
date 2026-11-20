package s3rp_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fujiwara/s3rp"
)

// writeTestCert writes a self-signed certificate for 127.0.0.1 and its key
// into dir, and returns the pool that trusts it.
func writeTestCert(t *testing.T, dir string) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "s3rp test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	certFile = filepath.Join(dir, "cert.pem")
	keyFile = filepath.Join(dir, "key.pem")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AppendCertsFromPEM(certPEM)
	return certFile, keyFile, pool
}

func TestServeTLS(t *testing.T) {
	certFile, keyFile, pool := writeTestCert(t, t.TempDir())
	// Serve takes an address, not a listener: reserve a port and release it
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	l.Close()

	cfg := &s3rp.Config{
		Listen: addr,
		TLS:    &s3rp.TLSConfig{CertFile: certFile, KeyFile: keyFile},
		Tenants: []*s3rp.TenantConfig{{
			Name: "main",
			Users: []*s3rp.UserConfig{{Name: "user1", Keys: []*s3rp.KeyConfig{{
				AccessKeyID: "k1", SecretAccessKey: "s",
			}}}},
			Buckets: []*s3rp.BucketConfig{{
				Name:    "bucket1",
				Backend: &s3rp.BackendConfig{Endpoint: "http://127.0.0.1:1", AccessKeyID: "a", SecretAccessKey: "s"},
			}},
		}},
	}
	app, err := s3rp.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- app.Serve(ctx) }()

	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool}}}
	var resp *http.Response
	for range 50 {
		resp, err = client.Get("https://" + addr + "/")
		if err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// unsigned, so refused — but by the gateway, over TLS
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get("x-amz-request-id") == "" {
		t.Errorf("unexpected response: %d %v", resp.StatusCode, resp.Header)
	}
	cancel()
	if err := <-done; err != nil {
		t.Errorf("Serve: %v", err)
	}
}
