package membackend

import (
	"context"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/fujiwara/s3rp/checksum"
)

// copySource resolves a CopySource ("bucket/escaped-key", optionally
// "?versionId=...") to the object it names.
func (b *Backend) copySource(source *string) (*bucket, string, *object, *failure) {
	invalid := invalidArgument("Invalid copy source object key")
	raw, query, _ := strings.Cut(aws.ToString(source), "?")
	path, err := url.PathUnescape(raw)
	if err != nil {
		return nil, "", nil, invalid
	}
	name, key, ok := strings.Cut(strings.TrimPrefix(path, "/"), "/")
	if !ok || key == "" {
		return nil, "", nil, invalid
	}
	q, err := url.ParseQuery(query)
	if err != nil {
		return nil, "", nil, invalid
	}
	bkt, f := b.getBucket(&name)
	if f != nil {
		return nil, "", nil, f
	}
	var versionID *string
	if v := q.Get("versionId"); v != "" {
		versionID = &v
	}
	o, f := bkt.find(&key, versionID)
	if f != nil {
		return nil, "", nil, f
	}
	return bkt, key, o, nil
}

// checkCopyPreconditions applies the x-amz-copy-source-if-* conditions,
// every failure of which is 412 on a copy.
func checkCopyPreconditions(o *object, ifMatch, ifNoneMatch *string, ifModifiedSince, ifUnmodifiedSince *time.Time) *failure {
	modified := o.modified
	if ifMatch != nil {
		if !etagMatches(*ifMatch, o.etag) {
			return preconditionFailed()
		}
	} else if ifUnmodifiedSince != nil && modified.After(*ifUnmodifiedSince) {
		return preconditionFailed()
	}
	if ifNoneMatch != nil {
		if etagMatches(*ifNoneMatch, o.etag) {
			return preconditionFailed()
		}
	} else if ifModifiedSince != nil && !modified.After(*ifModifiedSince) {
		return preconditionFailed()
	}
	return nil
}

func (b *Backend) CopyObject(ctx context.Context, in *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tags, f := parseTagging(in.Tagging)
	if f != nil {
		return nil, f.err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	srcBucket, srcKey, src, f := b.copySource(in.CopySource)
	if f != nil {
		return nil, f.err()
	}
	if f := checkCopyPreconditions(src, in.CopySourceIfMatch, in.CopySourceIfNoneMatch,
		in.CopySourceIfModifiedSince, in.CopySourceIfUnmodifiedSince); f != nil {
		return nil, f.err()
	}
	key := aws.ToString(in.Key)
	replaceMetadata := in.MetadataDirective == types.MetadataDirectiveReplace
	replaceTags := in.TaggingDirective == types.TaggingDirectiveReplace
	onto := srcBucket == bkt && srcKey == key && src == bkt.current(key)
	changes := replaceMetadata || replaceTags || in.StorageClass != "" || in.ServerSideEncryption != "" ||
		in.ChecksumAlgorithm != "" || in.ObjectLockMode != "" || in.ObjectLockLegalHoldStatus != ""
	if onto && !changes {
		return nil, fail(http.StatusBadRequest, "InvalidRequest",
			"This copy request is illegal because it is trying to copy an object to itself without changing the object's metadata, storage class, website redirect location or encryption attributes.").err()
	}

	// a copy is a single-part object whatever the source was assembled from
	o := &object{
		data:         src.data,
		etag:         md5ETag(src.data),
		content:      src.content,
		metadata:     src.metadata,
		storageClass: string(in.StorageClass),
		sse:          in.ServerSideEncryption,
		kmsKeyID:     aws.ToString(in.SSEKMSKeyId),
		tags:         src.tags,
		lockMode:     in.ObjectLockMode,
		retainUntil:  in.ObjectLockRetainUntilDate,
		legalHold:    in.ObjectLockLegalHoldStatus,
	}
	if replaceMetadata {
		o.content = contentAttrs{
			contentType:        in.ContentType,
			cacheControl:       in.CacheControl,
			contentDisposition: in.ContentDisposition,
			contentEncoding:    in.ContentEncoding,
			contentLanguage:    in.ContentLanguage,
			expires:            in.Expires,
		}
		o.metadata = in.Metadata
	}
	if replaceTags {
		o.tags = tags
	}
	// the copy keeps the source's checksum algorithm unless it names its
	// own; either way the value is of the whole object
	algorithms := checksumAlgorithms(src.checksums)
	if in.ChecksumAlgorithm != "" {
		algorithms = []types.ChecksumAlgorithm{types.ChecksumAlgorithm(strings.ToUpper(string(in.ChecksumAlgorithm)))}
	}
	for _, a := range algorithms {
		value, ok := computeChecksum(string(a), o.data)
		if !ok {
			return nil, unsupportedChecksum(string(a)).err()
		}
		if o.checksums == nil {
			o.checksums = checksum.Values{}
		}
		o.checksums[string(a)] = value
		o.checksumType = types.ChecksumTypeFullObject
	}
	if f := b.commit(bkt, key, o); f != nil {
		return nil, f.err()
	}
	result := &types.CopyObjectResult{
		ETag:         aws.String(o.etag),
		LastModified: aws.Time(o.modified),
		ChecksumType: o.checksumType,
	}
	setChecksums(result, o.checksums)
	return &s3.CopyObjectOutput{
		CopyObjectResult:     result,
		VersionId:            bkt.versionIDOut(o),
		CopySourceVersionId:  srcBucket.versionIDOut(src),
		ServerSideEncryption: o.sse,
		SSEKMSKeyId:          o.kmsKeyIDOut(),
	}, nil
}

// parseCopyRange reads x-amz-copy-source-range, which unlike Range must be
// one complete "bytes=first-last" inside the source.
func parseCopyRange(header string, size int64) (start, end int64, f *failure) {
	invalid := invalidArgument("The x-amz-copy-source-range value must be of the form bytes=first-last where first and last are the zero-based offsets of the first and last bytes to copy")
	spec, ok := strings.CutPrefix(header, "bytes=")
	if !ok {
		return 0, 0, invalid
	}
	first, last, ok := strings.Cut(spec, "-")
	if !ok {
		return 0, 0, invalid
	}
	start, err1 := strconv.ParseInt(first, 10, 64)
	l, err2 := strconv.ParseInt(last, 10, 64)
	if err1 != nil || err2 != nil || start < 0 || l < start {
		return 0, 0, invalid
	}
	if l >= size {
		return 0, 0, invalidArgument("Range specified is not valid for source object of size: " + strconv.FormatInt(size, 10))
	}
	return start, l + 1, nil
}

func (b *Backend) UploadPartCopy(ctx context.Context, in *s3.UploadPartCopyInput, _ ...func(*s3.Options)) (*s3.UploadPartCopyOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f := checkPartNumber(in.PartNumber); f != nil {
		return nil, f.err()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	u, f := bkt.findUpload(in.Key, in.UploadId)
	if f != nil {
		return nil, f.err()
	}
	srcBucket, _, src, f := b.copySource(in.CopySource)
	if f != nil {
		return nil, f.err()
	}
	if f := checkCopyPreconditions(src, in.CopySourceIfMatch, in.CopySourceIfNoneMatch,
		in.CopySourceIfModifiedSince, in.CopySourceIfUnmodifiedSince); f != nil {
		return nil, f.err()
	}
	data := src.data
	if in.CopySourceRange != nil {
		start, end, f := parseCopyRange(*in.CopySourceRange, int64(len(src.data)))
		if f != nil {
			return nil, f.err()
		}
		data = src.data[start:end]
	}
	sums, f := writeChecksums(data, nil, u.algorithm)
	if f != nil {
		return nil, f.err()
	}
	p := &part{data: data, etag: md5ETag(data), modified: b.clock(), checksums: sums}
	u.parts[*in.PartNumber] = p
	result := &types.CopyPartResult{ETag: aws.String(p.etag), LastModified: aws.Time(p.modified)}
	setChecksums(result, sums)
	return &s3.UploadPartCopyOutput{
		CopyPartResult:       result,
		CopySourceVersionId:  srcBucket.versionIDOut(src),
		ServerSideEncryption: u.proto.sse,
		SSEKMSKeyId:          u.proto.kmsKeyIDOut(),
	}, nil
}
