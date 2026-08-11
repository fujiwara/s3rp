package harness_test

import (
	"testing"

	"github.com/fujiwara/s3rp/s3tests/harness"
)

func TestNewRequiresBackendConfig(t *testing.T) {
	if _, err := harness.New(t.Context(), harness.Config{}); err == nil {
		t.Error("expected an error for a missing endpoint")
	}
	if _, err := harness.New(t.Context(), harness.Config{
		BackendEndpoint: "http://127.0.0.1:7480",
	}); err == nil {
		t.Error("expected an error for missing credentials")
	}
}
