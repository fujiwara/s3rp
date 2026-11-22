package localbackend

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/fujiwara/s3rp/checksum"
)

func incompleteBody() *failure {
	return fail(http.StatusBadRequest, "IncompleteBody",
		"You did not provide the number of bytes specified by the Content-Length HTTP header")
}

// parseTagging reads the x-amz-tagging form (a URL query string).
func parseTagging(s *string) ([]types.Tag, *failure) {
	if s == nil || *s == "" {
		return nil, nil
	}
	q, err := url.ParseQuery(*s)
	if err != nil {
		return nil, invalidArgument("The header 'x-amz-tagging' shall be encoded as UTF-8 then URLEncoded URL query parameters without tag name duplicates.")
	}
	tags := make([]types.Tag, 0, len(q))
	for k, vs := range q {
		if len(vs) != 1 {
			return nil, invalidArgument("The header 'x-amz-tagging' shall be encoded as UTF-8 then URLEncoded URL query parameters without tag name duplicates.")
		}
		tags = append(tags, types.Tag{Key: aws.String(k), Value: aws.String(vs[0])})
	}
	slices.SortFunc(tags, func(a, b types.Tag) int { return strings.Compare(*a.Key, *b.Key) })
	return tags, nil
}

// etagMatches implements If-Match / If-None-Match: a list of entity tags,
// or "*" for any.
func etagMatches(header, etag string) bool {
	etag = strings.Trim(etag, `"`)
	for candidate := range strings.SplitSeq(header, ",") {
		candidate = strings.TrimSpace(candidate)
		if candidate == "*" {
			return true
		}
		candidate = strings.TrimPrefix(candidate, "W/")
		if strings.Trim(candidate, `"`) == etag {
			return true
		}
	}
	return false
}

func preconditionFailed() *failure {
	return fail(http.StatusPreconditionFailed, "PreconditionFailed",
		"At least one of the pre-conditions you specified did not hold")
}

// checkWritePreconditions applies If-Match / If-None-Match of a write to
// the key's current object.
func checkWritePreconditions(cur *object, ifMatch, ifNoneMatch *string) *failure {
	exists := cur != nil && !cur.deleteMarker
	if ifNoneMatch != nil && exists && etagMatches(*ifNoneMatch, cur.etag) {
		return preconditionFailed()
	}
	if ifMatch != nil {
		if !exists {
			return fail(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		}
		if !etagMatches(*ifMatch, cur.etag) {
			return preconditionFailed()
		}
	}
	return nil
}

// commit stores o as the new current version of key. o carries everything
// but its version id and modification time.
func (b *Backend) commit(bkt *bucket, key string, o *object) *failure {
	now := b.clock()
	if f := applyLockOnWrite(bkt, o, now); f != nil {
		return f
	}
	if o.sse == "" && bkt.encryption != nil {
		def := bkt.encryption.Rules[0].ApplyServerSideEncryptionByDefault
		o.sse = def.SSEAlgorithm
		o.kmsKeyID = aws.ToString(def.KMSMasterKeyID)
	}
	o.modified = now
	bkt.put(key, o)
	return nil
}

// put makes o the current version of key under the bucket's versioning
// state: a new version when enabled, otherwise replacing the null version.
func (bkt *bucket) put(key string, o *object) {
	switch bkt.versioning {
	case types.BucketVersioningStatusEnabled:
		o.versionID = newID()
		bkt.objects[key] = append([]*object{o}, bkt.objects[key]...)
	case types.BucketVersioningStatusSuspended:
		o.versionID = nullVersion
		kept := slices.DeleteFunc(bkt.objects[key], func(v *object) bool { return v.versionID == nullVersion })
		bkt.objects[key] = append([]*object{o}, kept...)
	default:
		o.versionID = nullVersion
		bkt.objects[key] = []*object{o}
	}
}

func (b *Backend) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	supplied := checksumsOf(in)
	d, f := newDigests(checksumNames(supplied, string(in.ChecksumAlgorithm))...)
	if f != nil {
		return nil, f.err()
	}
	bl, err := b.ingest(in.Body, d)
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() { b.release(bl, committed) }()
	if in.ContentLength != nil && bl.size != *in.ContentLength {
		return nil, incompleteBody().err()
	}
	if f := d.verify(in.ContentMD5, supplied); f != nil {
		return nil, f.err()
	}
	sums := d.values()
	tags, f := parseTagging(in.Tagging)
	if f != nil {
		return nil, f.err()
	}
	o := &object{
		body: content{{blob: bl, size: bl.size}},
		size: bl.size,
		etag: d.etag(),
		attrs: contentAttrs{
			contentType:        in.ContentType,
			cacheControl:       in.CacheControl,
			contentDisposition: in.ContentDisposition,
			contentEncoding:    in.ContentEncoding,
			contentLanguage:    in.ContentLanguage,
			expires:            in.Expires,
		},
		metadata:     in.Metadata,
		storageClass: string(in.StorageClass),
		sse:          in.ServerSideEncryption,
		kmsKeyID:     aws.ToString(in.SSEKMSKeyId),
		tags:         tags,
		checksums:    sums,
		lockMode:     in.ObjectLockMode,
		retainUntil:  in.ObjectLockRetainUntilDate,
		legalHold:    in.ObjectLockLegalHoldStatus,
	}
	if len(sums) > 0 {
		o.checksumType = types.ChecksumTypeFullObject
	}

	defer b.mutate()()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	key := aws.ToString(in.Key)
	if f := checkWritePreconditions(bkt.current(key), in.IfMatch, in.IfNoneMatch); f != nil {
		return nil, f.err()
	}
	if f := b.commit(bkt, key, o); f != nil {
		return nil, f.err()
	}
	committed = true
	out := &s3.PutObjectOutput{
		ETag:                 aws.String(o.etag),
		VersionId:            bkt.versionIDOut(o),
		ServerSideEncryption: o.sse,
		ChecksumType:         o.checksumType,
		Size:                 aws.Int64(o.size),
	}
	if o.kmsKeyID != "" {
		out.SSEKMSKeyId = aws.String(o.kmsKeyID)
	}
	setChecksums(out, o.checksums)
	return out, nil
}

// find resolves the object a read or an update names: the given version,
// or the current one. A delete marker is not an object: reading the current
// one is NoSuchKey, naming one by version is MethodNotAllowed, and both say
// so in headers as S3 does.
func (bkt *bucket) find(key, versionID *string) (*object, *failure) {
	vs := bkt.objects[aws.ToString(key)]
	if id := aws.ToString(versionID); id != "" {
		for _, o := range vs {
			if o.versionID != id {
				continue
			}
			if o.deleteMarker {
				return nil, fail(http.StatusMethodNotAllowed, "MethodNotAllowed",
					"The specified method is not allowed against this resource.").
					with("x-amz-delete-marker", "true").
					with("x-amz-version-id", o.versionID).
					with("Last-Modified", o.modified.Format(http.TimeFormat))
			}
			return o, nil
		}
		return nil, fail(http.StatusNotFound, "NoSuchVersion", "The specified version does not exist.")
	}
	if len(vs) == 0 {
		return nil, fail(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
	}
	o := vs[0]
	if o.deleteMarker {
		return nil, fail(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.").
			with("x-amz-delete-marker", "true").
			with("x-amz-version-id", o.versionID)
	}
	return o, nil
}

// checkReadPreconditions applies the conditional headers of a read in the
// order HTTP gives them: If-Match before If-Unmodified-Since, If-None-Match
// before If-Modified-Since.
func checkReadPreconditions(o *object, ifMatch, ifNoneMatch *string, ifModifiedSince, ifUnmodifiedSince *time.Time) *failure {
	modified := o.modified
	notModified := func() *failure {
		return fail(http.StatusNotModified, "NotModified", "Not Modified").
			with("ETag", o.etag).
			with("Last-Modified", o.modified.Format(http.TimeFormat))
	}
	if ifMatch != nil {
		if !etagMatches(*ifMatch, o.etag) {
			return preconditionFailed()
		}
	} else if ifUnmodifiedSince != nil && modified.After(*ifUnmodifiedSince) {
		return preconditionFailed()
	}
	if ifNoneMatch != nil {
		if etagMatches(*ifNoneMatch, o.etag) {
			return notModified()
		}
	} else if ifModifiedSince != nil && !modified.After(*ifModifiedSince) {
		return notModified()
	}
	return nil
}

func invalidRange() *failure {
	return fail(http.StatusRequestedRangeNotSatisfiable, "InvalidRange", "The requested range is not satisfiable")
}

// parseRange resolves a Range header against an object's size to the
// half-open interval [start, end). ok is false for a header S3 ignores
// (another unit, several ranges, a malformed one), which reads the whole
// object.
func parseRange(header string, size int64) (start, end int64, ok bool, f *failure) {
	spec, isBytes := strings.CutPrefix(header, "bytes=")
	if !isBytes || strings.Contains(spec, ",") {
		return 0, 0, false, nil
	}
	first, last, hasDash := strings.Cut(spec, "-")
	if !hasDash {
		return 0, 0, false, nil
	}
	if first == "" {
		n, err := strconv.ParseInt(last, 10, 64)
		if err != nil || n < 0 {
			return 0, 0, false, nil
		}
		if n == 0 || size == 0 {
			return 0, 0, false, invalidRange()
		}
		return max(size-n, 0), size, true, nil
	}
	start, err := strconv.ParseInt(first, 10, 64)
	if err != nil || start < 0 {
		return 0, 0, false, nil
	}
	end = size
	if last != "" {
		l, err := strconv.ParseInt(last, 10, 64)
		if err != nil || l < start {
			return 0, 0, false, nil
		}
		// clamped before the +1: the last byte may be given as MaxInt64
		end = min(l, size-1) + 1
	}
	if start >= size {
		return 0, 0, false, invalidRange()
	}
	return start, end, true, nil
}

// view is the part of an object a read returns.
type view struct {
	start, end int64
	partial    bool
	// whole is false when a Range or a part number narrowed the read, in
	// which case the object's checksum does not describe the bytes
	whole bool
	// part is the part a partNumber read selected, whose checksum is the
	// one that describes the bytes
	part *objectPart
	// partsCount is reported on a read of one part only
	partsCount *int32
}

// checksums is the checksum that describes what the view returns: the
// object's for all of it, a part's for that part, none for a range.
func (v view) checksums(o *object) (checksum.Values, bool) {
	switch {
	case v.whole:
		return o.checksums, len(o.checksums) > 0
	case v.part != nil:
		return v.part.checksums, len(v.part.checksums) > 0
	}
	return nil, false
}

func (v view) contentRange(size int64) *string {
	if !v.partial {
		return nil
	}
	return aws.String(fmt.Sprintf("bytes %d-%d/%d", v.start, v.end-1, size))
}

// viewOf resolves partNumber and Range, which S3 does not combine.
func viewOf(o *object, partNumber *int32, rng *string) (view, *failure) {
	size := o.size
	v := view{end: size, whole: true}
	if partNumber != nil {
		if rng != nil {
			return v, fail(http.StatusBadRequest, "InvalidRequest", "Cannot specify both Range header and partNumber query parameter.")
		}
		n := int(*partNumber)
		invalidPart := fail(http.StatusRequestedRangeNotSatisfiable, "InvalidPartNumber", "The requested partnumber is not satisfiable")
		if len(o.parts) == 0 {
			// an object that was not assembled from parts is its own part 1
			if n != 1 {
				return v, invalidPart
			}
			v.partial = true
			return v, nil
		}
		if n < 1 || n > len(o.parts) {
			return v, invalidPart
		}
		var offset int64
		for _, p := range o.parts[:n-1] {
			offset += p.size
		}
		v.start, v.end, v.partial, v.whole = offset, offset+o.parts[n-1].size, true, false
		v.partsCount = aws.Int32(int32(len(o.parts)))
		v.part = &o.parts[n-1]
		return v, nil
	}
	if rng != nil {
		start, end, ok, f := parseRange(*rng, size)
		if f != nil {
			return v, f
		}
		if ok {
			v.start, v.end, v.partial, v.whole = start, end, true, false
		}
	}
	return v, nil
}

// reportedStorageClass is the class a GET or HEAD reports: S3 leaves the
// header out for STANDARD.
func (o *object) reportedStorageClass() types.StorageClass {
	if o.storageClass == string(types.StorageClassStandard) {
		return ""
	}
	return types.StorageClass(o.storageClass)
}

// listedStorageClass is the class a listing reports, where STANDARD is
// spelled out.
func (o *object) listedStorageClass() string {
	if o.storageClass == "" {
		return string(types.StorageClassStandard)
	}
	return o.storageClass
}

func (o *object) tagCount() *int32 {
	if len(o.tags) == 0 {
		return nil
	}
	return aws.Int32(int32(len(o.tags)))
}

func (o *object) kmsKeyIDOut() *string {
	if o.kmsKeyID == "" {
		return nil
	}
	return aws.String(o.kmsKeyID)
}

func (o *object) contentTypeOut() *string {
	if o.attrs.contentType == nil {
		return aws.String("binary/octet-stream")
	}
	return o.attrs.contentType
}

func (o *object) expiresOut() *string {
	if o.attrs.expires == nil {
		return nil
	}
	return aws.String(o.attrs.expires.UTC().Format(http.TimeFormat))
}

func (b *Backend) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	o, f := bkt.find(in.Key, in.VersionId)
	if f != nil {
		return nil, f.err()
	}
	if f := checkReadPreconditions(o, in.IfMatch, in.IfNoneMatch, in.IfModifiedSince, in.IfUnmodifiedSince); f != nil {
		return nil, f.err()
	}
	v, f := viewOf(o, in.PartNumber, in.Range)
	if f != nil {
		return nil, f.err()
	}
	out := &s3.GetObjectOutput{
		Body:                      o.body.slice(v.start, v.end-v.start).open(),
		ContentLength:             aws.Int64(v.end - v.start),
		ContentRange:              v.contentRange(o.size),
		AcceptRanges:              aws.String("bytes"),
		ETag:                      aws.String(o.etag),
		LastModified:              aws.Time(o.modified),
		ContentType:               o.contentTypeOut(),
		CacheControl:              o.attrs.cacheControl,
		ContentDisposition:        o.attrs.contentDisposition,
		ContentEncoding:           o.attrs.contentEncoding,
		ContentLanguage:           o.attrs.contentLanguage,
		ExpiresString:             o.expiresOut(),
		Metadata:                  o.metadata,
		StorageClass:              o.reportedStorageClass(),
		VersionId:                 bkt.versionIDOut(o),
		ServerSideEncryption:      o.sse,
		SSEKMSKeyId:               o.kmsKeyIDOut(),
		TagCount:                  o.tagCount(),
		PartsCount:                v.partsCount,
		ObjectLockMode:            o.lockMode,
		ObjectLockRetainUntilDate: o.retainUntil,
		ObjectLockLegalHoldStatus: o.legalHold,
	}
	if in.ChecksumMode == types.ChecksumModeEnabled {
		if sums, ok := v.checksums(o); ok {
			setChecksums(out, sums)
			out.ChecksumType = o.checksumType
		}
	}
	// the response-* overrides of a presigned or explicit request
	if in.ResponseContentType != nil {
		out.ContentType = in.ResponseContentType
	}
	if in.ResponseContentDisposition != nil {
		out.ContentDisposition = in.ResponseContentDisposition
	}
	if in.ResponseCacheControl != nil {
		out.CacheControl = in.ResponseCacheControl
	}
	if in.ResponseContentEncoding != nil {
		out.ContentEncoding = in.ResponseContentEncoding
	}
	if in.ResponseContentLanguage != nil {
		out.ContentLanguage = in.ResponseContentLanguage
	}
	if in.ResponseExpires != nil {
		out.ExpiresString = aws.String(in.ResponseExpires.UTC().Format(http.TimeFormat))
	}
	return out, nil
}

func (b *Backend) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.head().err()
	}
	o, f := bkt.find(in.Key, in.VersionId)
	if f != nil {
		return nil, f.head().err()
	}
	if f := checkReadPreconditions(o, in.IfMatch, in.IfNoneMatch, in.IfModifiedSince, in.IfUnmodifiedSince); f != nil {
		return nil, f.head().err()
	}
	v, f := viewOf(o, in.PartNumber, in.Range)
	if f != nil {
		return nil, f.head().err()
	}
	out := &s3.HeadObjectOutput{
		ContentLength:             aws.Int64(v.end - v.start),
		ContentRange:              v.contentRange(o.size),
		AcceptRanges:              aws.String("bytes"),
		ETag:                      aws.String(o.etag),
		LastModified:              aws.Time(o.modified),
		ContentType:               o.contentTypeOut(),
		CacheControl:              o.attrs.cacheControl,
		ContentDisposition:        o.attrs.contentDisposition,
		ContentEncoding:           o.attrs.contentEncoding,
		ContentLanguage:           o.attrs.contentLanguage,
		ExpiresString:             o.expiresOut(),
		Metadata:                  o.metadata,
		StorageClass:              o.reportedStorageClass(),
		VersionId:                 bkt.versionIDOut(o),
		ServerSideEncryption:      o.sse,
		SSEKMSKeyId:               o.kmsKeyIDOut(),
		TagCount:                  o.tagCount(),
		PartsCount:                v.partsCount,
		ObjectLockMode:            o.lockMode,
		ObjectLockRetainUntilDate: o.retainUntil,
		ObjectLockLegalHoldStatus: o.legalHold,
	}
	if in.ChecksumMode == types.ChecksumModeEnabled {
		if sums, ok := v.checksums(o); ok {
			setChecksums(out, sums)
			out.ChecksumType = o.checksumType
		}
	}
	return out, nil
}

// deletion is one delete request against a key, shared by DeleteObject and
// each entry of DeleteObjects.
type deletion struct {
	key          string
	versionID    string
	bypass       bool
	ifMatch      *string
	lastModified *time.Time
	size         *int64
}

type deleted struct {
	// versionID is the version removed, or the delete marker created
	versionID *string
	// deleteMarker reports that the version removed was a delete marker,
	// or that one was created
	deleteMarker bool
}

func (b *Backend) deleteOne(bkt *bucket, d deletion) (deleted, *failure) {
	vs := bkt.objects[d.key]
	if d.ifMatch != nil || d.lastModified != nil || d.size != nil {
		var cur *object
		if len(vs) > 0 && !vs[0].deleteMarker {
			cur = vs[0]
		}
		if cur == nil {
			return deleted{}, fail(http.StatusNotFound, "NoSuchKey", "The specified key does not exist.")
		}
		if (d.ifMatch != nil && !etagMatches(*d.ifMatch, cur.etag)) ||
			(d.lastModified != nil && !cur.modified.Equal(d.lastModified.Truncate(time.Second))) ||
			(d.size != nil && *d.size != cur.size) {
			return deleted{}, preconditionFailed()
		}
	}
	if d.versionID != "" {
		i := slices.IndexFunc(vs, func(o *object) bool { return o.versionID == d.versionID })
		if i < 0 {
			// deleting a version that is not there succeeds, as on S3
			return deleted{versionID: aws.String(d.versionID)}, nil
		}
		o := vs[i]
		// the real time: see PutObjectRetention
		if o.locked(b.now(), d.bypass) {
			return deleted{}, fail(http.StatusForbidden, "AccessDenied",
				"Access Denied because object protected by object lock.")
		}
		if vs = slices.Delete(vs, i, i+1); len(vs) == 0 {
			delete(bkt.objects, d.key)
		} else {
			bkt.objects[d.key] = vs
		}
		res := deleted{deleteMarker: o.deleteMarker}
		if bkt.versioning != "" {
			res.versionID = aws.String(d.versionID)
		}
		return res, nil
	}
	switch bkt.versioning {
	case types.BucketVersioningStatusEnabled, types.BucketVersioningStatusSuspended:
		// a delete marker becomes the current version; it is written even
		// for a key that holds nothing
		marker := &object{deleteMarker: true, modified: b.clock()}
		bkt.put(d.key, marker)
		return deleted{versionID: aws.String(marker.versionID), deleteMarker: true}, nil
	default:
		delete(bkt.objects, d.key)
		return deleted{}, nil
	}
}

func (b *Backend) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer b.mutate()()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	res, f := b.deleteOne(bkt, deletion{
		key:          aws.ToString(in.Key),
		versionID:    aws.ToString(in.VersionId),
		bypass:       aws.ToBool(in.BypassGovernanceRetention),
		ifMatch:      in.IfMatch,
		lastModified: in.IfMatchLastModifiedTime,
		size:         in.IfMatchSize,
	})
	if f != nil {
		return nil, f.err()
	}
	out := &s3.DeleteObjectOutput{VersionId: res.versionID}
	if res.deleteMarker {
		out.DeleteMarker = aws.Bool(true)
	}
	return out, nil
}

func (b *Backend) DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.Delete == nil {
		return nil, malformedXML().err()
	}
	defer b.mutate()()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	out := &s3.DeleteObjectsOutput{}
	quiet := aws.ToBool(in.Delete.Quiet)
	for _, oi := range in.Delete.Objects {
		res, f := b.deleteOne(bkt, deletion{
			key:          aws.ToString(oi.Key),
			versionID:    aws.ToString(oi.VersionId),
			bypass:       aws.ToBool(in.BypassGovernanceRetention),
			ifMatch:      oi.ETag,
			lastModified: oi.LastModifiedTime,
			size:         oi.Size,
		})
		if f != nil {
			out.Errors = append(out.Errors, types.Error{
				Key: oi.Key, VersionId: oi.VersionId,
				Code: aws.String(f.code), Message: aws.String(f.message),
			})
			continue
		}
		if quiet {
			continue
		}
		d := types.DeletedObject{Key: oi.Key, VersionId: oi.VersionId}
		if res.deleteMarker {
			d.DeleteMarker = aws.Bool(true)
			d.DeleteMarkerVersionId = res.versionID
		}
		out.Deleted = append(out.Deleted, d)
	}
	return out, nil
}

func (b *Backend) GetObjectTagging(ctx context.Context, in *s3.GetObjectTaggingInput, _ ...func(*s3.Options)) (*s3.GetObjectTaggingOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	o, f := bkt.find(in.Key, in.VersionId)
	if f != nil {
		return nil, f.err()
	}
	return &s3.GetObjectTaggingOutput{TagSet: slices.Clone(o.tags), VersionId: bkt.versionIDOut(o)}, nil
}

func (b *Backend) PutObjectTagging(ctx context.Context, in *s3.PutObjectTaggingInput, _ ...func(*s3.Options)) (*s3.PutObjectTaggingOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.Tagging == nil {
		return nil, malformedXML().err()
	}
	defer b.mutate()()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	o, f := bkt.find(in.Key, in.VersionId)
	if f != nil {
		return nil, f.err()
	}
	o.tags = slices.Clone(in.Tagging.TagSet)
	return &s3.PutObjectTaggingOutput{VersionId: bkt.versionIDOut(o)}, nil
}

func (b *Backend) DeleteObjectTagging(ctx context.Context, in *s3.DeleteObjectTaggingInput, _ ...func(*s3.Options)) (*s3.DeleteObjectTaggingOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer b.mutate()()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	o, f := bkt.find(in.Key, in.VersionId)
	if f != nil {
		return nil, f.err()
	}
	o.tags = nil
	return &s3.DeleteObjectTaggingOutput{VersionId: bkt.versionIDOut(o)}, nil
}

func (b *Backend) GetObjectAttributes(ctx context.Context, in *s3.GetObjectAttributesInput, _ ...func(*s3.Options)) (*s3.GetObjectAttributesOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	o, f := bkt.find(in.Key, in.VersionId)
	if f != nil {
		return nil, f.err()
	}
	out := &s3.GetObjectAttributesOutput{
		LastModified: aws.Time(o.modified),
		VersionId:    bkt.versionIDOut(o),
	}
	for _, attr := range in.ObjectAttributes {
		switch attr {
		case types.ObjectAttributesEtag:
			out.ETag = aws.String(strings.Trim(o.etag, `"`))
		case types.ObjectAttributesChecksum:
			if len(o.checksums) > 0 {
				out.Checksum = &types.Checksum{ChecksumType: o.checksumType}
				sums := o.checksums
				if o.checksumType == types.ChecksumTypeComposite {
					// this one view gives a composite checksum without
					// its part count
					sums = checksum.Values{}
					for name, value := range o.checksums {
						sums[name], _, _ = strings.Cut(value, "-")
					}
				}
				setChecksums(out.Checksum, sums)
			}
		case types.ObjectAttributesObjectParts:
			if len(o.parts) > 0 {
				parts, f := attributeParts(o, in.MaxParts, in.PartNumberMarker)
				if f != nil {
					return nil, f.err()
				}
				out.ObjectParts = parts
			}
		case types.ObjectAttributesStorageClass:
			out.StorageClass = types.StorageClass(o.listedStorageClass())
		case types.ObjectAttributesObjectSize:
			out.ObjectSize = aws.Int64(o.size)
		}
	}
	return out, nil
}

func attributeParts(o *object, maxParts *int32, marker *string) (*types.GetObjectAttributesParts, *failure) {
	limit := 1000
	if maxParts != nil {
		if *maxParts < 0 {
			return nil, invalidArgument("Argument max-parts must be an integer between 0 and 2147483647")
		}
		limit = min(int(*maxParts), limit)
	}
	after := 0
	if marker != nil {
		n, err := strconv.Atoi(*marker)
		if err != nil || n < 0 {
			return nil, invalidArgument("Argument part-number-marker must be an integer between 0 and 2147483647")
		}
		after = n
	}
	out := &types.GetObjectAttributesParts{
		TotalPartsCount:  aws.Int32(int32(len(o.parts))),
		MaxParts:         aws.Int32(int32(limit)),
		PartNumberMarker: aws.String(strconv.Itoa(after)),
		IsTruncated:      aws.Bool(false),
	}
	last := after
	for i, p := range o.parts {
		n := i + 1
		if n <= after {
			continue
		}
		if len(out.Parts) == limit {
			out.IsTruncated = aws.Bool(true)
			break
		}
		part := types.ObjectPart{PartNumber: aws.Int32(int32(n)), Size: aws.Int64(p.size)}
		setChecksums(&part, p.checksums)
		out.Parts = append(out.Parts, part)
		last = n
	}
	out.NextPartNumberMarker = aws.String(strconv.Itoa(last))
	return out, nil
}
