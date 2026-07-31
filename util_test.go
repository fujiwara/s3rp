package s3rp_test

import (
	"net/http/httptest"
	"os"
	"testing"

	"github.com/fujiwara/s3rp"
)

func writeFile(t *testing.T, path, content string) error {
	t.Helper()
	return os.WriteFile(path, []byte(content), 0644)
}

func newTestServerForApp(t *testing.T, app *s3rp.S3RP) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)
	return ts
}
