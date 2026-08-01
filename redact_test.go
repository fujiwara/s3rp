package s3rp_test

import (
	"net/url"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp"
)

func TestRedactQuery(t *testing.T) {
	q, _ := url.ParseQuery("X-Amz-Algorithm=AWS4-HMAC-SHA256&X-Amz-Credential=AKID%2F20260101%2Fus-east-1%2Fs3%2Faws4_request&X-Amz-Signature=deadbeefcafe&X-Amz-Security-Token=sessiontoken&X-Amz-Expires=900&list-type=2")
	got := s3rp.RedactQuery(q)
	for _, secret := range []string{"deadbeefcafe", "sessiontoken"} {
		if strings.Contains(got, secret) {
			t.Errorf("redacted query still contains secret %q: %s", secret, got)
		}
	}
	// non-secret params are preserved for debugging
	for _, keep := range []string{"X-Amz-Credential", "X-Amz-Expires", "list-type"} {
		if !strings.Contains(got, keep) {
			t.Errorf("redacted query dropped %q: %s", keep, got)
		}
	}
	if !strings.Contains(got, "REDACTED") {
		t.Errorf("expect REDACTED marker: %s", got)
	}
}
