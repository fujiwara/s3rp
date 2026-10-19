package s3gw_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/sigv4"
)

// testSigningKey derives a SigV4 signing key independently of the package,
// so the fixtures do not take the derivation under test on trust.
func testSigningKey(secret, date, region string) []byte {
	key := []byte("AWS4" + secret)
	for _, v := range []string{date, region, "s3", "aws4_request"} {
		mac := hmac.New(sha256.New, key)
		mac.Write([]byte(v))
		key = mac.Sum(nil)
	}
	return key
}

// encodeSignedChunks encodes data as signed aws-chunked with the given chunk size.
func encodeSignedChunks(t *testing.T, vr *sigv4.Verified, data []byte, chunkSize int) []byte {
	t.Helper()
	key := vr.SigningKey
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
