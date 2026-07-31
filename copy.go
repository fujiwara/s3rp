package s3rp

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// resolveCopySource resolves an x-amz-copy-source header value
// (front bucket/key) to the backend copy source of the same backend.
// Copying between different backends is not supported.
func (app *S3RP) resolveCopySource(r *http.Request, vr *verifiedRequest, dst *bucketRT) (string, *S3Error) {
	raw := strings.TrimPrefix(r.Header.Get("x-amz-copy-source"), "/")
	rawPath, versionID, _ := strings.Cut(raw, "?")
	rawBucket, rawKey, ok := strings.Cut(rawPath, "/")
	if !ok || rawKey == "" {
		return "", newS3Error(http.StatusBadRequest, "InvalidArgument",
			"Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}
	srcBucket, err := url.PathUnescape(rawBucket)
	if err != nil {
		return "", newS3Error(http.StatusBadRequest, "InvalidArgument", "Invalid copy source")
	}
	srcKey, err := unescapeKey(rawKey)
	if err != nil {
		return "", newS3Error(http.StatusBadRequest, "InvalidArgument", "Invalid copy source")
	}
	src := app.keys[vr.AccessKeyID].buckets[srcBucket]
	if src == nil {
		return "", errAccessDenied()
	}
	sb, db := src.cfg.Backend, dst.cfg.Backend
	if sb.Endpoint != db.Endpoint || sb.Region != db.Region || sb.AccessKeyID != db.AccessKeyID {
		return "", errNotImplemented("copying between different backends")
	}
	copySource := (&url.URL{Path: "/" + sb.Bucket + "/" + srcKey}).EscapedPath()[1:]
	if versionID != "" {
		copySource += "?" + versionID
	}
	return copySource, nil
}

func (app *S3RP) copyObject(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string, vr *verifiedRequest) error {
	copySource, s3err := app.resolveCopySource(r, vr, rt)
	if s3err != nil {
		return s3err
	}
	in := &s3.CopyObjectInput{
		Bucket:     aws.String(rt.cfg.Backend.Bucket),
		Key:        aws.String(key),
		CopySource: aws.String(copySource),
	}
	if v := r.Header.Get("x-amz-metadata-directive"); v != "" {
		in.MetadataDirective = types.MetadataDirective(v)
	}
	if v := r.Header.Get("Content-Type"); v != "" {
		in.ContentType = aws.String(v)
	}
	if v := r.Header.Get("x-amz-storage-class"); v != "" {
		in.StorageClass = types.StorageClass(v)
	}
	if v := r.Header.Get("x-amz-tagging-directive"); v != "" {
		in.TaggingDirective = types.TaggingDirective(v)
	}
	if v := r.Header.Get("x-amz-tagging"); v != "" {
		in.Tagging = aws.String(v)
	}
	if md := metadataFromHeaders(r.Header); len(md) > 0 {
		in.Metadata = md
	}
	if v := r.Header.Get("x-amz-copy-source-if-match"); v != "" {
		in.CopySourceIfMatch = aws.String(v)
	}
	if v := r.Header.Get("x-amz-copy-source-if-none-match"); v != "" {
		in.CopySourceIfNoneMatch = aws.String(v)
	}
	if v := r.Header.Get("x-amz-copy-source-if-modified-since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			in.CopySourceIfModifiedSince = aws.Time(t)
		}
	}
	if v := r.Header.Get("x-amz-copy-source-if-unmodified-since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			in.CopySourceIfUnmodifiedSince = aws.Time(t)
		}
	}
	out, err := rt.client.CopyObject(r.Context(), in)
	if err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	if out.CopySourceVersionId != nil {
		w.Header().Set("x-amz-copy-source-version-id", *out.CopySourceVersionId)
	}
	result := &CopyObjectResult{XMLNS: s3XMLNS}
	if cr := out.CopyObjectResult; cr != nil {
		if cr.ETag != nil {
			result.ETag = *cr.ETag
		}
		if cr.LastModified != nil {
			result.LastModified = s3Time(*cr.LastModified)
		}
	}
	return writeXML(w, result)
}

func (app *S3RP) uploadPartCopy(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string, vr *verifiedRequest) error {
	copySource, s3err := app.resolveCopySource(r, vr, rt)
	if s3err != nil {
		return s3err
	}
	query := r.URL.Query()
	partNumber, err := strconv.ParseInt(query.Get("partNumber"), 10, 32)
	if err != nil {
		return newS3Error(http.StatusBadRequest, "InvalidArgument",
			"Argument partNumber must be an integer.")
	}
	in := &s3.UploadPartCopyInput{
		Bucket:     aws.String(rt.cfg.Backend.Bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(query.Get("uploadId")),
		PartNumber: aws.Int32(int32(partNumber)),
		CopySource: aws.String(copySource),
	}
	if v := r.Header.Get("x-amz-copy-source-range"); v != "" {
		in.CopySourceRange = aws.String(v)
	}
	out, err := rt.client.UploadPartCopy(r.Context(), in)
	if err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	if out.CopySourceVersionId != nil {
		w.Header().Set("x-amz-copy-source-version-id", *out.CopySourceVersionId)
	}
	result := &CopyPartResult{XMLNS: s3XMLNS}
	if cr := out.CopyPartResult; cr != nil {
		if cr.ETag != nil {
			result.ETag = *cr.ETag
		}
		if cr.LastModified != nil {
			result.LastModified = s3Time(*cr.LastModified)
		}
	}
	return writeXML(w, result)
}
