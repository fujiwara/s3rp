// Command s3tests-harness serves an s3rp gateway augmented with
// CreateBucket/DeleteBucket for running the ceph/s3-tests compatibility
// suite. See s3tests/README.md.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/fujiwara/s3rp/s3tests/harness"
)

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func main() {
	var listen, endpoint, key, secret string
	flag.StringVar(&listen, "listen", envOr("S3TESTS_HARNESS_LISTEN", "127.0.0.1:7481"), "listen address")
	flag.StringVar(&endpoint, "backend-endpoint", envOr("S3RP_TEST_BACKEND_ENDPOINT", "http://127.0.0.1:7480"), "backend S3 endpoint")
	flag.StringVar(&key, "backend-access-key-id", envOr("S3RP_TEST_BACKEND_ACCESS_KEY_ID", "backendkey"), "backend access key id")
	flag.StringVar(&secret, "backend-secret-access-key", envOr("S3RP_TEST_BACKEND_SECRET_ACCESS_KEY", "backendsecret"), "backend secret access key")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	h, err := harness.New(ctx, harness.Config{
		BackendEndpoint:        endpoint,
		BackendAccessKeyID:     key,
		BackendSecretAccessKey: secret,
	})
	if err != nil {
		log.Fatal(err)
	}

	srv := &http.Server{
		Addr:              listen,
		Handler:           h, // directly: no ServeMux in front of S3 paths
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(shutdownCtx)
	}()
	log.Printf("s3tests-harness listening on %s (backend %s)", listen, endpoint)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}
