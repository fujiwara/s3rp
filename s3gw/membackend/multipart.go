package membackend

import (
	"cmp"
	"context"
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/fujiwara/s3rp/checksum"
)

// minPartSize is S3's minimum for every part but the last.
const minPartSize = 5 * 1024 * 1024

// maxPartNumber is S3's highest part number.
const maxPartNumber = 10000

type upload struct {
	id        string
	key       string
	initiated time.Time
	// proto carries what CreateMultipartUpload said about the object to be
	// assembled
	proto        *object
	algorithm    types.ChecksumAlgorithm
	checksumType types.ChecksumType
	parts        map[int32]*part
}

type part struct {
	data      []byte
	etag      string
	modified  time.Time
	checksums checksum.Values
}

func noSuchUpload() *failure {
	return fail(http.StatusNotFound, "NoSuchUpload",
		"The specified upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.")
}

func (bkt *bucket) findUpload(key, id *string) (*upload, *failure) {
	u, ok := bkt.uploads[aws.ToString(id)]
	if !ok || u.key != aws.ToString(key) {
		return nil, noSuchUpload()
	}
	return u, nil
}

func (b *Backend) CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	tags, f := parseTagging(in.Tagging)
	if f != nil {
		return nil, f.err()
	}
	algorithm := types.ChecksumAlgorithm(strings.ToUpper(string(in.ChecksumAlgorithm)))
	checksumType := in.ChecksumType
	if algorithm != "" {
		// a full-object checksum of parts needs an algorithm that combines:
		// the CRCs do, the SHAs can only be composed
		composable := algorithm == types.ChecksumAlgorithmSha1 || algorithm == types.ChecksumAlgorithmSha256
		switch checksumType {
		case "":
			checksumType = types.ChecksumTypeComposite
			if algorithm == types.ChecksumAlgorithmCrc64nvme {
				checksumType = types.ChecksumTypeFullObject
			}
		case types.ChecksumTypeFullObject:
			if composable {
				return nil, fail(http.StatusBadRequest, "InvalidRequest",
					fmt.Sprintf("The FULL_OBJECT checksum type cannot be used with the %s checksum algorithm.", strings.ToLower(string(algorithm)))).err()
			}
		case types.ChecksumTypeComposite:
			if algorithm == types.ChecksumAlgorithmCrc64nvme {
				return nil, fail(http.StatusBadRequest, "InvalidRequest",
					"The COMPOSITE checksum type cannot be used with the crc64nvme checksum algorithm.").err()
			}
		default:
			return nil, fail(http.StatusBadRequest, "InvalidRequest", "Value for x-amz-checksum-type header is invalid.").err()
		}
	}
	proto := &object{
		content: contentAttrs{
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
		lockMode:     in.ObjectLockMode,
		retainUntil:  in.ObjectLockRetainUntilDate,
		legalHold:    in.ObjectLockLegalHoldStatus,
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	now := b.clock()
	// refuse lock settings the bucket cannot honor now, not at completion
	probe := *proto
	if f := applyLockOnWrite(bkt, &probe, now); f != nil {
		return nil, f.err()
	}
	if proto.sse == "" && bkt.encryption != nil {
		def := bkt.encryption.Rules[0].ApplyServerSideEncryptionByDefault
		proto.sse, proto.kmsKeyID = def.SSEAlgorithm, aws.ToString(def.KMSMasterKeyID)
	}
	u := &upload{
		id:           newID(),
		key:          aws.ToString(in.Key),
		initiated:    now,
		proto:        proto,
		algorithm:    algorithm,
		checksumType: checksumType,
		parts:        map[int32]*part{},
	}
	bkt.uploads[u.id] = u
	return &s3.CreateMultipartUploadOutput{
		Bucket:               in.Bucket,
		Key:                  in.Key,
		UploadId:             aws.String(u.id),
		ChecksumAlgorithm:    algorithm,
		ChecksumType:         checksumType,
		ServerSideEncryption: proto.sse,
		SSEKMSKeyId:          proto.kmsKeyIDOut(),
	}, nil
}

func checkPartNumber(n *int32) *failure {
	if n == nil || *n < 1 || *n > maxPartNumber {
		return invalidArgument("Part number must be an integer between 1 and 10000, inclusive")
	}
	return nil
}

func (b *Backend) UploadPart(ctx context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f := checkPartNumber(in.PartNumber); f != nil {
		return nil, f.err()
	}
	data, err := readBody(in.Body, in.ContentLength)
	if err != nil {
		return nil, err
	}
	if f := checkContentMD5(data, in.ContentMD5); f != nil {
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
	algorithm := in.ChecksumAlgorithm
	if algorithm == "" {
		algorithm = u.algorithm
	}
	sums, f := writeChecksums(data, checksumsOf(in), algorithm)
	if f != nil {
		return nil, f.err()
	}
	p := &part{data: data, etag: md5ETag(data), modified: b.clock(), checksums: sums}
	u.parts[*in.PartNumber] = p
	out := &s3.UploadPartOutput{
		ETag:                 aws.String(p.etag),
		ServerSideEncryption: u.proto.sse,
		SSEKMSKeyId:          u.proto.kmsKeyIDOut(),
	}
	setChecksums(out, sums)
	return out, nil
}

// compositeChecksum is S3's checksum of checksums: the algorithm over the
// parts' raw digests, suffixed with the number of parts.
func compositeChecksum(name string, parts []*part) (string, bool) {
	h := checksum.NewHash(name)
	if h == nil {
		return "", false
	}
	for _, p := range parts {
		raw, err := base64.StdEncoding.DecodeString(p.checksums[name])
		if err != nil || len(raw) == 0 {
			return "", false
		}
		h.Write(raw)
	}
	return checksum.Base64(h) + "-" + strconv.Itoa(len(parts)), true
}

func (b *Backend) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.MultipartUpload == nil || len(in.MultipartUpload.Parts) == 0 {
		return nil, malformedXML().err()
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
	if in.ChecksumType != "" && in.ChecksumType != u.checksumType {
		return nil, fail(http.StatusBadRequest, "InvalidRequest",
			fmt.Sprintf("The upload was created using the %s checksum mode. The complete request must use the same checksum mode.", u.checksumType)).err()
	}
	invalidPart := fail(http.StatusBadRequest, "InvalidPart",
		"One or more of the specified parts could not be found.  The part may not have been uploaded, or the specified entity tag may not match the part's entity tag.")
	completed := in.MultipartUpload.Parts
	parts := make([]*part, 0, len(completed))
	var size int
	for i, cp := range completed {
		n := aws.ToInt32(cp.PartNumber)
		if i > 0 && n <= aws.ToInt32(completed[i-1].PartNumber) {
			return nil, fail(http.StatusBadRequest, "InvalidPartOrder",
				"The list of parts was not in ascending order. Parts must be ordered by part number.").err()
		}
		p, ok := u.parts[n]
		if !ok || strings.Trim(aws.ToString(cp.ETag), `"`) != strings.Trim(p.etag, `"`) {
			return nil, invalidPart.err()
		}
		for name, want := range checksumsOf(&cp) {
			if p.checksums[name] != want {
				return nil, invalidPart.err()
			}
		}
		parts = append(parts, p)
		size += len(p.data)
	}
	// only once every part is known to be there
	for _, p := range parts[:len(parts)-1] {
		if len(p.data) < minPartSize {
			return nil, fail(http.StatusBadRequest, "EntityTooSmall",
				"Your proposed upload is smaller than the minimum allowed size").err()
		}
	}
	if in.MpuObjectSize != nil && *in.MpuObjectSize != int64(size) {
		return nil, fail(http.StatusBadRequest, "InvalidRequest",
			"The provided 'x-amz-mp-object-size' header value does not match what was computed").err()
	}

	o := *u.proto
	o.data = make([]byte, 0, size)
	etag := md5.New()
	for _, p := range parts {
		o.data = append(o.data, p.data...)
		sum, _ := hex.DecodeString(strings.Trim(p.etag, `"`))
		etag.Write(sum)
		o.parts = append(o.parts, objectPart{size: int64(len(p.data)), checksums: p.checksums})
	}
	o.etag = `"` + hex.EncodeToString(etag.Sum(nil)) + "-" + strconv.Itoa(len(parts)) + `"`

	if name := string(u.algorithm); name != "" {
		var value string
		var ok bool
		if u.checksumType == types.ChecksumTypeFullObject {
			value, ok = computeChecksum(name, o.data)
		} else {
			value, ok = compositeChecksum(name, parts)
		}
		if ok {
			o.checksums, o.checksumType = checksum.Values{name: value}, u.checksumType
		}
	}
	// the checksum of the whole object, when the client states one
	for name, want := range checksumsOf(in) {
		if got, ok := o.checksums[name]; ok && got != want {
			return nil, fail(http.StatusBadRequest, "BadDigest",
				fmt.Sprintf("The %s you specified did not match the calculated checksum.", name)).err()
		}
	}

	if f := checkWritePreconditions(bkt.current(u.key), in.IfMatch, in.IfNoneMatch); f != nil {
		return nil, f.err()
	}
	if f := b.commit(bkt, u.key, &o); f != nil {
		return nil, f.err()
	}
	delete(bkt.uploads, u.id)
	bkt.finished[u.id] = u.key
	out := &s3.CompleteMultipartUploadOutput{
		Bucket:               in.Bucket,
		Key:                  in.Key,
		ETag:                 aws.String(o.etag),
		VersionId:            bkt.versionIDOut(&o),
		ServerSideEncryption: o.sse,
		SSEKMSKeyId:          o.kmsKeyIDOut(),
		ChecksumType:         o.checksumType,
	}
	setChecksums(out, o.checksums)
	return out, nil
}

func (b *Backend) AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	u, f := bkt.findUpload(in.Key, in.UploadId)
	if f != nil {
		if key, ok := bkt.finished[aws.ToString(in.UploadId)]; ok && key == aws.ToString(in.Key) {
			return &s3.AbortMultipartUploadOutput{}, nil
		}
		return nil, f.err()
	}
	delete(bkt.uploads, u.id)
	bkt.finished[u.id] = u.key
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (b *Backend) ListParts(ctx context.Context, in *s3.ListPartsInput, _ ...func(*s3.Options)) (*s3.ListPartsOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit, f := pageSize(in.MaxParts, "max-parts")
	if f != nil {
		return nil, f.err()
	}
	after := 0
	if in.PartNumberMarker != nil {
		n, err := strconv.Atoi(*in.PartNumberMarker)
		if err != nil || n < 0 {
			return nil, invalidArgument("Argument part-number-marker must be an integer between 0 and 2147483647").err()
		}
		after = n
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	u, f := bkt.findUpload(in.Key, in.UploadId)
	if f != nil {
		return nil, f.err()
	}
	numbers := make([]int32, 0, len(u.parts))
	for n := range u.parts {
		if int(n) > after {
			numbers = append(numbers, n)
		}
	}
	slices.Sort(numbers)
	out := &s3.ListPartsOutput{
		Bucket:            in.Bucket,
		Key:               in.Key,
		UploadId:          in.UploadId,
		StorageClass:      types.StorageClass(u.proto.listedStorageClass()),
		PartNumberMarker:  aws.String(strconv.Itoa(after)),
		MaxParts:          aws.Int32(int32(limit)),
		IsTruncated:       aws.Bool(len(numbers) > limit),
		ChecksumAlgorithm: u.algorithm,
		ChecksumType:      u.checksumType,
	}
	last := after
	for _, n := range numbers[:min(len(numbers), limit)] {
		p := u.parts[n]
		entry := types.Part{
			PartNumber:   aws.Int32(n),
			ETag:         aws.String(p.etag),
			Size:         aws.Int64(int64(len(p.data))),
			LastModified: aws.Time(p.modified),
		}
		setChecksums(&entry, p.checksums)
		out.Parts = append(out.Parts, entry)
		last = int(n)
	}
	out.NextPartNumberMarker = aws.String(strconv.Itoa(last))
	return out, nil
}

func (b *Backend) ListMultipartUploads(ctx context.Context, in *s3.ListMultipartUploadsInput, _ ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit, f := pageSize(in.MaxUploads, "max-uploads")
	if f != nil {
		return nil, f.err()
	}
	encode, f := encoder(in.EncodingType)
	if f != nil {
		return nil, f.err()
	}
	prefix, delimiter := aws.ToString(in.Prefix), aws.ToString(in.Delimiter)
	keyMarker, idMarker := aws.ToString(in.KeyMarker), aws.ToString(in.UploadIdMarker)

	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	uploads := make([]*upload, 0, len(bkt.uploads))
	for _, u := range bkt.uploads {
		if strings.HasPrefix(u.key, prefix) {
			uploads = append(uploads, u)
		}
	}
	// by key, and within a key in the order the uploads were started
	slices.SortFunc(uploads, func(a, b *upload) int {
		return cmp.Or(strings.Compare(a.key, b.key), a.initiated.Compare(b.initiated), strings.Compare(a.id, b.id))
	})
	out := &s3.ListMultipartUploadsOutput{
		Bucket:         in.Bucket,
		Prefix:         optional(encode, in.Prefix),
		Delimiter:      optional(encode, in.Delimiter),
		KeyMarker:      optional(encode, in.KeyMarker),
		UploadIdMarker: in.UploadIdMarker,
		MaxUploads:     aws.Int32(int32(limit)),
		IsTruncated:    aws.Bool(false),
		EncodingType:   in.EncodingType,
	}
	if limit == 0 {
		return out, nil
	}
	// with an upload id marker the listing resumes inside the marker key,
	// after that upload; without one it resumes at the next key
	passed := false
	behind := func(u *upload) bool {
		switch {
		case keyMarker == "" || u.key > keyMarker:
			return false
		case u.key < keyMarker || idMarker == "":
			return true
		case passed:
			return false
		}
		passed = u.id == idMarker
		return true
	}
	count := 0
	var lastPrefix string
	for _, u := range uploads {
		if behind(u) {
			continue
		}
		cp, rolled := commonPrefix(u.key, prefix, delimiter)
		if rolled && (cp == lastPrefix || cp == keyMarker) {
			continue
		}
		if count == limit {
			out.IsTruncated = aws.Bool(true)
			break
		}
		count++
		if rolled {
			out.CommonPrefixes = append(out.CommonPrefixes, types.CommonPrefix{Prefix: encode(cp)})
			out.NextKeyMarker, out.NextUploadIdMarker = encode(cp), nil
			lastPrefix = cp
			continue
		}
		out.Uploads = append(out.Uploads, types.MultipartUpload{
			Key:               encode(u.key),
			UploadId:          aws.String(u.id),
			Initiated:         aws.Time(u.initiated),
			StorageClass:      types.StorageClass(u.proto.listedStorageClass()),
			ChecksumAlgorithm: u.algorithm,
			ChecksumType:      u.checksumType,
		})
		out.NextKeyMarker, out.NextUploadIdMarker = encode(u.key), aws.String(u.id)
	}
	if !aws.ToBool(out.IsTruncated) {
		out.NextKeyMarker, out.NextUploadIdMarker = nil, nil
	}
	return out, nil
}
