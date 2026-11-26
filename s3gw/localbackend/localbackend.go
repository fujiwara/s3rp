// Package localbackend is an S3 object store in the gateway's own process,
// shaped as the client the gateway calls a backend through. With it a service built on
// s3gw runs without any backend at all: the gateway, the service's own hooks
// and store, and this package in one process — a local stand-in for the
// service (an emulator, a test fixture), not a place to keep data.
//
//	be := localbackend.New()
//	gw.SetBackendFactory(func(context.Context, *store.Backend) (s3gw.BackendClient, error) {
//		return be, nil
//	})
//
// New holds everything in memory; Open keeps it in a directory, so that it
// is there again when the process is started the next time.
//
// One Backend holds every bucket, whatever backend definition a bucket
// names, so the definitions need only agree on what the gateway compares
// (the copy operations require the same endpoint, region and access key).
//
// Besides the methods of s3gw.BackendClient it has the ones a control plane
// calls on a backend with the backend's own credentials — CreateBucket,
// DeleteBucket, PutBucketVersioning, PutObjectLockConfiguration,
// PutBucketEncryption — with the signatures of the aws-sdk's *s3.Client, so
// that code written against an interface over the SDK client takes either.
//
// What it does not do is what a backend does on its own schedule or beyond
// the API: lifecycle expiration, replication, encryption (the requested
// mode and key id are recorded and reported, nothing is encrypted), storage
// tiers (a class is a label). A checksum algorithm it cannot compute (the
// XXHASH family) is refused rather than stored unverified. Errors carry
// S3's codes and statuses; the wording a client reads is the gateway's.
package localbackend

import (
	"context"
	"crypto/rand"
	"net/http"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/fujiwara/s3rp/checksum"
	"github.com/fujiwara/s3rp/s3gw"
)

var _ s3gw.BackendClient = (*Backend)(nil)
var _ s3gw.StreamChecksummer = (*Backend)(nil)

// Backend is an in-memory S3 object store. The zero value is not usable;
// call New. It is safe for concurrent use.
type Backend struct {
	mu      sync.RWMutex
	buckets map[string]*bucket
	now     func() time.Time

	// The rest is set by Open and unused by a Backend held in memory
	// (persist.go). dir is where the state is kept.
	dir string
	// dirty reports a change not yet written; guarded by mu
	dirty bool
	// wake tells the flusher of a change; stop ends it, stopped says it has
	wake, stop, stopped chan struct{}
	closeOnce           sync.Once
	closeErr            error
	// flushMu serializes flushes
	flushMu sync.Mutex
	// blobMu guards the blob files' books: known is every blob file, and
	// each blob's state says where it is in its life (blobTransitions)
	blobMu sync.Mutex
	known  map[string]*blob
}

// New returns an empty Backend that holds everything in memory.
func New() *Backend {
	return &Backend{buckets: map[string]*bucket{}, now: time.Now}
}

// SetNow replaces the clock behind modification times and Object Lock
// retention, so a test can let a retention period pass.
func (b *Backend) SetNow(f func() time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.now = f
}

// ChecksumsStream implements s3gw.StreamChecksummer: the body is read into
// memory, so its checksum can always be computed.
func (b *Backend) ChecksumsStream() bool { return true }

// clock is the current time at the precision S3 keeps: whole seconds, in
// listings too.
func (b *Backend) clock() time.Time {
	return b.now().UTC().Truncate(time.Second)
}

type bucket struct {
	created    time.Time
	versioning types.BucketVersioningStatus // "" until versioning is first set
	lock       *types.ObjectLockConfiguration
	encryption *types.ServerSideEncryptionConfiguration
	// objects holds each key's versions, newest first; a key that was never
	// versioned has exactly one, with the version id "null"
	objects map[string][]*object
	uploads map[string]*upload
	// finished remembers the key of each upload that was completed or
	// aborted: aborting one again succeeds, as on S3, where aborting an id
	// that never existed does not
	finished map[string]string
}

// nullVersion is the version id of an object written while versioning is
// off or suspended.
const nullVersion = "null"

// versioningState is a bucket's versioning with its Object Lock, which
// depend on each other: what the versioning and lock fields say together.
type versioningState int

const (
	// versioningOff: never set; every key holds one version, "null"
	versioningOff versioningState = iota + 1
	// versioningEnabled: every write is a new version
	versioningEnabled
	// versioningSuspended: writes replace the "null" version, and the
	// versions written while enabled stay
	versioningSuspended
	// versioningLocked: enabled, with Object Lock, and so for good
	versioningLocked
	numVersioningStates
)

func (s versioningState) String() string {
	return [...]string{"", "off", "enabled", "suspended", "locked"}[s]
}

// versioningEvent is a request that changes the versioning or the lock.
type versioningEvent int

const (
	// versioningEnable, versioningSuspend: PutBucketVersioning
	versioningEnable versioningEvent = iota
	versioningSuspend
	// versioningLock: PutObjectLockConfiguration
	versioningLock
	numVersioningEvents
)

func (e versioningEvent) String() string {
	return [...]string{"enable", "suspend", "lock"}[e]
}

// versioningTransitions is what a bucket's versioning can become; a zero
// entry is a request refused with InvalidBucketState. Nothing leads back to
// versioningOff, and nothing out of versioningLocked: a version under
// Object Lock is kept by every write being a new version. It is held to
// tla/VersionStore.tla, which is checked for that (TestVersioningTable).
var versioningTransitions = [numVersioningStates][numVersioningEvents]versioningState{
	versioningOff:       {versioningEnable: versioningEnabled, versioningSuspend: versioningSuspended},
	versioningEnabled:   {versioningEnable: versioningEnabled, versioningSuspend: versioningSuspended, versioningLock: versioningLocked},
	versioningSuspended: {versioningEnable: versioningEnabled, versioningSuspend: versioningSuspended},
	versioningLocked:    {versioningEnable: versioningLocked, versioningLock: versioningLocked},
}

func (bkt *bucket) versioningState() versioningState {
	switch {
	case bkt.lock != nil:
		return versioningLocked
	case bkt.versioning == types.BucketVersioningStatusEnabled:
		return versioningEnabled
	case bkt.versioning == types.BucketVersioningStatusSuspended:
		return versioningSuspended
	}
	return versioningOff
}

// setVersioning takes the step of e: it sets the versioning the state it
// leads to has and reports whether there is such a step. The lock itself is
// the caller's to store.
func (bkt *bucket) setVersioning(e versioningEvent) bool {
	switch versioningTransitions[bkt.versioningState()][e] {
	case versioningEnabled, versioningLocked:
		bkt.versioning = types.BucketVersioningStatusEnabled
	case versioningSuspended:
		bkt.versioning = types.BucketVersioningStatusSuspended
	default:
		return false
	}
	return true
}

// object is one version of a key. Everything but the tags and the lock
// state is immutable once stored, and so are the blobs its body is made of.
type object struct {
	versionID    string
	deleteMarker bool
	modified     time.Time
	body         content
	size         int64
	etag         string // quoted, as S3 sends it
	attrs        contentAttrs
	metadata     map[string]string
	storageClass string
	sse          types.ServerSideEncryption
	kmsKeyID     string
	tags         []types.Tag
	checksums    checksum.Values
	checksumType types.ChecksumType
	// parts is set for an object assembled by CompleteMultipartUpload
	parts       []objectPart
	lockMode    types.ObjectLockMode
	retainUntil *time.Time
	legalHold   types.ObjectLockLegalHoldStatus
}

type contentAttrs struct {
	contentType        *string
	cacheControl       *string
	contentDisposition *string
	contentEncoding    *string
	contentLanguage    *string
	expires            *time.Time
}

type objectPart struct {
	size      int64
	checksums checksum.Values
}

func (b *Backend) getBucket(name *string) (*bucket, *failure) {
	bkt, ok := b.buckets[aws.ToString(name)]
	if !ok {
		return nil, fail(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist")
	}
	return bkt, nil
}

// current is the latest version of a key, nil when the key has none.
func (bkt *bucket) current(key string) *object {
	if vs := bkt.objects[key]; len(vs) > 0 {
		return vs[0]
	}
	return nil
}

// versionIDOut is the version id reported for an object: none in a bucket
// that was never versioned.
func (bkt *bucket) versionIDOut(o *object) *string {
	if bkt.versioning == "" {
		return nil
	}
	return aws.String(o.versionID)
}

func newID() string { return rand.Text() }

// CreateBucket creates a bucket; ObjectLockEnabledForBucket also enables
// versioning, as on S3.
func (b *Backend) CreateBucket(ctx context.Context, in *s3.CreateBucketInput, _ ...func(*s3.Options)) (*s3.CreateBucketOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name := aws.ToString(in.Bucket)
	if name == "" {
		return nil, fail(http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid.").err()
	}
	defer b.mutate()()
	if _, ok := b.buckets[name]; ok {
		return nil, fail(http.StatusConflict, "BucketAlreadyOwnedByYou",
			"Your previous request to create the named bucket succeeded and you already own it.").err()
	}
	bkt := &bucket{created: b.clock(), objects: map[string][]*object{}, uploads: map[string]*upload{}, finished: map[string]string{}}
	if aws.ToBool(in.ObjectLockEnabledForBucket) {
		bkt.versioning = types.BucketVersioningStatusEnabled
		bkt.lock = &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled}
	}
	b.buckets[name] = bkt
	return &s3.CreateBucketOutput{Location: aws.String("/" + name)}, nil
}

// DeleteBucket deletes a bucket holding no object, version or delete
// marker. Multipart uploads in progress go with it.
func (b *Backend) DeleteBucket(ctx context.Context, in *s3.DeleteBucketInput, _ ...func(*s3.Options)) (*s3.DeleteBucketOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer b.mutate()()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	if len(bkt.objects) > 0 {
		return nil, fail(http.StatusConflict, "BucketNotEmpty", "The bucket you tried to delete is not empty").err()
	}
	delete(b.buckets, aws.ToString(in.Bucket))
	return &s3.DeleteBucketOutput{}, nil
}

// PutBucketVersioning enables or suspends versioning. A bucket with Object
// Lock cannot be suspended.
func (b *Backend) PutBucketVersioning(ctx context.Context, in *s3.PutBucketVersioningInput, _ ...func(*s3.Options)) (*s3.PutBucketVersioningOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer b.mutate()()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	var status types.BucketVersioningStatus
	if in.VersioningConfiguration != nil {
		status = in.VersioningConfiguration.Status
	}
	var event versioningEvent
	switch status {
	case types.BucketVersioningStatusEnabled:
		event = versioningEnable
	case types.BucketVersioningStatusSuspended:
		event = versioningSuspend
	default:
		return nil, fail(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema").err()
	}
	if !bkt.setVersioning(event) {
		return nil, fail(http.StatusConflict, "InvalidBucketState",
			"An Object Lock configuration is present on this bucket, so the versioning state cannot be changed.").err()
	}
	return &s3.PutBucketVersioningOutput{}, nil
}

// PutObjectLockConfiguration sets a bucket's Object Lock configuration,
// its default retention included. The bucket must have versioning enabled.
func (b *Backend) PutObjectLockConfiguration(ctx context.Context, in *s3.PutObjectLockConfigurationInput, _ ...func(*s3.Options)) (*s3.PutObjectLockConfigurationOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer b.mutate()()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	if versioningTransitions[bkt.versioningState()][versioningLock] == 0 {
		return nil, fail(http.StatusConflict, "InvalidBucketState",
			"Versioning must be 'Enabled' on the bucket to apply a Object Lock configuration").err()
	}
	cfg := in.ObjectLockConfiguration
	if cfg == nil || cfg.ObjectLockEnabled != types.ObjectLockEnabledEnabled {
		return nil, malformedXML().err()
	}
	stored := &types.ObjectLockConfiguration{ObjectLockEnabled: types.ObjectLockEnabledEnabled}
	if cfg.Rule != nil && cfg.Rule.DefaultRetention != nil {
		dr := *cfg.Rule.DefaultRetention
		days, years := aws.ToInt32(dr.Days), aws.ToInt32(dr.Years)
		validMode := dr.Mode == types.ObjectLockRetentionModeGovernance || dr.Mode == types.ObjectLockRetentionModeCompliance
		// exactly one of the two periods, and a positive one
		if !validMode || (days > 0) == (years > 0) || days < 0 || years < 0 {
			return nil, malformedXML().err()
		}
		stored.Rule = &types.ObjectLockRule{DefaultRetention: &dr}
	}
	bkt.lock = stored
	return &s3.PutObjectLockConfigurationOutput{}, nil
}

// PutBucketEncryption sets the bucket's default encryption, which objects
// written without an encryption mode of their own then report.
func (b *Backend) PutBucketEncryption(ctx context.Context, in *s3.PutBucketEncryptionInput, _ ...func(*s3.Options)) (*s3.PutBucketEncryptionOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer b.mutate()()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	cfg := in.ServerSideEncryptionConfiguration
	if cfg == nil || len(cfg.Rules) != 1 || cfg.Rules[0].ApplyServerSideEncryptionByDefault == nil {
		return nil, malformedXML().err()
	}
	stored := *cfg
	bkt.encryption = &stored
	return &s3.PutBucketEncryptionOutput{}, nil
}

// DeleteBucketEncryption removes the bucket's default encryption.
func (b *Backend) DeleteBucketEncryption(ctx context.Context, in *s3.DeleteBucketEncryptionInput, _ ...func(*s3.Options)) (*s3.DeleteBucketEncryptionOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer b.mutate()()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	bkt.encryption = nil
	return &s3.DeleteBucketEncryptionOutput{}, nil
}

func (b *Backend) HeadBucket(ctx context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	if _, f := b.getBucket(in.Bucket); f != nil {
		return nil, f.head().err()
	}
	return &s3.HeadBucketOutput{}, nil
}

func (b *Backend) GetBucketVersioning(ctx context.Context, in *s3.GetBucketVersioningInput, _ ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	return &s3.GetBucketVersioningOutput{Status: bkt.versioning}, nil
}

func (b *Backend) GetBucketEncryption(ctx context.Context, in *s3.GetBucketEncryptionInput, _ ...func(*s3.Options)) (*s3.GetBucketEncryptionOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	if bkt.encryption == nil {
		return nil, fail(http.StatusNotFound, "ServerSideEncryptionConfigurationNotFoundError",
			"The server side encryption configuration was not found").err()
	}
	cfg := *bkt.encryption
	return &s3.GetBucketEncryptionOutput{ServerSideEncryptionConfiguration: &cfg}, nil
}

func (b *Backend) GetObjectLockConfiguration(ctx context.Context, in *s3.GetObjectLockConfigurationInput, _ ...func(*s3.Options)) (*s3.GetObjectLockConfigurationOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	if bkt.lock == nil {
		return nil, fail(http.StatusNotFound, "ObjectLockConfigurationNotFoundError",
			"Object Lock configuration does not exist for this bucket").err()
	}
	cfg := *bkt.lock
	return &s3.GetObjectLockConfigurationOutput{ObjectLockConfiguration: &cfg}, nil
}
