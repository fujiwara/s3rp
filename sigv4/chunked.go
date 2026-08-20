package sigv4

import (
	"bufio"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"github.com/fujiwara/s3rp/checksum"
	"github.com/fujiwara/s3rp/s3err"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// aws-chunked content encoding:
// <hex-size>[;chunk-signature=<64 hex>]\r\n<data>\r\n ... 0[;chunk-signature=...]\r\n[trailers]\r\n
// Specified in "Signature Calculations for the Authorization Header:
// Transferring Payload in Multiple Chunks (Chunked Upload)" in the S3 API
// reference (cited by title: AWS's deep links to it rot).

var emptySHA256 = sha256.Sum256(nil)

// deriveSigningKey derives the SigV4 signing key for a credential scope.
// Shared with the POST policy verifier, which signs with the same key.
func deriveSigningKey(secret string, date, region string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), []byte(date))
	k = hmacSHA256(k, []byte(region))
	k = hmacSHA256(k, []byte("s3"))
	return hmacSHA256(k, []byte("aws4_request"))
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

type chunkedReader struct {
	r       *bufio.Reader
	signed  bool
	trailer bool

	// signature chain state (signed only)
	signingKey []byte
	timestamp  string
	scope      string
	prevSig    string
	chunkSig   string // declared signature of the current chunk
	chunkHash  hash.Hash

	// checksum trailer verification (when the request declares one)
	ckAlg  string
	ckHash hash.Hash
	ckSeen bool // the declared checksum trailer was present

	remaining int64 // undelivered bytes of the current chunk
	// undecoded is what the signed x-amz-decoded-content-length still
	// allows. A chunk claiming more than this is refused at its header,
	// before any of its bytes are handed out — otherwise a consumer that
	// stops at the declared length (every SDK does, it bounds the body by
	// Content-Length) would take bytes from a chunk whose signature is
	// never reached, and therefore never checked.
	undecoded int64
	started   bool
	eof       bool
	err       error
}

// incompleteBody is the error for an aws-chunked stream that ends before
// delivering the signed x-amz-decoded-content-length: a truncated stream
// must not read as a complete shorter upload, because the terminal chunk —
// and with it the end of the signature chain — was never verified.
func incompleteBody(cause error) error {
	return s3err.New(http.StatusBadRequest, "IncompleteBody",
		"You did not provide the number of bytes specified by the x-amz-decoded-content-length HTTP header.").WithCause(cause)
}

// maxChunkLineLen bounds a chunk header or trailer line. Headers are about a
// hundred bytes (hex size, "chunk-signature=" and 64 hex digits) and trailers
// are HTTP header lines, so this is generous; without it a client could send
// an unterminated line and make the reader buffer without limit — on a server
// that deliberately sets no body read timeout.
const maxChunkLineLen = 8 * kib

const kib = 1 << 10

// NewChunkedReader returns a reader that decodes an aws-chunked request body.
// When vr's payload hash declares signed chunks, each chunk signature is
// verified against the signature chain seeded by the request signature.
// When trailerAlg names a checksum algorithm (from the x-amz-trailer
// header), the trailer checksum is verified against the decoded payload.
//
// decodedLength is the signed x-amz-decoded-content-length: the decoded
// stream may not exceed it, which is what keeps every delivered byte covered
// by a verified chunk signature.
func NewChunkedReader(body io.Reader, vr *Verified, trailerAlg string, decodedLength int64) io.Reader {
	signed := vr.PayloadHash == streamingSHA256 || vr.PayloadHash == streamingSHA256T
	cr := &chunkedReader{
		r:         bufio.NewReader(body),
		signed:    signed,
		trailer:   strings.HasSuffix(vr.PayloadHash, "-TRAILER"),
		undecoded: decodedLength,
	}
	if signed {
		cr.signingKey = deriveSigningKey(vr.SecretAccessKey, vr.SigningTime.Format("20060102"), vr.Region)
		cr.timestamp = vr.SigningTime.Format(amzDateFormat)
		cr.scope = vr.Scope
		cr.prevSig = vr.Signature
		cr.chunkHash = sha256.New()
	}
	if cr.trailer && trailerAlg != "" {
		cr.ckAlg = trailerAlg
		cr.ckHash = checksum.NewHash(trailerAlg)
	}
	return cr
}

func (cr *chunkedReader) Read(p []byte) (int, error) {
	if cr.err != nil {
		return 0, cr.err
	}
	if cr.eof {
		return 0, io.EOF
	}
	for cr.remaining == 0 {
		if cr.started {
			// the previous chunk was verified as soon as its data completed
			// (below); here we only consume the CRLF that frames it
			if err := cr.readCRLF(); err != nil {
				return 0, cr.fail(err)
			}
		}
		size, err := cr.readChunkHeader()
		if err != nil {
			return 0, cr.fail(err)
		}
		cr.started = true
		// refuse a chunk that claims more than the signed decoded length
		// still allows: its trailing bytes would never be read, so its
		// signature would never be checked, yet its leading bytes would
		// already have been delivered
		if size > cr.undecoded {
			return 0, cr.fail(fmt.Errorf(
				"chunk size %d exceeds the remaining x-amz-decoded-content-length %d", size, cr.undecoded))
		}
		cr.undecoded -= size
		if size == 0 {
			if err := cr.verifyChunk(); err != nil {
				return 0, cr.fail(err)
			}
			// the terminal chunk closes the stream, so anything short of the
			// declared decoded length is a truncated body, not a shorter one
			if cr.undecoded != 0 {
				return 0, cr.fail(incompleteBody(fmt.Errorf(
					"stream complete with %d bytes of x-amz-decoded-content-length undelivered", cr.undecoded)))
			}
			if err := cr.discardTrailers(); err != nil {
				return 0, cr.fail(err)
			}
			cr.eof = true
			return 0, io.EOF
		}
		cr.remaining = size
	}
	if int64(len(p)) > cr.remaining {
		p = p[:cr.remaining]
	}
	n, err := cr.r.Read(p)
	if n > 0 {
		cr.remaining -= int64(n)
		if cr.signed {
			cr.chunkHash.Write(p[:n])
		}
		if cr.ckHash != nil {
			cr.ckHash.Write(p[:n])
		}
	}
	if err == io.EOF && cr.remaining > 0 {
		err = incompleteBody(io.ErrUnexpectedEOF)
	}
	if err != nil && err != io.EOF {
		return n, cr.fail(err)
	}
	// Verify the chunk signature the moment its data is complete, before
	// handing the final bytes to the caller — so integrity does not depend on
	// the consumer reading past the last byte to reach EOF. On failure the
	// just-read bytes are dropped (the stream is aborted).
	if cr.remaining == 0 {
		if verr := cr.verifyChunk(); verr != nil {
			return 0, cr.fail(verr)
		}
		// The declared decoded length is exhausted, so the terminal chunk
		// and the trailers must be consumed and verified now, in the same
		// Read: the consumer is entitled to stop at the decoded length (the
		// SDK bounds the body by ContentLength), so a following Read that
		// would otherwise do this may never come.
		if cr.undecoded == 0 {
			if terr := cr.readTerminal(); terr != nil {
				return 0, cr.fail(terr)
			}
		}
	}
	return n, nil
}

// readTerminal consumes the zero-size terminal chunk and the trailers after
// the last data chunk, verifying both, and marks the stream complete.
func (cr *chunkedReader) readTerminal() error {
	if err := cr.readCRLF(); err != nil {
		return err
	}
	size, err := cr.readChunkHeader()
	if err != nil {
		return err
	}
	if size != 0 {
		return fmt.Errorf("chunk size %d exceeds the remaining x-amz-decoded-content-length 0", size)
	}
	if err := cr.verifyChunk(); err != nil {
		return err
	}
	if err := cr.discardTrailers(); err != nil {
		return err
	}
	cr.eof = true
	return nil
}

// truncateForError bounds a client-controlled value quoted in an error, so
// the cause handed to the observer cannot be inflated by the request.
func truncateForError(s string) string {
	const max = 64
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

func (cr *chunkedReader) fail(err error) error {
	cr.err = err
	return err
}

// readChunkHeader reads "<hex-size>[;chunk-signature=<hex>]\r\n".
func (cr *chunkedReader) readChunkHeader() (int64, error) {
	line, err := cr.readLine()
	if err != nil {
		// a stream ending where a chunk header belongs never reached its
		// terminal chunk; io.EOF here would read as a clean end of body
		if err == io.EOF {
			err = incompleteBody(io.ErrUnexpectedEOF)
		}
		return 0, err
	}
	sizeStr, ext, hasExt := strings.Cut(line, ";")
	size, err := strconv.ParseInt(sizeStr, 16, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("malformed chunk size %q", truncateForError(sizeStr))
	}
	cr.chunkSig = ""
	if hasExt {
		name, value, _ := strings.Cut(ext, "=")
		if name != "chunk-signature" {
			return 0, fmt.Errorf("unknown chunk extension %q", truncateForError(name))
		}
		cr.chunkSig = value
	}
	if cr.signed && cr.chunkSig == "" {
		return 0, fmt.Errorf("missing chunk signature")
	}
	return size, nil
}

// verifyChunk checks the signature of the chunk just consumed and
// advances the signature chain.
func (cr *chunkedReader) verifyChunk() error {
	if !cr.signed {
		return nil
	}
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256-PAYLOAD",
		cr.timestamp,
		cr.scope,
		cr.prevSig,
		hex.EncodeToString(emptySHA256[:]),
		hex.EncodeToString(cr.chunkHash.Sum(nil)),
	}, "\n")
	want := hex.EncodeToString(hmacSHA256(cr.signingKey, []byte(stringToSign)))
	if subtle.ConstantTimeCompare([]byte(want), []byte(cr.chunkSig)) != 1 {
		return s3err.SignatureDoesNotMatch()
	}
	cr.prevSig = cr.chunkSig
	cr.chunkHash.Reset()
	return nil
}

// discardTrailers reads the trailing headers after the final chunk,
// verifying the declared checksum trailer against the decoded payload.
func (cr *chunkedReader) discardTrailers() error {
	if !cr.trailer {
		// optional final CRLF
		cr.readCRLF()
		return nil
	}
	for {
		line, err := cr.readLine()
		if err == io.EOF || (err == nil && line == "") {
			// a declared checksum that never arrived is a checksum not
			// verified; accepting the upload would report integrity the
			// client never proved
			if cr.ckHash != nil && !cr.ckSeen {
				return s3err.New(http.StatusBadRequest, "BadDigest",
					fmt.Sprintf("The x-amz-checksum-%s trailer declared by x-amz-trailer is missing.", cr.ckAlg))
			}
			return nil
		}
		if err != nil {
			return err
		}
		if cr.ckHash == nil {
			continue
		}
		name, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(name), checksum.HeaderPrefix+cr.ckAlg) {
			if strings.TrimSpace(value) != checksum.Base64(cr.ckHash) {
				return s3err.New(http.StatusBadRequest, "BadDigest",
					fmt.Sprintf("The %s you specified did not match the calculated checksum.", strings.ToUpper(cr.ckAlg)))
			}
			cr.ckSeen = true
		}
	}
}

func (cr *chunkedReader) readCRLF() error {
	b := make([]byte, 2)
	if _, err := io.ReadFull(cr.r, b); err != nil {
		if err == io.EOF {
			err = io.ErrUnexpectedEOF
		}
		return incompleteBody(err)
	}
	if b[0] != '\r' || b[1] != '\n' {
		return fmt.Errorf("malformed chunk: expected CRLF")
	}
	return nil
}

// readLine reads one CRLF-terminated framing line, bounded by
// maxChunkLineLen so an unterminated line cannot grow the buffer without
// limit. The bound is on the line, not on a single read, so a slow client
// sending it in pieces is bounded too.
func (cr *chunkedReader) readLine() (string, error) {
	var sb strings.Builder
	for {
		b, err := cr.r.ReadByte()
		if err != nil {
			if err == io.EOF && sb.Len() > 0 {
				return strings.TrimRight(sb.String(), "\r\n"), nil
			}
			return "", err
		}
		if b == '\n' {
			return strings.TrimRight(sb.String(), "\r\n"), nil
		}
		if sb.Len() >= maxChunkLineLen {
			return "", fmt.Errorf("chunk framing line exceeds %d bytes", maxChunkLineLen)
		}
		sb.WriteByte(b)
	}
}
