package s3gw_test

import (
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// The unsigned-x-amz-* gate, exercised through the full gateway stack
// (Gateway.Handler), not just sigv4.Verify. hdr.Signed treats an uncovered
// header as absent, which is only safe because a request carrying one never
// reaches a handler: were the gate to regress, an unsigned precondition or
// object-lock header would be silently dropped — the "client believes it is
// protected" failure mode. These pin the wiring from the HTTP server down.
func TestUnsignedAmzHeaderRefusedEndToEnd(t *testing.T) {
	t.Run("presigned PUT with object-lock header", func(t *testing.T) {
		stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"etag"`)}}
		client, _ := newTestProxy(t, stub)
		presigner := s3.NewPresignClient(client)
		presigned, err := presigner.PresignPutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("testbucket"),
			Key:    aws.String("locked.txt"),
		})
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(http.MethodPut, presigned.URL, strings.NewReader("data"))
		if err != nil {
			t.Fatal(err)
		}
		// the URL grantor never signed a retention; the holder must not be
		// able to attach one
		req.Header.Set("x-amz-object-lock-mode", "COMPLIANCE")
		req.Header.Set("x-amz-object-lock-retain-until-date", time.Now().AddDate(1, 0, 0).Format(time.RFC3339))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "AccessDenied") {
			t.Fatalf("expect 403 AccessDenied, got %d: %s", resp.StatusCode, body)
		}
		if stub.putIn != nil {
			t.Error("the backend must never see a request refused by the gate")
		}
	})
	t.Run("header-signed DELETE with unsigned precondition", func(t *testing.T) {
		stub := &stubBackend{delOut: &s3.DeleteObjectOutput{}}
		_, ts := newTestProxy(t, stub)
		req, err := http.NewRequest(http.MethodDelete, ts.URL+"/testbucket/protected.txt", nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Amz-Content-Sha256", emptyPayloadHash)
		signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
		if err := signer.SignHTTP(t.Context(), testCreds(), req, emptyPayloadHash, "s3", "us-east-1", time.Now()); err != nil {
			t.Fatal(err)
		}
		// added after signing: honoring it would apply an unauthenticated
		// precondition, dropping it would delete unconditionally while the
		// client believes it is protected — refusal is the only sound answer
		req.Header.Set("x-amz-if-match-size", "4")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusForbidden || !strings.Contains(string(body), "AccessDenied") {
			t.Fatalf("expect 403 AccessDenied, got %d: %s", resp.StatusCode, body)
		}
		if stub.delIn != nil {
			t.Error("the backend must never see a request refused by the gate")
		}
	})
}
