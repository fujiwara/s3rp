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
// https://docs.aws.amazon.com/AmazonS3/latest/API/sigv4-streaming.html

var emptySHA256 = sha256.Sum256(nil)

// deriveSigningKey derives the SigV4 signing key for the scope of vr.
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

	remaining int64 // undelivered bytes of the current chunk
	started   bool
	eof       bool
	err       error
}

// NewChunkedReader returns a reader that decodes an aws-chunked request body.
// When vr's payload hash declares signed chunks, each chunk signature is
// verified against the signature chain seeded by the request signature.
// When trailerAlg names a checksum algorithm (from the x-amz-trailer
// header), the trailer checksum is verified against the decoded payload.
func NewChunkedReader(body io.Reader, vr *Verified, trailerAlg string) io.Reader {
	signed := vr.PayloadHash == streamingSHA256 || vr.PayloadHash == streamingSHA256T
	cr := &chunkedReader{
		r:       bufio.NewReader(body),
		signed:  signed,
		trailer: strings.HasSuffix(vr.PayloadHash, "-TRAILER"),
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
		if size == 0 {
			if err := cr.verifyChunk(); err != nil {
				return 0, cr.fail(err)
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
		err = io.ErrUnexpectedEOF
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
	}
	return n, nil
}

func (cr *chunkedReader) fail(err error) error {
	cr.err = err
	return err
}

// readChunkHeader reads "<hex-size>[;chunk-signature=<hex>]\r\n".
func (cr *chunkedReader) readChunkHeader() (int64, error) {
	line, err := cr.readLine()
	if err != nil {
		return 0, err
	}
	sizeStr, ext, hasExt := strings.Cut(line, ";")
	size, err := strconv.ParseInt(sizeStr, 16, 64)
	if err != nil || size < 0 {
		return 0, fmt.Errorf("malformed chunk size %q", sizeStr)
	}
	cr.chunkSig = ""
	if hasExt {
		name, value, _ := strings.Cut(ext, "=")
		if name != "chunk-signature" {
			return 0, fmt.Errorf("unknown chunk extension %q", name)
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
		if strings.EqualFold(strings.TrimSpace(name), "x-amz-checksum-"+cr.ckAlg) {
			if strings.TrimSpace(value) != checksum.Base64(cr.ckHash) {
				return s3err.New(http.StatusBadRequest, "BadDigest",
					fmt.Sprintf("The %s you specified did not match the calculated checksum.", strings.ToUpper(cr.ckAlg)))
			}
		}
	}
}

func (cr *chunkedReader) readCRLF() error {
	b := make([]byte, 2)
	if _, err := io.ReadFull(cr.r, b); err != nil {
		return err
	}
	if b[0] != '\r' || b[1] != '\n' {
		return fmt.Errorf("malformed chunk: expected CRLF")
	}
	return nil
}

func (cr *chunkedReader) readLine() (string, error) {
	line, err := cr.r.ReadString('\n')
	if err != nil && !(err == io.EOF && line != "") {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
