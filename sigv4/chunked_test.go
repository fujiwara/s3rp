package sigv4_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/fujiwara/s3rp/sigv4"
)

// Known-answer test vector from the AWS documentation.
// https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html
func awsDocsVerifiedRequest() *sigv4.Verified {
	return &sigv4.Verified{
		AccessKeyID:     "AKIAIOSFODNN7EXAMPLE",
		SecretAccessKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		Signature:       "4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9",
		SigningTime:     time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC),
		Scope:           "20130524/us-east-1/s3/aws4_request",
		Region:          "us-east-1",
		PayloadHash:     "STREAMING-AWS4-HMAC-SHA256-PAYLOAD",
	}
}

func awsDocsChunkedBody() []byte {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "10000;chunk-signature=ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648\r\n")
	buf.Write(bytes.Repeat([]byte("a"), 65536))
	buf.WriteString("\r\n")
	fmt.Fprintf(buf, "400;chunk-signature=0055627c9e194cb4542bae2aa5492e3c1575bbb81b612b7d234b86a503ef5497\r\n")
	buf.Write(bytes.Repeat([]byte("a"), 1024))
	buf.WriteString("\r\n")
	fmt.Fprintf(buf, "0;chunk-signature=b6c6ea8a5354eaf15b3cb7646744f4275b71ea724fed81ceb9323e279d449df9\r\n")
	buf.WriteString("\r\n")
	return buf.Bytes()
}

func TestChunkedReaderAWSDocsVector(t *testing.T) {
	r := sigv4.NewChunkedReader(bytes.NewReader(awsDocsChunkedBody()), awsDocsVerifiedRequest(), "")
	decoded, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(decoded) != 66560 {
		t.Errorf("expect 66560 bytes, got %d", len(decoded))
	}
	if !bytes.Equal(decoded, bytes.Repeat([]byte("a"), 66560)) {
		t.Error("decoded content mismatch")
	}
}

func TestChunkedReaderBadSignature(t *testing.T) {
	body := awsDocsChunkedBody()
	// corrupt the first chunk signature
	corrupted := bytes.Replace(body, []byte("ad80c730"), []byte("deadbeef"), 1)
	r := sigv4.NewChunkedReader(bytes.NewReader(corrupted), awsDocsVerifiedRequest(), "")
	_, err := io.ReadAll(r)
	if err == nil {
		t.Fatal("expect error for bad chunk signature")
	}
	if !strings.Contains(err.Error(), "SignatureDoesNotMatch") {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestChunkedReaderTamperedData(t *testing.T) {
	body := awsDocsChunkedBody()
	// flip a payload byte without touching the signatures
	i := bytes.IndexByte(body, 'a')
	body[i] = 'b'
	r := sigv4.NewChunkedReader(bytes.NewReader(body), awsDocsVerifiedRequest(), "")
	_, err := io.ReadAll(r)
	if err == nil {
		t.Fatal("expect error for tampered chunk data")
	}
}

// encodeSignedChunks encodes data as signed aws-chunked with the given chunk size.
func encodeSignedChunks(t *testing.T, vr *sigv4.Verified, data []byte, chunkSize int) []byte {
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

func TestChunkedReaderRoundTrip(t *testing.T) {
	vr := awsDocsVerifiedRequest()
	for _, size := range []int{1, 7, 1000, 65536} {
		t.Run(fmt.Sprintf("chunk size %d", size), func(t *testing.T) {
			data := []byte(strings.Repeat("0123456789abcdef", 100)) // 1600 bytes
			encoded := encodeSignedChunks(t, vr, data, size)
			r := sigv4.NewChunkedReader(bytes.NewReader(encoded), vr, "")
			decoded, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(decoded, data) {
				t.Error("round trip mismatch")
			}
		})
	}
}

func TestChunkedReaderUnsignedTrailer(t *testing.T) {
	vr := awsDocsVerifiedRequest()
	vr.PayloadHash = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	data := []byte("hello, unsigned chunked world")
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "%x\r\n", len(data))
	buf.Write(data)
	buf.WriteString("\r\n")
	buf.WriteString("0\r\n")
	buf.WriteString("x-amz-checksum-crc32:AAAAAA==\r\n")
	buf.WriteString("\r\n")
	// no trailer algorithm declared: the (bogus) checksum is not verified
	r := sigv4.NewChunkedReader(buf, vr, "")
	decoded, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(decoded, data) {
		t.Errorf("expect %q, got %q", data, decoded)
	}
}

// encodeUnsignedTrailer encodes data as unsigned aws-chunked with a
// checksum trailer.
func encodeUnsignedTrailer(data []byte, trailerName, trailerValue string) *bytes.Buffer {
	buf := new(bytes.Buffer)
	fmt.Fprintf(buf, "%x\r\n", len(data))
	buf.Write(data)
	buf.WriteString("\r\n0\r\n")
	fmt.Fprintf(buf, "%s:%s\r\n\r\n", trailerName, trailerValue)
	return buf
}

func TestChunkedReaderTrailerChecksum(t *testing.T) {
	vr := awsDocsVerifiedRequest()
	vr.PayloadHash = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	// crc32("123456789") = 0xCBF43926 -> base64 of big-endian bytes
	data := []byte("123456789")
	const goodCRC32 = "y/Q5Jg=="

	t.Run("valid checksum", func(t *testing.T) {
		buf := encodeUnsignedTrailer(data, "x-amz-checksum-crc32", goodCRC32)
		r := sigv4.NewChunkedReader(buf, vr, "crc32")
		decoded, err := io.ReadAll(r)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(decoded, data) {
			t.Errorf("expect %q, got %q", data, decoded)
		}
	})
	t.Run("checksum mismatch", func(t *testing.T) {
		buf := encodeUnsignedTrailer(data, "x-amz-checksum-crc32", "AAAAAA==")
		r := sigv4.NewChunkedReader(buf, vr, "crc32")
		_, err := io.ReadAll(r)
		if err == nil {
			t.Fatal("expect error for checksum mismatch")
		}
		if !strings.Contains(err.Error(), "BadDigest") {
			t.Errorf("unexpected error: %v", err)
		}
	})
	t.Run("sha256 roundtrip", func(t *testing.T) {
		sum := sha256.Sum256(data)
		buf := encodeUnsignedTrailer(data, "x-amz-checksum-sha256", base64.StdEncoding.EncodeToString(sum[:]))
		r := sigv4.NewChunkedReader(buf, vr, "sha256")
		if _, err := io.ReadAll(r); err != nil {
			t.Fatal(err)
		}
	})
}
