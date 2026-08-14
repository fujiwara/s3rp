package sigv4_test

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/fujiwara/s3rp/sigv4"
)

// Regressions for what a security review found in the aws-chunked decoder.

// A consumer stops at x-amz-decoded-content-length — every SDK does, it
// bounds the body by Content-Length. If a chunk may claim more than that,
// its trailing bytes are never read, so its signature is never checked,
// while its leading bytes have already been handed over. The decoder must
// refuse such a chunk at its header, before delivering anything.
func TestChunkedReaderRefusesChunkPastDecodedLength(t *testing.T) {
	vr := awsDocsVerifiedRequest()
	var buf bytes.Buffer
	data := bytes.Repeat([]byte("A"), 100)
	fmt.Fprintf(&buf, "%x;chunk-signature=%s\r\n", len(data), strings.Repeat("0", 64))
	buf.Write(data)
	buf.WriteString("\r\n")

	// the client signed a decoded length of 1 but sends a 100-byte chunk
	r := sigv4.NewChunkedReader(bytes.NewReader(buf.Bytes()), vr, "", 1)
	p := make([]byte, 1)
	n, err := r.Read(p)
	if err == nil {
		t.Fatalf("expect the oversized chunk to be refused, got %d bytes %q", n, p[:n])
	}
	if n != 0 {
		t.Errorf("expect no bytes to be delivered, got %q", p[:n])
	}
	if !strings.Contains(err.Error(), "x-amz-decoded-content-length") {
		t.Errorf("unexpected error: %v", err)
	}
}

// The sum of chunk sizes may not exceed the declared length either.
func TestChunkedReaderRefusesChunksSummingPastDecodedLength(t *testing.T) {
	vr := awsDocsVerifiedRequest()
	data := []byte(strings.Repeat("x", 64))
	encoded := encodeSignedChunks(t, vr, data, 16) // four chunks of 16
	r := sigv4.NewChunkedReader(bytes.NewReader(encoded), vr, "", 48)
	_, err := io.ReadAll(r)
	if err == nil {
		t.Fatal("expect the fourth chunk to be refused")
	}
	if !strings.Contains(err.Error(), "x-amz-decoded-content-length") {
		t.Errorf("unexpected error: %v", err)
	}
}

// A declared trailer checksum that never arrives must fail: accepting the
// upload would claim an integrity check the client never proved.
func TestChunkedReaderRequiresDeclaredTrailer(t *testing.T) {
	vr := awsDocsVerifiedRequest()
	vr.PayloadHash = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
	data := []byte("123456789")

	t.Run("trailer omitted entirely", func(t *testing.T) {
		buf := new(bytes.Buffer)
		fmt.Fprintf(buf, "%x\r\n", len(data))
		buf.Write(data)
		buf.WriteString("\r\n0\r\n\r\n") // final chunk, empty trailer block
		r := sigv4.NewChunkedReader(buf, vr, "crc32", int64(len(data)))
		if _, err := io.ReadAll(r); err == nil || !strings.Contains(err.Error(), "BadDigest") {
			t.Errorf("expect BadDigest for the missing trailer, got %v", err)
		}
	})
	t.Run("a different trailer present", func(t *testing.T) {
		buf := encodeUnsignedTrailer(data, "x-amz-meta-note", "not the checksum")
		r := sigv4.NewChunkedReader(buf, vr, "crc32", int64(len(data)))
		if _, err := io.ReadAll(r); err == nil || !strings.Contains(err.Error(), "BadDigest") {
			t.Errorf("expect BadDigest when the declared trailer is absent, got %v", err)
		}
	})
	t.Run("stream ends without a trailer block", func(t *testing.T) {
		buf := new(bytes.Buffer)
		fmt.Fprintf(buf, "%x\r\n", len(data))
		buf.Write(data)
		buf.WriteString("\r\n0\r\n") // EOF right after the final chunk
		r := sigv4.NewChunkedReader(buf, vr, "crc32", int64(len(data)))
		if _, err := io.ReadAll(r); err == nil || !strings.Contains(err.Error(), "BadDigest") {
			t.Errorf("expect BadDigest at EOF without the trailer, got %v", err)
		}
	})
}

// An unterminated framing line must not grow the buffer without limit: the
// server sets no body read timeout, so a slow client could hold memory
// indefinitely.
func TestChunkedReaderBoundsFramingLines(t *testing.T) {
	vr := awsDocsVerifiedRequest()
	body := bytes.Repeat([]byte("f"), 1<<20) // 1 MiB, no newline
	r := sigv4.NewChunkedReader(bytes.NewReader(body), vr, "", 1<<30)
	_, err := r.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("expect an unterminated line to be refused")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("unexpected error: %v", err)
	}
	// and the refusal must not quote the whole line back into the cause,
	// which the observer logs
	if len(err.Error()) > 200 {
		t.Errorf("error message is %d bytes; a client must not inflate it", len(err.Error()))
	}
}

// A malformed (but terminated) chunk size is quoted back truncated.
func TestChunkedReaderTruncatesQuotedValues(t *testing.T) {
	vr := awsDocsVerifiedRequest()
	var buf bytes.Buffer
	buf.WriteString(strings.Repeat("z", 4096) + "\r\n")
	r := sigv4.NewChunkedReader(&buf, vr, "", 1<<30)
	_, err := r.Read(make([]byte, 1))
	if err == nil {
		t.Fatal("expect a malformed chunk size to be refused")
	}
	if len(err.Error()) > 200 {
		t.Errorf("error message is %d bytes; the value must be truncated", len(err.Error()))
	}
}

// The terminal chunk and the trailers must be verified in the same Read
// that delivers the last payload byte: a consumer stopping at the decoded
// length (every SDK does, it bounds the body by Content-Length) never
// issues the Read after it, so anything deferred to that Read is never
// checked — a truncated stream would pass for a complete upload.
func TestChunkedReaderRequiresTerminalChunk(t *testing.T) {
	t.Run("empty stream, zero decoded length", func(t *testing.T) {
		r := sigv4.NewChunkedReader(bytes.NewReader(nil), awsDocsVerifiedRequest(), "", 0)
		if _, err := io.ReadAll(r); err == nil || !strings.Contains(err.Error(), "IncompleteBody") {
			t.Errorf("expect IncompleteBody for an empty stream, got %v", err)
		}
	})

	t.Run("stream ends after the last data chunk", func(t *testing.T) {
		vr := awsDocsVerifiedRequest()
		data := bytes.Repeat([]byte("a"), 20)
		body := encodeSignedChunks(t, vr, data, 10)
		before, _, ok := bytes.Cut(body, []byte("0;chunk-signature"))
		if !ok {
			t.Fatal("terminator not found")
		}
		r := sigv4.NewChunkedReader(bytes.NewReader(before), vr, "", int64(len(data)))
		if _, err := io.ReadAll(r); err == nil || !strings.Contains(err.Error(), "IncompleteBody") {
			t.Errorf("expect IncompleteBody without the terminal chunk, got %v", err)
		}
	})

	t.Run("consumer stopping at the decoded length sees the truncation", func(t *testing.T) {
		vr := awsDocsVerifiedRequest()
		data := bytes.Repeat([]byte("a"), 20)
		body := encodeSignedChunks(t, vr, data, 10)
		before, _, ok := bytes.Cut(body, []byte("0;chunk-signature"))
		if !ok {
			t.Fatal("terminator not found")
		}
		r := sigv4.NewChunkedReader(bytes.NewReader(before), vr, "", int64(len(data)))
		got, readErr := readUpTo(r, len(data))
		if readErr == nil || !strings.Contains(readErr.Error(), "IncompleteBody") {
			t.Errorf("expect IncompleteBody before the last byte, got %v", readErr)
		}
		if got == len(data) {
			t.Error("the full payload was delivered despite the missing terminal chunk")
		}
	})

	t.Run("trailer checksum verified before the last byte is delivered", func(t *testing.T) {
		vr := awsDocsVerifiedRequest()
		vr.PayloadHash = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
		data := []byte("123456789")
		buf := encodeUnsignedTrailer(data, "x-amz-checksum-crc32", "AAAAAA==") // wrong value
		r := sigv4.NewChunkedReader(buf, vr, "crc32", int64(len(data)))
		got, readErr := readUpTo(r, len(data))
		if readErr == nil || !strings.Contains(readErr.Error(), "BadDigest") {
			t.Errorf("expect BadDigest before the last byte, got %v", readErr)
		}
		if got == len(data) {
			t.Error("the full payload was delivered despite the failed trailer checksum")
		}
	})
}

// readUpTo reads r one byte at a time until limit bytes were delivered or a
// read fails, and never reads past the limit — the consumer shape the SDK
// has when Content-Length bounds the body.
func readUpTo(r io.Reader, limit int) (int, error) {
	buf := make([]byte, 1)
	var got int
	for got < limit {
		n, err := r.Read(buf)
		got += n
		if err != nil {
			return got, err
		}
	}
	return got, nil
}
