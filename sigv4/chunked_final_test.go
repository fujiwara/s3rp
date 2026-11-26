package sigv4_test

import (
	"bytes"
	"errors"
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
		r := sigv4.NewChunkedReader(bytes.NewReader(append([]byte(nil), body...)), vr, "", int64(len(data)))
		if _, err := io.ReadAll(r); err == nil {
			t.Error("expect error for tampered final chunk")
		}
	})

	t.Run("read exactly decoded length then stop", func(t *testing.T) {
		r := sigv4.NewChunkedReader(bytes.NewReader(append([]byte(nil), body...)), vr, "", int64(len(data)))
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

// errWithBytes hands out its pieces one per Read, and the last of them
// together with an error, as an io.Reader may: a body paced by a limiter
// does when the client leaves.
type errWithBytes struct {
	pieces [][]byte
	err    error
}

func (r *errWithBytes) Read(p []byte) (int, error) {
	if len(r.pieces) == 0 {
		return 0, r.err
	}
	n := copy(p, r.pieces[0])
	if r.pieces[0] = r.pieces[0][n:]; len(r.pieces[0]) > 0 {
		return n, nil
	}
	if r.pieces = r.pieces[1:]; len(r.pieces) > 0 {
		return n, nil
	}
	return n, r.err
}

// TestChunkedReaderReadErrorDeliversNothing checks that the bytes that came
// with a read error are not handed out. They may be the chunk's last: a
// consumer that has the declared length by them never looks at the error
// (io.ReadFull does not), and would hold a chunk nobody verified.
func TestChunkedReaderReadErrorDeliversNothing(t *testing.T) {
	vr := awsDocsVerifiedRequest()
	// one chunk, larger than bufio's buffer: its data is then read straight
	// from the source, so the error that comes with it is seen with it
	data := bytes.Repeat([]byte("a"), 8192)
	body := encodeSignedChunks(t, vr, data, len(data))
	head := bytes.Index(body, []byte("\r\n")) + 2
	// an altered byte, and the stream failing right after the data
	body[head] = 'b'
	broken := errors.New("connection reset")
	src := &errWithBytes{pieces: [][]byte{body[:head], body[head : head+len(data)]}, err: broken}

	r := sigv4.NewChunkedReader(src, vr, "", int64(len(data)))
	buf := make([]byte, len(data))
	n, err := io.ReadFull(r, buf)
	if n == len(data) {
		t.Errorf("the consumer holds all %d declared bytes, of a chunk nobody verified", n)
	}
	if !errors.Is(err, broken) {
		t.Errorf("expect the read error, got %v", err)
	}
	if n, err := r.Read(buf); n != 0 || !errors.Is(err, broken) {
		t.Errorf("expect the refusal to stay, got %d, %v", n, err)
	}
}
