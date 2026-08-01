package s3rp_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
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

// encodeSignedChunks encodes data as signed aws-chunked with the given chunk size.
func encodeSignedChunks(t *testing.T, vr *s3rp.VerifiedRequest, data []byte, chunkSize int) []byte {
	t.Helper()
	secret := vr.SecretAccessKey
	key := []byte("AWS4" + secret)
	for _, v := range []string{vr.SigningTime.Format("20060102"), vr.Region, "s3", "aws4_request"} {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(v))
		key = mac.Sum(nil)
	}
	emptyHash := sha256.Sum256(nil)
	prevSig := vr.Signature
	buf := new(bytes.Buffer)
	sign := func(chunk []byte) string {
		chunkHash := sha256.Sum256(chunk)
		stringToSign := strings.Join([]string{
			"AWS4-HMAC-SHA256-PAYLOAD",
			vr.SigningTime.Format("20060102T150405Z"),
			vr.Scope,
			prevSig,
			hex.EncodeToString(emptyHash[:]),
			hex.EncodeToString(chunkHash[:]),
		}, "\n")
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(stringToSign))
		sig := hex.EncodeToString(mac.Sum(nil))
		prevSig = sig
		return sig
	}
	for len(data) > 0 {
		n := min(chunkSize, len(data))
		chunk := data[:n]
		data = data[n:]
		fmt.Fprintf(buf, "%x;chunk-signature=%s\r\n", n, sign(chunk))
		buf.Write(chunk)
		buf.WriteString("\r\n")
	}
	fmt.Fprintf(buf, "0;chunk-signature=%s\r\n\r\n", sign(nil))
	return buf.Bytes()
}
