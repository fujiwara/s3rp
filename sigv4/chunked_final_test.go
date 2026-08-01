package sigv4_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/fujiwara/s3rp/sigv4"
)

// TestChunkedReaderFinalChunkVerifiedEagerly checks that a tampered final
// chunk is rejected even when the consumer reads exactly the decoded length
// and stops, rather than reading through to EOF. Verification must not depend
// on the consumer reading past the last payload byte.
func TestChunkedReaderFinalChunkVerifiedEagerly(t *testing.T) {
	vr := awsDocsVerifiedRequest()
	data := bytes.Repeat([]byte("a"), 20)
	body := encodeSignedChunks(t, vr, data, 10) // two 10-byte chunks
	// flip the last payload byte of the final chunk (just before "\r\n0;")
	term := bytes.Index(body, []byte("\r\n0;chunk-signature"))
	if term < 1 {
		t.Fatal("terminator not found")
	}
	body[term-1] = 'b'

	t.Run("read to EOF", func(t *testing.T) {
		r := sigv4.NewChunkedReader(bytes.NewReader(append([]byte(nil), body...)), vr, "")
		if _, err := io.ReadAll(r); err == nil {
			t.Error("expect error for tampered final chunk")
		}
	})

	t.Run("read exactly decoded length then stop", func(t *testing.T) {
		r := sigv4.NewChunkedReader(bytes.NewReader(append([]byte(nil), body...)), vr, "")
		buf := make([]byte, 1)
		var got int
		var readErr error
		for got < len(data) {
			n, err := r.Read(buf)
			got += n
			if err != nil {
				readErr = err
				break
			}
		}
		if readErr == nil {
			t.Error("expect the tampered final chunk to be rejected without reading to EOF")
		}
	})
}
