package sigv4_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/fujiwara/s3rp/sigv4"
)

// signedRequestRegion is signedRequest with the signing region chosen by the
// test.
func signedRequestRegion(t *testing.T, region string) *http.Request {
	t.Helper()
	const url = "http://s3.example.com/bucket/key.txt"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-Amz-Content-Sha256", emptyPayload)
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecret}
	if err := signer.SignHTTP(context.Background(), creds, req, emptyPayload, "s3", region, testTime); err != nil {
		t.Fatal(err)
	}
	sr := httptest.NewRequest("GET", url, nil)
	sr.Header = req.Header
	sr.Host = req.Host
	return sr
}

// presignedRequestRegion builds a server-side request for a presigned URL
// signed for the given region.
func presignedRequestRegion(t *testing.T, region string) *http.Request {
	t.Helper()
	// X-Amz-Expires is in the query before signing, as the SDK presigner
	// adds it: it is part of the canonical request
	const url = "http://s3.example.com/bucket/key.txt?X-Amz-Expires=300"
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatal(err)
	}
	signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
	creds := aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecret}
	signedURI, _, err := signer.PresignHTTP(context.Background(), creds, req, "UNSIGNED-PAYLOAD", "s3", region, testTime)
	if err != nil {
		t.Fatal(err)
	}
	sr := httptest.NewRequest("GET", signedURI, nil)
	sr.Host = req.Host
	return sr
}

func TestVerifyRegionUnpinnedAcceptsAny(t *testing.T) {
	for _, region := range []string{"us-east-1", "ap-northeast-1", "moon-crater-7"} {
		if _, err := newVerifier().Verify(signedRequestRegion(t, region), lookup); err != nil {
			t.Errorf("region %s: expect success without a pinned region, got %v", region, err)
		}
	}
}

func TestVerifyRegionPinned(t *testing.T) {
	pinned := func() *sigv4.Verifier {
		v := sigv4.NewVerifier()
		v.Now = func() time.Time { return testTime }
		v.SetRegion("ap-northeast-1")
		return v
	}

	t.Run("matching region verifies", func(t *testing.T) {
		if _, err := pinned().Verify(signedRequestRegion(t, "ap-northeast-1"), lookup); err != nil {
			t.Errorf("expect success, got %v", err)
		}
		if _, err := pinned().Verify(presignedRequestRegion(t, "ap-northeast-1"), lookup); err != nil {
			t.Errorf("presigned: expect success, got %v", err)
		}
	})

	t.Run("header auth with the wrong region", func(t *testing.T) {
		_, err := pinned().Verify(signedRequestRegion(t, "us-east-1"), lookup)
		if err == nil {
			t.Fatal("expect an error")
		}
		if err.Code != "AuthorizationHeaderMalformed" {
			t.Errorf("expect AuthorizationHeaderMalformed, got %s", err.Code)
		}
		// the message must name the expected region, as AWS does, so a
		// misdirected client can be fixed from its own error output
		if !strings.Contains(err.Message, "the region 'us-east-1' is wrong; expecting 'ap-northeast-1'") {
			t.Errorf("unexpected message %q", err.Message)
		}
	})

	t.Run("presigned URL with the wrong region", func(t *testing.T) {
		_, err := pinned().Verify(presignedRequestRegion(t, "us-east-1"), lookup)
		if err == nil {
			t.Fatal("expect an error")
		}
		if err.Code != "AuthorizationQueryParametersError" {
			t.Errorf("expect AuthorizationQueryParametersError, got %s", err.Code)
		}
		if !strings.Contains(err.Message, "the region 'us-east-1' is wrong; expecting 'ap-northeast-1'") {
			t.Errorf("unexpected message %q", err.Message)
		}
	})
}
