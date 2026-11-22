package membackend

import (
	"context"
	"encoding/base64"
	"net/url"
	"slices"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// maxListEntries is S3's page-size limit and default for every listing.
const maxListEntries = 1000

func pageSize(requested *int32, name string) (int, *failure) {
	if requested == nil {
		return maxListEntries, nil
	}
	if *requested < 0 {
		return 0, invalidArgument("Argument " + name + " must be an integer between 0 and 2147483647")
	}
	return min(int(*requested), maxListEntries), nil
}

// encoder returns the spelling of keys in a listing: as they are, or
// URL-encoded when the request asked for encoding-type=url.
func encoder(t types.EncodingType) (func(string) *string, *failure) {
	switch t {
	case "":
		return func(s string) *string { return aws.String(s) }, nil
	case types.EncodingTypeUrl:
		return func(s string) *string {
			return aws.String(strings.ReplaceAll(url.QueryEscape(s), "%2F", "/"))
		}, nil
	}
	return nil, invalidArgument("Invalid Encoding Method specified in Request")
}

func optional(encode func(string) *string, s *string) *string {
	if s == nil {
		return nil
	}
	return encode(*s)
}

// commonPrefix is the prefix a key rolls up into under a delimiter: the
// key up to and including the first delimiter after the listing prefix.
func commonPrefix(key, prefix, delimiter string) (string, bool) {
	if delimiter == "" {
		return "", false
	}
	i := strings.Index(key[len(prefix):], delimiter)
	if i < 0 {
		return "", false
	}
	return key[:len(prefix)+i+len(delimiter)], true
}

// sortedKeys is the bucket's keys under prefix, in the UTF-8 binary order
// S3 lists in. live limits it to keys whose current version is an object.
func (bkt *bucket) sortedKeys(prefix string, live bool) []string {
	keys := make([]string, 0, len(bkt.objects))
	for k, vs := range bkt.objects {
		if !strings.HasPrefix(k, prefix) || (live && vs[0].deleteMarker) {
			continue
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

type listed struct {
	name   string
	prefix bool
}

// page walks sorted keys after a marker and gathers one page of keys and
// common prefixes, each counting once. A common prefix that does not sort
// after the marker is behind it, with every key it rolls up: the marker was
// that prefix (the last entry of the previous page) or a key inside it.
func page(keys []string, prefix, delimiter, after string, limit int) (entries []listed, truncated bool) {
	if limit == 0 {
		return nil, false
	}
	var lastPrefix string
	for _, k := range keys {
		if after != "" && k <= after {
			continue
		}
		cp, rolled := commonPrefix(k, prefix, delimiter)
		if rolled && (cp == lastPrefix || (after != "" && cp <= after)) {
			continue
		}
		if len(entries) == limit {
			return entries, true
		}
		if rolled {
			entries = append(entries, listed{name: cp, prefix: true})
			lastPrefix = cp
		} else {
			entries = append(entries, listed{name: k})
		}
	}
	return entries, false
}

func (bkt *bucket) listEntry(key string, encode func(string) *string) types.Object {
	o := bkt.objects[key][0]
	return types.Object{
		Key:               encode(key),
		ETag:              aws.String(o.etag),
		Size:              aws.Int64(int64(len(o.data))),
		LastModified:      aws.Time(o.modified),
		StorageClass:      types.ObjectStorageClass(o.listedStorageClass()),
		ChecksumAlgorithm: checksumAlgorithms(o.checksums),
		ChecksumType:      o.checksumType,
	}
}

func (b *Backend) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit, f := pageSize(in.MaxKeys, "max-keys")
	if f != nil {
		return nil, f.err()
	}
	encode, f := encoder(in.EncodingType)
	if f != nil {
		return nil, f.err()
	}
	after := aws.ToString(in.StartAfter)
	if in.ContinuationToken != nil {
		token, err := base64.RawURLEncoding.DecodeString(*in.ContinuationToken)
		if err != nil {
			return nil, invalidArgument("The continuation token provided is incorrect").err()
		}
		after = max(after, string(token))
	}
	prefix, delimiter := aws.ToString(in.Prefix), aws.ToString(in.Delimiter)

	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	entries, truncated := page(bkt.sortedKeys(prefix, true), prefix, delimiter, after, limit)
	out := &s3.ListObjectsV2Output{
		Name:              in.Bucket,
		Prefix:            optional(encode, in.Prefix),
		Delimiter:         optional(encode, in.Delimiter),
		StartAfter:        optional(encode, in.StartAfter),
		ContinuationToken: in.ContinuationToken,
		MaxKeys:           aws.Int32(int32(limit)),
		KeyCount:          aws.Int32(int32(len(entries))),
		IsTruncated:       aws.Bool(truncated),
		EncodingType:      in.EncodingType,
	}
	for _, e := range entries {
		if e.prefix {
			out.CommonPrefixes = append(out.CommonPrefixes, types.CommonPrefix{Prefix: encode(e.name)})
		} else {
			out.Contents = append(out.Contents, bkt.listEntry(e.name, encode))
		}
	}
	if truncated {
		out.NextContinuationToken = aws.String(base64.RawURLEncoding.EncodeToString([]byte(entries[len(entries)-1].name)))
	}
	return out, nil
}

func (b *Backend) ListObjects(ctx context.Context, in *s3.ListObjectsInput, _ ...func(*s3.Options)) (*s3.ListObjectsOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit, f := pageSize(in.MaxKeys, "max-keys")
	if f != nil {
		return nil, f.err()
	}
	encode, f := encoder(in.EncodingType)
	if f != nil {
		return nil, f.err()
	}
	prefix, delimiter := aws.ToString(in.Prefix), aws.ToString(in.Delimiter)

	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	entries, truncated := page(bkt.sortedKeys(prefix, true), prefix, delimiter, aws.ToString(in.Marker), limit)
	out := &s3.ListObjectsOutput{
		Name:         in.Bucket,
		Prefix:       optional(encode, in.Prefix),
		Delimiter:    optional(encode, in.Delimiter),
		Marker:       optional(encode, in.Marker),
		MaxKeys:      aws.Int32(int32(limit)),
		IsTruncated:  aws.Bool(truncated),
		EncodingType: in.EncodingType,
	}
	for _, e := range entries {
		if e.prefix {
			out.CommonPrefixes = append(out.CommonPrefixes, types.CommonPrefix{Prefix: encode(e.name)})
		} else {
			out.Contents = append(out.Contents, bkt.listEntry(e.name, encode))
		}
	}
	// S3 names the next marker only when a delimiter is in play; without
	// one the client continues from the last key it was given
	if truncated && delimiter != "" {
		out.NextMarker = encode(entries[len(entries)-1].name)
	}
	return out, nil
}

func (b *Backend) ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit, f := pageSize(in.MaxKeys, "max-keys")
	if f != nil {
		return nil, f.err()
	}
	encode, f := encoder(in.EncodingType)
	if f != nil {
		return nil, f.err()
	}
	prefix, delimiter := aws.ToString(in.Prefix), aws.ToString(in.Delimiter)
	keyMarker, versionMarker := aws.ToString(in.KeyMarker), aws.ToString(in.VersionIdMarker)
	if versionMarker != "" && keyMarker == "" {
		return nil, invalidArgument("A version-id marker cannot be specified without a key marker.").err()
	}

	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	out := &s3.ListObjectVersionsOutput{
		Name:            in.Bucket,
		Prefix:          optional(encode, in.Prefix),
		Delimiter:       optional(encode, in.Delimiter),
		KeyMarker:       optional(encode, in.KeyMarker),
		VersionIdMarker: in.VersionIdMarker,
		MaxKeys:         aws.Int32(int32(limit)),
		IsTruncated:     aws.Bool(false),
		EncodingType:    in.EncodingType,
	}
	if limit == 0 {
		return out, nil
	}
	count := 0
	var lastPrefix string
	// full reports that the page is complete, recording where the next
	// one starts
	full := func() bool {
		if count < limit {
			return false
		}
		out.IsTruncated = aws.Bool(true)
		return true
	}
keys:
	for _, k := range bkt.sortedKeys(prefix, false) {
		versions := bkt.objects[k]
		switch {
		case keyMarker == "" || k > keyMarker:
		case k == keyMarker && versionMarker != "":
			// resume inside the marker key, after the marker version
			i := slices.IndexFunc(versions, func(o *object) bool { return o.versionID == versionMarker })
			if i < 0 {
				continue
			}
			versions = versions[i+1:]
		default:
			continue
		}
		if cp, rolled := commonPrefix(k, prefix, delimiter); rolled {
			if cp == lastPrefix || (keyMarker != "" && cp <= keyMarker) {
				continue
			}
			if full() {
				break
			}
			out.CommonPrefixes = append(out.CommonPrefixes, types.CommonPrefix{Prefix: encode(cp)})
			out.NextKeyMarker, out.NextVersionIdMarker = encode(cp), nil
			lastPrefix = cp
			count++
			continue
		}
		for _, o := range versions {
			if full() {
				break keys
			}
			latest := aws.Bool(o == bkt.objects[k][0])
			if o.deleteMarker {
				out.DeleteMarkers = append(out.DeleteMarkers, types.DeleteMarkerEntry{
					Key:          encode(k),
					VersionId:    aws.String(o.versionID),
					IsLatest:     latest,
					LastModified: aws.Time(o.modified),
				})
			} else {
				out.Versions = append(out.Versions, types.ObjectVersion{
					Key:               encode(k),
					VersionId:         aws.String(o.versionID),
					IsLatest:          latest,
					ETag:              aws.String(o.etag),
					Size:              aws.Int64(int64(len(o.data))),
					LastModified:      aws.Time(o.modified),
					StorageClass:      types.ObjectVersionStorageClass(o.listedStorageClass()),
					ChecksumAlgorithm: checksumAlgorithms(o.checksums),
					ChecksumType:      o.checksumType,
				})
			}
			out.NextKeyMarker, out.NextVersionIdMarker = encode(k), aws.String(o.versionID)
			count++
		}
	}
	if !aws.ToBool(out.IsTruncated) {
		out.NextKeyMarker, out.NextVersionIdMarker = nil, nil
	}
	return out, nil
}
