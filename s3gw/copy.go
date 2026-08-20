package s3gw

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"
	"github.com/fujiwara/s3rp/store"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// resolveCopySource resolves an x-amz-copy-source header value
// (front bucket/key) to the backend copy source of the same backend.
// Copying between different backends is not supported. The copy source names
// another object to read, so it is only ever taken from a signed header.
func (g *Gateway) resolveCopySource(c *opCtx) (string, *s3err.Error) {
	raw := strings.TrimPrefix(c.signed("x-amz-copy-source"), "/")
	rawPath, versionID, _ := strings.Cut(raw, "?")
	rawBucket, rawKey, ok := strings.Cut(rawPath, "/")
	if !ok || rawKey == "" {
		return "", s3err.New(http.StatusBadRequest, "InvalidArgument",
			"Copy Source must mention the source bucket and key: sourcebucket/sourcekey")
	}
	srcBucket, err := url.PathUnescape(rawBucket)
	if err != nil {
		return "", s3err.New(http.StatusBadRequest, "InvalidArgument", "Invalid copy source").WithCause(err)
	}
	srcKey, err := unescapeKey(rawKey)
	if err != nil {
		return "", s3err.New(http.StatusBadRequest, "InvalidArgument", "Invalid copy source").WithCause(err)
	}
	// the source is resolved within the requesting key's tenant — deliberately
	// not through resolveBucket — so copying from another tenant's bucket is
	// impossible by construction even where a policy grants cross-tenant reads
	src, err := g.store.GetBucket(c.r.Context(), c.vr.Tenant, srcBucket)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// the cause tells the observer this denial is an unresolved
			// source bucket, not a policy decision; the client sees neither
			return "", s3err.AccessDenied().WithCause(err)
		}
		return "", s3err.Internal(err, "bucket lookup failed")
	}
	// reading the copy source needs s3:GetObject on the source bucket
	if s3e := g.authorize(c.vr, src, "s3:GetObject", src.Name+"/"+srcKey); s3e != nil {
		return "", s3e
	}
	sb, db := src.Backend, c.rt.cfg.Backend
	if sb.Endpoint != db.Endpoint || sb.Region != db.Region || sb.AccessKeyID != db.AccessKeyID {
		return "", s3err.NotImplemented("copying between different backends")
	}
	copySource := (&url.URL{Path: "/" + sb.Bucket + "/" + srcKey}).EscapedPath()[1:]
	if versionID != "" {
		copySource += "?" + versionID
	}
	return copySource, nil
}

func (g *Gateway) copyObject(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	copySource, s3e := g.resolveCopySource(c)
	if s3e != nil {
		return s3e
	}
	in := &s3.CopyObjectInput{
		Bucket:     aws.String(rt.cfg.Backend.Bucket),
		Key:        aws.String(key),
		CopySource: aws.String(copySource),
	}
	if v := c.signed("x-amz-metadata-directive"); v != "" {
		in.MetadataDirective = types.MetadataDirective(v)
	}
	if v := c.attr("Content-Type"); v != "" {
		in.ContentType = aws.String(v)
	}
	if v := c.signed("x-amz-storage-class"); v != "" {
		in.StorageClass = types.StorageClass(v)
	}
	if v := c.signed("x-amz-tagging-directive"); v != "" {
		in.TaggingDirective = types.TaggingDirective(v)
	}
	if v := c.signed("x-amz-tagging"); v != "" {
		in.Tagging = aws.String(v)
	}
	if md := c.hdr.AmzMeta(); len(md) > 0 {
		in.Metadata = md
	}
	if v := c.signed("x-amz-copy-source-if-match"); v != "" {
		in.CopySourceIfMatch = aws.String(v)
	}
	if v := c.signed("x-amz-copy-source-if-none-match"); v != "" {
		in.CopySourceIfNoneMatch = aws.String(v)
	}
	if v := c.signed("x-amz-copy-source-if-modified-since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			in.CopySourceIfModifiedSince = aws.Time(t)
		}
	}
	if v := c.signed("x-amz-copy-source-if-unmodified-since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			in.CopySourceIfUnmodifiedSince = aws.Time(t)
		}
	}
	if s3e := applySSE(c.signed, &in.ServerSideEncryption, &in.SSEKMSKeyId); s3e != nil {
		return s3e
	}
	out, err := rt.client.CopyObject(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	setSSEHeaders(w.Header(), out.ServerSideEncryption, out.SSEKMSKeyId)
	if out.CopySourceVersionId != nil {
		w.Header().Set("x-amz-copy-source-version-id", *out.CopySourceVersionId)
	}
	result := &s3xml.CopyObjectResult{XMLNS: s3xml.Namespace}
	if cr := out.CopyObjectResult; cr != nil {
		if cr.ETag != nil {
			result.ETag = *cr.ETag
		}
		if cr.LastModified != nil {
			result.LastModified = s3xml.FormatTime(*cr.LastModified)
		}
	}
	return s3xml.Write(w, result)
}

func (g *Gateway) uploadPartCopy(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	copySource, s3e := g.resolveCopySource(c)
	if s3e != nil {
		return s3e
	}
	query := r.URL.Query()
	partNumber, err := strconv.ParseInt(query.Get("partNumber"), 10, 32)
	if err != nil {
		return s3err.New(http.StatusBadRequest, "InvalidArgument",
			"Argument partNumber must be an integer.")
	}
	in := &s3.UploadPartCopyInput{
		Bucket:     aws.String(rt.cfg.Backend.Bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(query.Get("uploadId")),
		PartNumber: aws.Int32(int32(partNumber)),
		CopySource: aws.String(copySource),
	}
	if v := c.signed("x-amz-copy-source-range"); v != "" {
		in.CopySourceRange = aws.String(v)
	}
	out, err := rt.client.UploadPartCopy(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	if out.CopySourceVersionId != nil {
		w.Header().Set("x-amz-copy-source-version-id", *out.CopySourceVersionId)
	}
	setSSEHeaders(w.Header(), out.ServerSideEncryption, out.SSEKMSKeyId)
	result := &s3xml.CopyPartResult{XMLNS: s3xml.Namespace}
	if cr := out.CopyPartResult; cr != nil {
		if cr.ETag != nil {
			result.ETag = *cr.ETag
		}
		if cr.LastModified != nil {
			result.LastModified = s3xml.FormatTime(*cr.LastModified)
		}
	}
	return s3xml.Write(w, result)
}
