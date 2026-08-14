package sigv4_test

import (
	"bytes"
	"io"
	"testing"

	"github.com/fujiwara/s3rp/checksum"
	"github.com/fujiwara/s3rp/sigv4"
)

// The chunked decoder consumes attacker-controlled bytes before the request
// is authorized, so these targets assert its security properties rather than
// just the absence of panics:
//
//   - Raw: arbitrary input never panics, never hangs, and an accepted stream
//     never delivers more than the declared decoded length.
//   - Roundtrip: a correctly signed stream decodes to its payload, and no
//     single-bit corruption of the stream is accepted with a different
//     payload (accepted ⇒ intact).
//
// "Accepted with the payload unchanged" is allowed on mutation: a flip can
// land in redundant framing (the optional final CRLF, the case of a trailer
// name) without weakening integrity.

// fuzzPayloadCap bounds the plaintext so a worst-case chunking (1-byte
// chunks, one HMAC each) stays fast enough for the fuzzer.
const fuzzPayloadCap = 4 * 1024

func FuzzChunkedReaderRaw(f *testing.F) {
	f.Add(awsDocsChunkedBody(), uint8(0), int64(1<<30))
	f.Add(awsDocsChunkedBody(), uint8(0), int64(66560))
	f.Add(awsDocsChunkedBody(), uint8(0), int64(3))
	f.Add(encodeUnsignedTrailer([]byte("123456789"), "x-amz-checksum-crc32", "y/Q5Jg==").Bytes(), uint8(2), int64(9))
	f.Add(encodeUnsignedTrailer([]byte("123456789"), "x-amz-checksum-crc32", "y/Q5Jg==").Bytes(), uint8(3), int64(9))
	f.Add([]byte("0\r\n\r\n"), uint8(0), int64(0))
	f.Add([]byte(""), uint8(1), int64(0))
	f.Add([]byte("ffffffffffffffff;chunk-signature=00\r\n"), uint8(0), int64(1<<62))

	f.Fuzz(func(t *testing.T, body []byte, mode uint8, decodedLen int64) {
		vr := awsDocsVerifiedRequest()
		alg := ""
		switch mode % 4 {
		case 0: // signed, no trailer
		case 1:
			vr.PayloadHash = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
			alg = "crc32"
		case 2:
			vr.PayloadHash = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
			alg = "crc32"
		case 3:
			vr.PayloadHash = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
			// x-amz-trailer absent: trailer lines are read but not verified
		}
		r := sigv4.NewChunkedReader(bytes.NewReader(body), vr, alg, decodedLen)
		decoded, err := io.ReadAll(r)
		if err == nil && int64(len(decoded)) != decodedLen {
			t.Fatalf("accepted stream delivered %d bytes, declared decoded length is %d",
				len(decoded), decodedLen)
		}
	})
}

func FuzzChunkedReaderRoundtrip(f *testing.F) {
	f.Add([]byte("0123456789abcdef"), 7, 0, uint8(0))
	f.Add([]byte(""), 1, 0, uint8(0))
	f.Add(bytes.Repeat([]byte("a"), 2048), 1000, 30, uint8(3))

	f.Fuzz(func(t *testing.T, payload []byte, chunkSize, mutPos int, mutBit uint8) {
		if len(payload) > fuzzPayloadCap {
			payload = payload[:fuzzPayloadCap]
		}
		if chunkSize < 1 {
			chunkSize = 1
		}
		vr := awsDocsVerifiedRequest()
		enc := encodeSignedChunks(t, vr, payload, chunkSize)

		// completeness: the intact stream decodes to the payload, with the
		// decoded length declared exactly
		r := sigv4.NewChunkedReader(bytes.NewReader(enc), vr, "", int64(len(payload)))
		decoded, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("intact stream refused: %v", err)
		}
		if !bytes.Equal(decoded, payload) {
			t.Fatalf("intact stream decoded to %d bytes, want %d", len(decoded), len(payload))
		}

		// soundness: no single-bit corruption may be accepted with a
		// different payload
		mut := bytes.Clone(enc)
		pos := mutPos % len(mut)
		if pos < 0 {
			pos += len(mut)
		}
		mut[pos] ^= 1 << (mutBit % 8)
		r = sigv4.NewChunkedReader(bytes.NewReader(mut), vr, "", int64(len(payload)))
		decoded, err = io.ReadAll(r)
		if err == nil && !bytes.Equal(decoded, payload) {
			t.Fatalf("stream with bit %d of byte %d flipped accepted with altered payload", mutBit%8, pos)
		}

		// and no truncation may be accepted with anything but the full
		// payload — the terminal chunk authenticates the end of the stream
		r = sigv4.NewChunkedReader(bytes.NewReader(enc[:pos]), vr, "", int64(len(payload)))
		decoded, err = io.ReadAll(r)
		if err == nil && !bytes.Equal(decoded, payload) {
			t.Fatalf("stream truncated to %d of %d bytes accepted with altered payload", pos, len(enc))
		}
	})
}

func FuzzChunkedReaderTrailerRoundtrip(f *testing.F) {
	f.Add([]byte("123456789"), uint8(0), 0, uint8(0))
	f.Add([]byte("123456789"), uint8(1), 20, uint8(5))
	f.Add([]byte(""), uint8(2), 0, uint8(0))

	f.Fuzz(func(t *testing.T, payload []byte, algPick uint8, mutPos int, mutBit uint8) {
		if len(payload) > fuzzPayloadCap {
			payload = payload[:fuzzPayloadCap]
		}
		alg := []string{"crc32", "crc32c", "crc64nvme", "sha1", "sha256"}[int(algPick)%5]
		h := checksum.NewHash(alg)
		h.Write(payload)
		enc := encodeUnsignedTrailer(payload, "x-amz-checksum-"+alg, checksum.Base64(h)).Bytes()
		vr := awsDocsVerifiedRequest()
		vr.PayloadHash = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"

		r := sigv4.NewChunkedReader(bytes.NewReader(enc), vr, alg, int64(len(payload)))
		decoded, err := io.ReadAll(r)
		if err != nil {
			t.Fatalf("intact stream refused: %v", err)
		}
		if !bytes.Equal(decoded, payload) {
			t.Fatalf("intact stream decoded to %d bytes, want %d", len(decoded), len(payload))
		}

		mut := bytes.Clone(enc)
		pos := mutPos % len(mut)
		if pos < 0 {
			pos += len(mut)
		}
		mut[pos] ^= 1 << (mutBit % 8)
		r = sigv4.NewChunkedReader(bytes.NewReader(mut), vr, alg, int64(len(payload)))
		decoded, err = io.ReadAll(r)
		if err == nil && !bytes.Equal(decoded, payload) {
			t.Fatalf("stream with bit %d of byte %d flipped accepted with altered payload", mutBit%8, pos)
		}

		r = sigv4.NewChunkedReader(bytes.NewReader(enc[:pos]), vr, alg, int64(len(payload)))
		decoded, err = io.ReadAll(r)
		if err == nil && !bytes.Equal(decoded, payload) {
			t.Fatalf("stream truncated to %d of %d bytes accepted with altered payload", pos, len(enc))
		}
	})
}
