package store_test

import (
	"testing"

	"github.com/fujiwara/s3rp/store"
)

func TestBackendIsHTTPS(t *testing.T) {
	for endpoint, want := range map[string]bool{
		"":                          true, // no endpoint: the SDK resolves Amazon S3, which is https
		"https://s3.example":        true,
		"HTTPS://s3.example:443":    true,
		"https://rgw.internal:7443": true,
		"http://127.0.0.1:7480":     false,
		"http://backend.invalid":    false,
	} {
		if got := (&store.Backend{Endpoint: endpoint}).IsHTTPS(); got != want {
			t.Errorf("%q: got %v, want %v", endpoint, got, want)
		}
	}
}
