package localbackend

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/fujiwara/s3rp/checksum"
)

// The layout of a directory given to Open:
//
//	meta.json    every bucket, key, version and upload, and the blobs each
//	             consists of; rewritten as a whole
//	blobs/<id>   one file per uploaded body, never rewritten
//
// Blob files carry opaque names, not keys: S3 keys a filesystem cannot keep
// apart ("a" and "a/", names differing in case) are different objects.
const (
	metaFile    = "meta.json"
	blobDirName = "blobs"
	// tempPrefix marks a blob still being written; one left behind by a
	// process that died is removed by the next Open
	tempPrefix = "tmp-"
	// formatVersion is the version of meta.json this package writes, and
	// the only one it reads
	formatVersion = 1
	// flushDelay is how long a change waits for others to be written with
	flushDelay = 100 * time.Millisecond
)

// Open returns a Backend that keeps its state in dir, creating the
// directory if needed and taking up what an earlier Backend left there.
// Call Close when done with it.
//
// It is the same backend as New's, with files where that one has memory,
// and still a stand-in rather than storage: changes reach the directory a
// moment after the request that made them is answered, nothing is synced
// to the device, and one directory serves one Backend at a time — which
// nothing enforces. It survives the process ending; it does not promise to
// survive the machine losing power.
func Open(dir string) (*Backend, error) {
	// an empty dir is how the rest of the package tells memory from files
	if dir == "" {
		return nil, errors.New("localbackend: Open needs a directory; use New for a Backend in memory")
	}
	b := New()
	b.dir = dir
	b.pending = map[string]*blob{}
	b.known = map[string]*blob{}
	b.garbage = map[string]*blob{}
	if err := os.MkdirAll(b.blobDir(), 0o700); err != nil {
		return nil, err
	}
	if err := b.load(); err != nil {
		return nil, fmt.Errorf("failed to load %s: %w", dir, err)
	}
	b.wake = make(chan struct{}, 1)
	b.stop = make(chan struct{})
	b.stopped = make(chan struct{})
	go b.flusher()
	return b, nil
}

func (b *Backend) blobDir() string { return filepath.Join(b.dir, blobDirName) }

// Close writes what has not been written yet and stops the Backend's
// writing to its directory. The Backend must not be used afterwards. On a
// Backend made by New it does nothing.
func (b *Backend) Close() error {
	if b.dir == "" {
		return nil
	}
	b.closeOnce.Do(func() {
		close(b.stop)
		<-b.stopped
		b.closeErr = b.flush()
	})
	return b.closeErr
}

// Sync writes what has not been written yet, without waiting for the
// delay changes are normally gathered over. On a Backend made by New it
// does nothing.
func (b *Backend) Sync() error {
	if b.dir == "" {
		return nil
	}
	return b.flush()
}

// mutate takes the write lock for a change of state; the function it
// returns releases the lock and has the change written.
func (b *Backend) mutate() func() {
	b.mu.Lock()
	return func() {
		b.dirty = true
		b.mu.Unlock()
		if b.wake != nil {
			select {
			case b.wake <- struct{}{}:
			default:
			}
		}
	}
}

func (b *Backend) flusher() {
	defer close(b.stopped)
	for {
		select {
		case <-b.stop:
			return
		case <-b.wake:
		}
		select {
		case <-b.stop:
			return
		case <-time.After(flushDelay):
		}
		// an error is kept for Sync and Close to report, and the state
		// stays dirty for the next attempt
		_ = b.flush()
	}
}

// flush writes the state if it changed, then removes the blob files the
// written state no longer refers to. The order is what keeps the directory
// sound whenever the process stops: a blob is on disk before meta.json
// names it, and is removed only after a meta.json that does not.
func (b *Backend) flush() error {
	b.flushMu.Lock()
	defer b.flushMu.Unlock()

	b.mu.Lock()
	dirty := b.dirty
	b.dirty = false
	var snap *snapshot
	if dirty {
		var referenced map[string]bool
		snap, referenced = b.snapshot()
		// decided under the same lock as the snapshot: a blob neither
		// referenced nor pending now can never be referenced again
		b.blobMu.Lock()
		for id, bl := range b.known {
			if !referenced[id] && b.pending[id] == nil {
				b.garbage[id] = bl
			}
		}
		b.blobMu.Unlock()
	}
	b.mu.Unlock()

	if dirty {
		if err := b.writeMeta(snap); err != nil {
			b.mu.Lock()
			b.dirty = true
			b.mu.Unlock()
			return err
		}
	}
	// under the write lock no response body can be opened, so a blob
	// without readers here stays without
	b.mu.Lock()
	defer b.mu.Unlock()
	var errs []error
	for id, bl := range b.garbage {
		if bl.readers.Load() > 0 {
			continue // a later flush, or the next Open, takes it
		}
		if err := os.Remove(bl.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
			continue
		}
		delete(b.garbage, id)
		b.blobMu.Lock()
		delete(b.known, id)
		b.blobMu.Unlock()
	}
	return errors.Join(errs...)
}

// metaOptions keep a key or a value that is not UTF-8 from failing every
// later flush: its invalid bytes are written as U+FFFD, and two keys that
// become the same name that way are both written, the last being read.
var metaOptions = json.JoinOptions(
	json.Deterministic(true),
	jsontext.AllowInvalidUTF8(true),
	jsontext.AllowDuplicateNames(true),
)

func (b *Backend) writeMeta(snap *snapshot) error {
	data, err := json.Marshal(snap, metaOptions)
	if err != nil {
		return err
	}
	tmp := filepath.Join(b.dir, metaFile+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(b.dir, metaFile))
}

// load restores the state from the directory and clears what a previous
// process left unfinished: blobs half written, and blobs nothing refers to.
func (b *Backend) load() error {
	entries, err := os.ReadDir(b.blobDir())
	if err != nil {
		return err
	}
	for _, e := range entries {
		path := filepath.Join(b.blobDir(), e.Name())
		if strings.HasPrefix(e.Name(), tempPrefix) {
			if err := os.Remove(path); err != nil {
				return err
			}
			continue
		}
		info, err := e.Info()
		if err != nil {
			return err
		}
		b.known[e.Name()] = &blob{id: e.Name(), size: info.Size(), path: path}
	}

	data, err := os.ReadFile(filepath.Join(b.dir, metaFile))
	switch {
	case errors.Is(err, fs.ErrNotExist):
		data = nil
	case err != nil:
		return err
	}
	referenced := map[string]bool{}
	if data != nil {
		var snap snapshot
		if err := json.Unmarshal(data, &snap, metaOptions); err != nil {
			return fmt.Errorf("%s: %w", metaFile, err)
		}
		if snap.Version != formatVersion {
			return fmt.Errorf("%s: format version %d, this package reads %d", metaFile, snap.Version, formatVersion)
		}
		if err := b.restore(&snap, referenced); err != nil {
			return fmt.Errorf("%s: %w", metaFile, err)
		}
	}
	for id, bl := range b.known {
		if !referenced[id] {
			if err := os.Remove(bl.path); err != nil {
				return err
			}
			delete(b.known, id)
		}
	}
	return nil
}

// The records below are meta.json. They are this package's own, apart from
// the SDK's types the state is held in, so that an SDK update cannot change
// the format; a change to them needs a new formatVersion.

type snapshot struct {
	Version int                      `json:"version"`
	Buckets map[string]*bucketRecord `json:"buckets"`
}

type bucketRecord struct {
	Created    time.Time                  `json:"created"`
	Versioning string                     `json:"versioning,omitempty"`
	Lock       *lockRecord                `json:"lock,omitzero"`
	Encryption *encryptionRecord          `json:"encryption,omitzero"`
	Objects    map[string][]*objectRecord `json:"objects,omitempty"`
	Uploads    []*uploadRecord            `json:"uploads,omitempty"`
	Finished   map[string]string          `json:"finished,omitempty"`
}

// lockRecord is an Object Lock configuration; its presence is the lock
// being enabled, the fields its default retention.
type lockRecord struct {
	Mode  string `json:"mode,omitempty"`
	Days  int32  `json:"days,omitzero"`
	Years int32  `json:"years,omitzero"`
}

type encryptionRecord struct {
	Algorithm        string `json:"algorithm"`
	KMSKeyID         string `json:"kmsKeyId,omitempty"`
	BucketKeyEnabled *bool  `json:"bucketKeyEnabled,omitzero"`
}

type segmentRecord struct {
	Blob string `json:"blob"`
	Off  int64  `json:"off,omitzero"`
	Size int64  `json:"size"`
}

type tagRecord struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

type objectPartRecord struct {
	Size      int64             `json:"size"`
	Checksums map[string]string `json:"checksums,omitempty"`
}

type objectRecord struct {
	VersionID          string             `json:"versionId,omitempty"`
	DeleteMarker       bool               `json:"deleteMarker,omitzero"`
	Modified           time.Time          `json:"modified"`
	Body               []segmentRecord    `json:"body,omitempty"`
	ETag               string             `json:"etag,omitempty"`
	ContentType        *string            `json:"contentType,omitzero"`
	CacheControl       *string            `json:"cacheControl,omitzero"`
	ContentDisposition *string            `json:"contentDisposition,omitzero"`
	ContentEncoding    *string            `json:"contentEncoding,omitzero"`
	ContentLanguage    *string            `json:"contentLanguage,omitzero"`
	Expires            *time.Time         `json:"expires,omitempty"`
	Metadata           map[string]string  `json:"metadata,omitempty"`
	StorageClass       string             `json:"storageClass,omitempty"`
	SSE                string             `json:"sse,omitempty"`
	KMSKeyID           string             `json:"kmsKeyId,omitempty"`
	Tags               []tagRecord        `json:"tags,omitempty"`
	Checksums          map[string]string  `json:"checksums,omitempty"`
	ChecksumType       string             `json:"checksumType,omitempty"`
	Parts              []objectPartRecord `json:"parts,omitempty"`
	LockMode           string             `json:"lockMode,omitempty"`
	RetainUntil        *time.Time         `json:"retainUntil,omitempty"`
	LegalHold          string             `json:"legalHold,omitempty"`
}

type partRecord struct {
	Number    int32             `json:"number"`
	Body      []segmentRecord   `json:"body,omitempty"`
	ETag      string            `json:"etag"`
	Modified  time.Time         `json:"modified"`
	Checksums map[string]string `json:"checksums,omitempty"`
}

type uploadRecord struct {
	ID           string        `json:"id"`
	Key          string        `json:"key"`
	Initiated    time.Time     `json:"initiated"`
	Object       *objectRecord `json:"object"`
	Algorithm    string        `json:"algorithm,omitempty"`
	ChecksumType string        `json:"checksumType,omitempty"`
	Parts        []partRecord  `json:"parts,omitempty"`
}

// snapshot renders the state as records, and the ids of the blobs it
// refers to. The caller holds the lock.
func (b *Backend) snapshot() (*snapshot, map[string]bool) {
	referenced := map[string]bool{}
	body := func(c content) []segmentRecord {
		out := make([]segmentRecord, 0, len(c))
		for _, s := range c {
			referenced[s.blob.id] = true
			out = append(out, segmentRecord{Blob: s.blob.id, Off: s.off, Size: s.size})
		}
		return out
	}
	record := func(o *object) *objectRecord {
		r := &objectRecord{
			VersionID:          o.versionID,
			DeleteMarker:       o.deleteMarker,
			Modified:           o.modified,
			Body:               body(o.body),
			ETag:               o.etag,
			ContentType:        o.attrs.contentType,
			CacheControl:       o.attrs.cacheControl,
			ContentDisposition: o.attrs.contentDisposition,
			ContentEncoding:    o.attrs.contentEncoding,
			ContentLanguage:    o.attrs.contentLanguage,
			Expires:            o.attrs.expires,
			Metadata:           o.metadata,
			StorageClass:       o.storageClass,
			SSE:                string(o.sse),
			KMSKeyID:           o.kmsKeyID,
			Checksums:          o.checksums,
			ChecksumType:       string(o.checksumType),
			LockMode:           string(o.lockMode),
			RetainUntil:        o.retainUntil,
			LegalHold:          string(o.legalHold),
		}
		for _, t := range o.tags {
			r.Tags = append(r.Tags, tagRecord{Key: aws.ToString(t.Key), Value: aws.ToString(t.Value)})
		}
		for _, p := range o.parts {
			r.Parts = append(r.Parts, objectPartRecord{Size: p.size, Checksums: p.checksums})
		}
		return r
	}

	snap := &snapshot{Version: formatVersion, Buckets: map[string]*bucketRecord{}}
	for name, bkt := range b.buckets {
		br := &bucketRecord{
			Created:    bkt.created,
			Versioning: string(bkt.versioning),
			Objects:    map[string][]*objectRecord{},
			Finished:   maps.Clone(bkt.finished),
		}
		if bkt.lock != nil {
			br.Lock = &lockRecord{}
			if bkt.lock.Rule != nil && bkt.lock.Rule.DefaultRetention != nil {
				dr := bkt.lock.Rule.DefaultRetention
				br.Lock.Mode, br.Lock.Days, br.Lock.Years = string(dr.Mode), aws.ToInt32(dr.Days), aws.ToInt32(dr.Years)
			}
		}
		if bkt.encryption != nil {
			rule := bkt.encryption.Rules[0]
			def := rule.ApplyServerSideEncryptionByDefault
			br.Encryption = &encryptionRecord{
				Algorithm:        string(def.SSEAlgorithm),
				KMSKeyID:         aws.ToString(def.KMSMasterKeyID),
				BucketKeyEnabled: rule.BucketKeyEnabled,
			}
		}
		for key, versions := range bkt.objects {
			for _, o := range versions {
				br.Objects[key] = append(br.Objects[key], record(o))
			}
		}
		for _, u := range bkt.uploads {
			ur := &uploadRecord{
				ID:           u.id,
				Key:          u.key,
				Initiated:    u.initiated,
				Object:       record(u.proto),
				Algorithm:    string(u.algorithm),
				ChecksumType: string(u.checksumType),
			}
			for n, p := range u.parts {
				ur.Parts = append(ur.Parts, partRecord{
					Number: n, Body: body(p.body), ETag: p.etag, Modified: p.modified, Checksums: p.checksums,
				})
			}
			br.Uploads = append(br.Uploads, ur)
		}
		snap.Buckets[name] = br
	}
	return snap, referenced
}

// restore rebuilds the state from records, marking the blobs it refers to.
// A record naming a blob that is not in the directory, or more of one than
// the file holds, is an error: the directory is not one this package left.
func (b *Backend) restore(snap *snapshot, referenced map[string]bool) error {
	body := func(records []segmentRecord) (content, error) {
		var c content
		for _, r := range records {
			bl, ok := b.known[r.Blob]
			if !ok {
				return nil, fmt.Errorf("blob %q is missing", r.Blob)
			}
			if r.Off < 0 || r.Size < 0 || r.Off+r.Size > bl.size {
				return nil, fmt.Errorf("blob %q holds %d bytes, a record wants %d from %d", r.Blob, bl.size, r.Size, r.Off)
			}
			referenced[r.Blob] = true
			c = append(c, segment{blob: bl, off: r.Off, size: r.Size})
		}
		return c, nil
	}
	restored := func(r *objectRecord) (*object, error) {
		c, err := body(r.Body)
		if err != nil {
			return nil, err
		}
		o := &object{
			versionID:    r.VersionID,
			deleteMarker: r.DeleteMarker,
			modified:     r.Modified,
			body:         c,
			size:         c.size(),
			etag:         r.ETag,
			attrs: contentAttrs{
				contentType:        r.ContentType,
				cacheControl:       r.CacheControl,
				contentDisposition: r.ContentDisposition,
				contentEncoding:    r.ContentEncoding,
				contentLanguage:    r.ContentLanguage,
				expires:            r.Expires,
			},
			metadata:     r.Metadata,
			storageClass: r.StorageClass,
			sse:          types.ServerSideEncryption(r.SSE),
			kmsKeyID:     r.KMSKeyID,
			checksums:    checksum.Values(r.Checksums),
			checksumType: types.ChecksumType(r.ChecksumType),
			lockMode:     types.ObjectLockMode(r.LockMode),
			retainUntil:  r.RetainUntil,
			legalHold:    types.ObjectLockLegalHoldStatus(r.LegalHold),
		}
		for _, t := range r.Tags {
			o.tags = append(o.tags, types.Tag{Key: aws.String(t.Key), Value: aws.String(t.Value)})
		}
		for _, p := range r.Parts {
			o.parts = append(o.parts, objectPart{size: p.Size, checksums: checksum.Values(p.Checksums)})
		}
		return o, nil
	}

	for name, br := range snap.Buckets {
		bkt := &bucket{
			created:    br.Created,
			versioning: types.BucketVersioningStatus(br.Versioning),
			objects:    map[string][]*object{},
			uploads:    map[string]*upload{},
			finished:   br.Finished,
		}
		if bkt.finished == nil {
			bkt.finished = map[string]string{}
		}
		if br.Lock != nil {
			bkt.lock = &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled}
			if br.Lock.Mode != "" {
				dr := &types.DefaultRetention{Mode: types.ObjectLockRetentionMode(br.Lock.Mode)}
				if br.Lock.Days > 0 {
					dr.Days = aws.Int32(br.Lock.Days)
				}
				if br.Lock.Years > 0 {
					dr.Years = aws.Int32(br.Lock.Years)
				}
				bkt.lock.Rule = &types.ObjectLockRule{DefaultRetention: dr}
			}
		}
		if e := br.Encryption; e != nil {
			def := &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryption(e.Algorithm)}
			if e.KMSKeyID != "" {
				def.KMSMasterKeyID = aws.String(e.KMSKeyID)
			}
			bkt.encryption = &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
				ApplyServerSideEncryptionByDefault: def, BucketKeyEnabled: e.BucketKeyEnabled,
			}}}
		}
		for key, records := range br.Objects {
			for _, r := range records {
				o, err := restored(r)
				if err != nil {
					return fmt.Errorf("%s/%s: %w", name, key, err)
				}
				bkt.objects[key] = append(bkt.objects[key], o)
			}
		}
		for _, ur := range br.Uploads {
			if ur.Object == nil {
				return fmt.Errorf("%s: upload %s has no object record", name, ur.ID)
			}
			proto, err := restored(ur.Object)
			if err != nil {
				return fmt.Errorf("%s: upload %s: %w", name, ur.ID, err)
			}
			u := &upload{
				id:           ur.ID,
				key:          ur.Key,
				initiated:    ur.Initiated,
				proto:        proto,
				algorithm:    types.ChecksumAlgorithm(ur.Algorithm),
				checksumType: types.ChecksumType(ur.ChecksumType),
				parts:        map[int32]*part{},
			}
			for _, pr := range ur.Parts {
				c, err := body(pr.Body)
				if err != nil {
					return fmt.Errorf("%s: upload %s part %d: %w", name, ur.ID, pr.Number, err)
				}
				u.parts[pr.Number] = &part{
					body: c, size: c.size(), etag: pr.ETag, modified: pr.Modified, checksums: checksum.Values(pr.Checksums),
				}
			}
			bkt.uploads[u.id] = u
		}
		b.buckets[name] = bkt
	}
	return nil
}
