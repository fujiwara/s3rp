package localbackend

import (
	"bytes"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/fujiwara/s3rp/checksum"
)

// blob is bytes stored once and never changed: the body of one PutObject
// or UploadPart. A Backend made by New holds them in memory, one made by
// Open as a file each.
type blob struct {
	// id names the file under the blob directory; empty in memory
	id   string
	size int64
	mem  []byte
	path string
	// readers counts the response bodies still reading the file, which
	// keeps it from being removed under them
	readers atomic.Int32
	// state is where a blob kept in a file is in its life; guarded by the
	// Backend's blobMu, and unused in memory
	state blobState
}

// blobState is the life of a blob file.
type blobState int

const (
	// blobNew: not in the books yet
	blobNew blobState = iota
	// blobPending: written by a write still in progress, which nothing
	// refers to yet and which is not to be collected for that
	blobPending
	// blobStored: its write is over, and the state may refer to it
	blobStored
	// blobGarbage: the state stopped referring to it, for good; the file is
	// removed once meta.json no longer names it and no response reads it
	blobGarbage
	// blobRemoved: the file is gone
	blobRemoved
	numBlobStates
)

func (s blobState) String() string {
	return [...]string{"new", "pending", "stored", "garbage", "removed"}[s]
}

// blobEvent is what happens to a blob file.
type blobEvent int

const (
	// blobIngested: a write stored its body (ingest)
	blobIngested blobEvent = iota
	// blobRestored: Open found the file, and meta.json names it
	blobRestored
	// blobCommitted, blobAbandoned: the write ended, having made the state
	// refer to the blob or not (release)
	blobCommitted
	blobAbandoned
	// blobDropped: a flush found the state no longer referring to it
	blobDropped
	// blobCollected: a flush removed the file
	blobCollected
	numBlobEvents
)

func (e blobEvent) String() string {
	return [...]string{"ingested", "restored", "committed", "abandoned", "dropped", "collected"}[e]
}

// blobTransitions is a blob file's life; a zero entry is an event that does
// not happen in that state, and no event leads back to blobNew. It is held
// to tla/BlobStore.tla, which is checked for what the directory promises
// whenever the process stops: every blob meta.json names is there
// (TestBlobTable).
var blobTransitions = [numBlobStates][numBlobEvents]blobState{
	blobNew:     {blobIngested: blobPending, blobRestored: blobStored},
	blobPending: {blobCommitted: blobStored, blobAbandoned: blobRemoved},
	blobStored:  {blobDropped: blobGarbage},
	blobGarbage: {blobCollected: blobRemoved},
}

// advance moves the blob as the table says, and reports whether the event
// is one that happens in its state; the blob stays as it is otherwise. The
// caller holds blobMu.
func (bl *blob) advance(e blobEvent) bool {
	next := blobTransitions[bl.state][e]
	if next == blobNew {
		return false
	}
	bl.state = next
	return true
}

// segment is a range of a blob.
type segment struct {
	blob *blob
	off  int64
	size int64
}

// content is what an object or a part consists of: ranges of blobs, in
// order. An upload is one whole blob; a completed multipart upload is its
// parts' content end to end and a copy is its source's, so neither copies
// a byte.
type content []segment

func (c content) size() int64 {
	var n int64
	for _, s := range c {
		n += s.size
	}
	return n
}

// slice is the n bytes of c from off.
func (c content) slice(off, n int64) content {
	var out content
	for _, s := range c {
		if n <= 0 {
			break
		}
		if off >= s.size {
			off -= s.size
			continue
		}
		take := min(s.size-off, n)
		out = append(out, segment{blob: s.blob, off: s.off + off, size: take})
		off, n = 0, n-take
	}
	return out
}

// open returns a reader of c that stays valid after the lock it was opened
// under is released: until it is closed, the files it has yet to read are
// not removed.
func (c content) open() io.ReadCloser {
	for _, s := range c {
		s.blob.readers.Add(1)
	}
	return &contentReader{rest: c, held: c}
}

type contentReader struct {
	rest content
	held content
	cur  io.Reader
	file *os.File
}

func (r *contentReader) Read(p []byte) (int, error) {
	for {
		if r.cur == nil {
			if len(r.rest) == 0 {
				return 0, io.EOF
			}
			s := r.rest[0]
			r.rest = r.rest[1:]
			if s.blob.path == "" {
				r.cur = bytes.NewReader(s.blob.mem[s.off : s.off+s.size])
			} else {
				f, err := os.Open(s.blob.path)
				if err != nil {
					return 0, err
				}
				r.file = f
				r.cur = io.NewSectionReader(f, s.off, s.size)
			}
		}
		n, err := r.cur.Read(p)
		if err == io.EOF {
			r.closeCurrent()
			if n > 0 {
				return n, nil
			}
			continue
		}
		return n, err
	}
}

func (r *contentReader) closeCurrent() {
	if r.file != nil {
		r.file.Close()
		r.file = nil
	}
	r.cur = nil
}

func (r *contentReader) Close() error {
	r.closeCurrent()
	for _, s := range r.held {
		s.blob.readers.Add(-1)
	}
	r.held, r.rest = nil, nil
	return nil
}

// writeTo feeds c to w, for a digest of content already stored. The caller
// holds the write lock, under which nothing is removed.
func (c content) writeTo(w io.Writer) error {
	r := &contentReader{rest: c}
	defer r.closeCurrent()
	_, err := io.Copy(w, r)
	return err
}

// ingest stores what r yields as a new blob, feeding w the same bytes. The
// read error is returned as it is: the gateway's body readers fail with the
// S3 error the client must see, and the gateway finds it in the chain.
//
// A blob kept in a file is pending until release is called for it: not yet
// referenced by anything, and not to be collected for that.
func (b *Backend) ingest(r io.Reader, w io.Writer) (*blob, error) {
	if r == nil {
		r = strings.NewReader("")
	}
	if b.dir == "" {
		var buf bytes.Buffer
		if _, err := io.Copy(io.MultiWriter(&buf, w), r); err != nil {
			return nil, fmt.Errorf("failed to read the request body: %w", err)
		}
		return &blob{size: int64(buf.Len()), mem: buf.Bytes()}, nil
	}
	f, err := os.CreateTemp(b.blobDir(), tempPrefix)
	if err != nil {
		return nil, internalError(err)
	}
	n, err := io.Copy(io.MultiWriter(f, w), r)
	if cerr := f.Close(); err == nil && cerr != nil {
		// a failed write, unlike a failed read, is this package's
		return nil, errors.Join(internalError(cerr), os.Remove(f.Name()))
	}
	if err != nil {
		os.Remove(f.Name())
		// io.Copy does not say which side failed; the body's own errors
		// are S3 errors and are found in the chain either way
		return nil, fmt.Errorf("failed to store the request body: %w", err)
	}
	bl := &blob{id: newID(), size: n}
	bl.path = filepath.Join(b.blobDir(), bl.id)
	// in the books, as pending, before the file has its name, so that a
	// collection in between cannot take it for garbage
	b.blobMu.Lock()
	bl.advance(blobIngested)
	b.known[bl.id] = bl
	b.blobMu.Unlock()
	if err := os.Rename(f.Name(), bl.path); err != nil {
		b.release(bl, false)
		os.Remove(f.Name())
		return nil, internalError(err)
	}
	return bl, nil
}

// release ends a blob's pending state once the write it belongs to is
// over. A blob the write did not come to reference is removed at once.
func (b *Backend) release(bl *blob, committed bool) {
	if bl == nil || bl.id == "" {
		return
	}
	b.blobMu.Lock()
	if committed {
		bl.advance(blobCommitted)
	} else if bl.advance(blobAbandoned) {
		delete(b.known, bl.id)
	}
	b.blobMu.Unlock()
	if !committed {
		os.Remove(bl.path)
	}
}

func internalError(cause error) error {
	return errors.Join(fail(http.StatusInternalServerError, "InternalError",
		"We encountered an internal error. Please try again.").err(), cause)
}

// digests computes, over one pass of some bytes, the MD5 an ETag is made of
// and the checksums of the named algorithms.
type digests struct {
	md5  hash.Hash
	sums map[string]hash.Hash
	w    io.Writer
}

// newDigests refuses an algorithm that cannot be computed: its value would
// be stored unverified, or the algorithm silently not applied.
func newDigests(names ...string) (*digests, *failure) {
	d := &digests{md5: md5.New(), sums: map[string]hash.Hash{}}
	writers := []io.Writer{d.md5}
	for _, name := range names {
		name = strings.ToUpper(name)
		if name == "" || d.sums[name] != nil {
			continue
		}
		h := checksum.NewHash(name)
		if h == nil {
			return nil, unsupportedChecksum(name)
		}
		d.sums[name] = h
		writers = append(writers, h)
	}
	d.w = io.MultiWriter(writers...)
	return d, nil
}

func (d *digests) Write(p []byte) (int, error) { return d.w.Write(p) }

// etag is the MD5 as S3 sends it: hex, quoted.
func (d *digests) etag() string {
	return `"` + hex.EncodeToString(d.md5.Sum(nil)) + `"`
}

func (d *digests) values() checksum.Values {
	if len(d.sums) == 0 {
		return nil
	}
	v := checksum.Values{}
	for name, h := range d.sums {
		v[name] = checksum.Base64(h)
	}
	return v
}

// verify holds the digests against what the client stated.
func (d *digests) verify(contentMD5 *string, supplied checksum.Values) *failure {
	if contentMD5 != nil {
		want, err := base64.StdEncoding.DecodeString(*contentMD5)
		if err != nil || len(want) != md5.Size {
			return fail(http.StatusBadRequest, "InvalidDigest", "The Content-MD5 you specified was invalid.")
		}
		if !bytes.Equal(d.md5.Sum(nil), want) {
			return fail(http.StatusBadRequest, "BadDigest", "The Content-MD5 you specified did not match what we received.")
		}
	}
	got := d.values()
	for name, want := range supplied {
		if got[name] != want {
			return fail(http.StatusBadRequest, "BadDigest",
				fmt.Sprintf("The %s you specified did not match the calculated checksum.", name))
		}
	}
	return nil
}

// names lists the algorithms a write needs digests of: those it supplied a
// value for and the one it named.
func checksumNames(supplied checksum.Values, algorithm string) []string {
	names := make([]string, 0, len(supplied)+1)
	for name := range supplied {
		names = append(names, name)
	}
	return append(names, algorithm)
}

// digestContent digests content already stored.
func digestContent(c content, names ...string) (*digests, error) {
	d, f := newDigests(names...)
	if f != nil {
		return nil, f.err()
	}
	if err := c.writeTo(d); err != nil {
		return nil, internalError(err)
	}
	return d, nil
}
