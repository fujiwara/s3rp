package s3gw

import (
	"crypto/md5"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"hash"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fujiwara/s3rp/checksum"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"
	"github.com/fujiwara/s3rp/sigv4"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func (g *Gateway) getObject(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	in := &s3.GetObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	applyConditionalHeaders(c.hdr, &in.IfMatch, &in.IfNoneMatch, &in.IfModifiedSince, &in.IfUnmodifiedSince)
	if v := c.attr("Range"); v != "" {
		in.Range = aws.String(v)
	}
	if strings.EqualFold(c.signed("x-amz-checksum-mode"), "enabled") {
		in.ChecksumMode = types.ChecksumModeEnabled
	}
	query := r.URL.Query()
	if v := query.Get(qpVersionID); v != "" {
		in.VersionId = aws.String(v)
	}
	if s3e := applyPartNumber(query, &in.PartNumber); s3e != nil {
		return s3e
	}
	if v := query.Get("response-content-type"); v != "" {
		in.ResponseContentType = aws.String(v)
	}
	if v := query.Get("response-content-disposition"); v != "" {
		in.ResponseContentDisposition = aws.String(v)
	}
	if v := query.Get("response-cache-control"); v != "" {
		in.ResponseCacheControl = aws.String(v)
	}
	if v := query.Get("response-content-encoding"); v != "" {
		in.ResponseContentEncoding = aws.String(v)
	}
	if v := query.Get("response-content-language"); v != "" {
		in.ResponseContentLanguage = aws.String(v)
	}
	if v := query.Get("response-expires"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			in.ResponseExpires = aws.Time(t)
		}
	}
	out, err := rt.client.GetObject(r.Context(), in)
	if err != nil {
		relayErrorHeaders(w.Header(), err)
		return s3err.FromSDKError(err, r.URL.Path)
	}
	defer out.Body.Close()
	c.setResponse(OpResponse{
		SSE:          string(out.ServerSideEncryption),
		SSEKMSKeyID:  aws.ToString(out.SSEKMSKeyId),
		StorageClass: string(out.StorageClass),
		Metadata:     out.Metadata,
		ETag:         aws.ToString(out.ETag),
		VersionID:    aws.ToString(out.VersionId),
	})

	h := w.Header()
	setObjectHeaders(h, objectHeaderValues{
		ContentType:        out.ContentType,
		ContentLength:      out.ContentLength,
		ETag:               out.ETag,
		LastModified:       out.LastModified,
		CacheControl:       out.CacheControl,
		ContentDisposition: out.ContentDisposition,
		ContentEncoding:    out.ContentEncoding,
		ContentLanguage:    out.ContentLanguage,
		Expires:            out.ExpiresString,
		StorageClass:       string(out.StorageClass),
		VersionID:          out.VersionId,
		SSE:                out.ServerSideEncryption,
		SSEKMSKeyID:        out.SSEKMSKeyId,
		Metadata:           out.Metadata,
	})
	if out.AcceptRanges != nil {
		h.Set("Accept-Ranges", *out.AcceptRanges)
	}
	if out.TagCount != nil {
		h.Set("x-amz-tagging-count", strconv.FormatInt(int64(*out.TagCount), 10))
	}
	if out.PartsCount != nil {
		h.Set("x-amz-mp-parts-count", strconv.FormatInt(int64(*out.PartsCount), 10))
	}
	checksum.SetHeaders(h, checksum.Values{
		CRC32:     out.ChecksumCRC32,
		CRC32C:    out.ChecksumCRC32C,
		CRC64NVME: out.ChecksumCRC64NVME,
		SHA1:      out.ChecksumSHA1,
		SHA256:    out.ChecksumSHA256,
	}, string(out.ChecksumType))
	setObjectLockResponseHeaders(h, out.ObjectLockMode, out.ObjectLockRetainUntilDate, out.ObjectLockLegalHoldStatus)
	status := http.StatusOK
	if out.ContentRange != nil {
		h.Set("Content-Range", *out.ContentRange)
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	if _, err := io.Copy(w, out.Body); err != nil {
		// response is already in flight; the client sees a broken body
		slog.WarnContext(r.Context(), "failed to copy object body", "error", err)
	}
	return nil
}

func (g *Gateway) headObject(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	in := &s3.HeadObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	applyConditionalHeaders(c.hdr, &in.IfMatch, &in.IfNoneMatch, &in.IfModifiedSince, &in.IfUnmodifiedSince)
	if v := c.attr("Range"); v != "" {
		in.Range = aws.String(v)
	}
	if v := r.URL.Query().Get(qpVersionID); v != "" {
		in.VersionId = aws.String(v)
	}
	if s3e := applyPartNumber(r.URL.Query(), &in.PartNumber); s3e != nil {
		return s3e
	}
	if strings.EqualFold(c.signed("x-amz-checksum-mode"), "enabled") {
		in.ChecksumMode = types.ChecksumModeEnabled
	}
	out, err := rt.client.HeadObject(r.Context(), in)
	if err != nil {
		relayErrorHeaders(w.Header(), err)
		return s3err.FromSDKError(err, r.URL.Path)
	}
	c.setResponse(OpResponse{
		SSE:          string(out.ServerSideEncryption),
		SSEKMSKeyID:  aws.ToString(out.SSEKMSKeyId),
		StorageClass: string(out.StorageClass),
		Metadata:     out.Metadata,
		ETag:         aws.ToString(out.ETag),
		VersionID:    aws.ToString(out.VersionId),
	})
	setObjectHeaders(w.Header(), objectHeaderValues{
		ContentType:        out.ContentType,
		ContentLength:      out.ContentLength,
		ETag:               out.ETag,
		LastModified:       out.LastModified,
		CacheControl:       out.CacheControl,
		ContentDisposition: out.ContentDisposition,
		ContentEncoding:    out.ContentEncoding,
		ContentLanguage:    out.ContentLanguage,
		Expires:            out.ExpiresString,
		StorageClass:       string(out.StorageClass),
		VersionID:          out.VersionId,
		SSE:                out.ServerSideEncryption,
		SSEKMSKeyID:        out.SSEKMSKeyId,
		Metadata:           out.Metadata,
	})
	if out.AcceptRanges != nil {
		w.Header().Set("Accept-Ranges", *out.AcceptRanges)
	}
	if out.PartsCount != nil {
		w.Header().Set("x-amz-mp-parts-count", strconv.FormatInt(int64(*out.PartsCount), 10))
	}
	checksum.SetHeaders(w.Header(), checksum.Values{
		CRC32:     out.ChecksumCRC32,
		CRC32C:    out.ChecksumCRC32C,
		CRC64NVME: out.ChecksumCRC64NVME,
		SHA1:      out.ChecksumSHA1,
		SHA256:    out.ChecksumSHA256,
	}, string(out.ChecksumType))
	setObjectLockResponseHeaders(w.Header(), out.ObjectLockMode, out.ObjectLockRetainUntilDate, out.ObjectLockLegalHoldStatus)
	w.WriteHeader(http.StatusOK)
	return nil
}

func (g *Gateway) putObject(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	in := &s3.PutObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	body, length, s3e := requestBody(c)
	if s3e != nil {
		return s3e
	}
	in.Body = body
	in.ContentLength = aws.Int64(length)

	// write preconditions: dropping them would make the write unconditional
	// while the client believes it is protected
	if v := c.attr("If-Match"); v != "" {
		in.IfMatch = aws.String(v)
	}
	if v := c.attr("If-None-Match"); v != "" {
		in.IfNoneMatch = aws.String(v)
	}
	if v := c.attr("Content-Type"); v != "" {
		in.ContentType = aws.String(v)
	}
	if md5v, s3e := contentMD5Header(c.hdr); s3e != nil {
		return s3e
	} else if md5v != nil {
		in.ContentMD5 = md5v
	}
	if v := c.attr("Cache-Control"); v != "" {
		in.CacheControl = aws.String(v)
	}
	if v := c.attr("Content-Disposition"); v != "" {
		in.ContentDisposition = aws.String(v)
	}
	if v := contentEncodingWithoutAWSChunked(c.attr("Content-Encoding")); v != "" {
		in.ContentEncoding = aws.String(v)
	}
	if v := c.attr("Content-Language"); v != "" {
		in.ContentLanguage = aws.String(v)
	}
	if v := c.attr("Expires"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			in.Expires = aws.Time(t)
		}
	}
	if v := c.signed(hdrStorageClass); v != "" {
		in.StorageClass = types.StorageClass(v)
	}
	if v := c.signed("x-amz-tagging"); v != "" {
		in.Tagging = aws.String(v)
	}
	if s3e := applySSE(c.hdr, &in.ServerSideEncryption, &in.SSEKMSKeyId); s3e != nil {
		return s3e
	}
	applyObjectLockHeaders(c.hdr, &in.ObjectLockMode, &in.ObjectLockRetainUntilDate, &in.ObjectLockLegalHoldStatus)
	if md := c.hdr.AmzMeta(); len(md) > 0 {
		in.Metadata = md
	}
	cs := checksum.FromHeaders(r.Header)
	in.ChecksumCRC32 = cs.CRC32
	in.ChecksumCRC32C = cs.CRC32C
	in.ChecksumCRC64NVME = cs.CRC64NVME
	in.ChecksumSHA1 = cs.SHA1
	in.ChecksumSHA256 = cs.SHA256
	if alg := cs.Algorithm(); alg != "" {
		// name the algorithm alongside a precomputed checksum: the SDK then
		// sends x-amz-sdk-checksum-algorithm, without which Ceph RGW does
		// not store the checksum it was given
		in.ChecksumAlgorithm = types.ChecksumAlgorithm(alg)
	}
	if alg := trailerChecksumAlgorithm(rt, r.Header); alg != "" {
		in.ChecksumAlgorithm = alg
	}

	out, err := rt.client.PutObject(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	c.setResponse(OpResponse{
		SSE:         string(out.ServerSideEncryption),
		SSEKMSKeyID: aws.ToString(out.SSEKMSKeyId),
		ETag:        aws.ToString(out.ETag),
		VersionID:   aws.ToString(out.VersionId),
	})
	if out.ETag != nil {
		w.Header().Set("ETag", *out.ETag)
	}
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	setSSEHeaders(w.Header(), out.ServerSideEncryption, out.SSEKMSKeyId)
	checksum.SetHeaders(w.Header(), checksum.Values{
		CRC32:     out.ChecksumCRC32,
		CRC32C:    out.ChecksumCRC32C,
		CRC64NVME: out.ChecksumCRC64NVME,
		SHA1:      out.ChecksumSHA1,
		SHA256:    out.ChecksumSHA256,
	}, string(out.ChecksumType))
	w.WriteHeader(http.StatusOK)
	return nil
}

func (g *Gateway) deleteObject(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	in := &s3.DeleteObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	if v := r.URL.Query().Get(qpVersionID); v != "" {
		in.VersionId = aws.String(v)
	}
	if bypassGovernanceRetention(c.hdr) {
		in.BypassGovernanceRetention = aws.Bool(true)
	}
	// delete preconditions: dropped, they would make the delete
	// unconditional while the client believes it is protected
	if v := c.attr("If-Match"); v != "" {
		in.IfMatch = aws.String(v)
	}
	if v := c.signed("x-amz-if-match-last-modified-time"); v != "" {
		t, err := http.ParseTime(v)
		if err != nil {
			return s3err.New(http.StatusBadRequest, "InvalidArgument",
				"x-amz-if-match-last-modified-time must be a valid HTTP date").WithCause(err)
		}
		in.IfMatchLastModifiedTime = aws.Time(t)
	}
	if v := c.signed("x-amz-if-match-size"); v != "" {
		size, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return s3err.New(http.StatusBadRequest, "InvalidArgument",
				"x-amz-if-match-size must be an integer").WithCause(err)
		}
		in.IfMatchSize = aws.Int64(size)
	}
	out, err := rt.client.DeleteObject(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	if out.DeleteMarker != nil && *out.DeleteMarker {
		w.Header().Set("x-amz-delete-marker", "true")
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (g *Gateway) listObjectsV2(c *opCtx) error {
	w, r, rt := c.w, c.r, c.rt
	query := r.URL.Query()
	in := &s3.ListObjectsV2Input{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
	}
	if v := query.Get("prefix"); v != "" {
		in.Prefix = aws.String(v)
	}
	if v := query.Get("delimiter"); v != "" {
		in.Delimiter = aws.String(v)
	}
	if v := query.Get("max-keys"); v != "" {
		maxKeys, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return s3err.New(http.StatusBadRequest, "InvalidArgument",
				"Argument max-keys must be an integer.")
		}
		in.MaxKeys = aws.Int32(int32(maxKeys))
	}
	if v := query.Get("continuation-token"); v != "" {
		in.ContinuationToken = aws.String(v)
	}
	if v := query.Get("start-after"); v != "" {
		in.StartAfter = aws.String(v)
	}
	if v := query.Get("fetch-owner"); v == "true" {
		in.FetchOwner = aws.Bool(true)
	}
	if v := query.Get("encoding-type"); v != "" {
		in.EncodingType = types.EncodingType(v)
	}
	out, err := rt.client.ListObjectsV2(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	result := &s3xml.ListBucketResult{
		XMLNS: s3xml.Namespace,
		Name:  rt.cfg.Name, // the front bucket name, not the backend one
	}
	if out.Prefix != nil {
		result.Prefix = *out.Prefix
	}
	if out.Delimiter != nil {
		result.Delimiter = *out.Delimiter
	}
	if out.StartAfter != nil {
		result.StartAfter = *out.StartAfter
	}
	// echo the request's token, not the backend's: an empty token is not
	// forwarded, but S3 still echoes it (as an empty element)
	if query.Has("continuation-token") {
		result.ContinuationToken = aws.String(query.Get("continuation-token"))
	}
	if out.NextContinuationToken != nil {
		result.NextContinuationToken = *out.NextContinuationToken
	}
	if out.KeyCount != nil {
		result.KeyCount = *out.KeyCount
	}
	if out.MaxKeys != nil {
		result.MaxKeys = *out.MaxKeys
	}
	if out.EncodingType != "" {
		result.EncodingType = string(out.EncodingType)
	}
	if out.IsTruncated != nil {
		result.IsTruncated = *out.IsTruncated
	}
	// owner presence follows the request (fetch-owner), like AWS
	var owner *s3xml.Owner
	if in.FetchOwner != nil && *in.FetchOwner {
		owner = tenantOwner(c.rt.cfg.Tenant)
	}
	result.Contents = objectsFromSDK(out.Contents, owner)
	for _, cp := range out.CommonPrefixes {
		if cp.Prefix != nil {
			result.CommonPrefixes = append(result.CommonPrefixes, s3xml.CommonPrefix{Prefix: *cp.Prefix})
		}
	}
	return s3xml.Write(w, result)
}

// tenantOwner is the Owner (and Initiator) the gateway reports wherever the
// S3 API exposes one, matching the ACL stub and ListBuckets. The owner the
// backend reports is the operator's backend account and never appears in a
// response.
func tenantOwner(tenant string) *s3xml.Owner {
	return &s3xml.Owner{ID: tenant, DisplayName: tenant}
}

// objectsFromSDK converts listed objects for the response; owner (nil =
// omitted) replaces whatever owner the backend reported.
func objectsFromSDK(objects []types.Object, owner *s3xml.Owner) []s3xml.Object {
	result := make([]s3xml.Object, 0, len(objects))
	for _, obj := range objects {
		o := s3xml.Object{
			StorageClass: string(obj.StorageClass),
			Owner:        owner,
		}
		if obj.Key != nil {
			o.Key = *obj.Key
		}
		if obj.LastModified != nil {
			o.LastModified = s3xml.FormatTime(*obj.LastModified)
		}
		if obj.ETag != nil {
			o.ETag = *obj.ETag
		}
		if obj.Size != nil {
			o.Size = *obj.Size
		}
		result = append(result, o)
	}
	return result
}

func (g *Gateway) listObjectsV1(c *opCtx) error {
	w, r, rt := c.w, c.r, c.rt
	query := r.URL.Query()
	in := &s3.ListObjectsInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
	}
	if v := query.Get("prefix"); v != "" {
		in.Prefix = aws.String(v)
	}
	if v := query.Get("delimiter"); v != "" {
		in.Delimiter = aws.String(v)
	}
	if v := query.Get("marker"); v != "" {
		in.Marker = aws.String(v)
	}
	if v := query.Get("max-keys"); v != "" {
		maxKeys, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return s3err.New(http.StatusBadRequest, "InvalidArgument",
				"Argument max-keys must be an integer.")
		}
		in.MaxKeys = aws.Int32(int32(maxKeys))
	}
	if v := query.Get("encoding-type"); v != "" {
		in.EncodingType = types.EncodingType(v)
	}
	out, err := rt.client.ListObjects(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	result := &s3xml.ListBucketResultV1{
		XMLNS: s3xml.Namespace,
		Name:  rt.cfg.Name, // the front bucket name, not the backend one
	}
	if out.Prefix != nil {
		result.Prefix = *out.Prefix
	}
	if out.Delimiter != nil {
		result.Delimiter = *out.Delimiter
	}
	if out.Marker != nil {
		result.Marker = *out.Marker
	}
	if out.NextMarker != nil {
		result.NextMarker = *out.NextMarker
	}
	if out.MaxKeys != nil {
		result.MaxKeys = *out.MaxKeys
	}
	if out.EncodingType != "" {
		result.EncodingType = string(out.EncodingType)
	}
	if out.IsTruncated != nil {
		result.IsTruncated = *out.IsTruncated
	}
	// ListObjects (V1) always carries an Owner on AWS
	result.Contents = objectsFromSDK(out.Contents, tenantOwner(c.rt.cfg.Tenant))
	for _, cp := range out.CommonPrefixes {
		if cp.Prefix != nil {
			result.CommonPrefixes = append(result.CommonPrefixes, s3xml.CommonPrefix{Prefix: *cp.Prefix})
		}
	}
	return s3xml.Write(w, result)
}

// frontRegion is the region this endpoint reports to clients: the SetRegion
// value, us-east-1 when unset. Never the backend's region — that is backend
// identity, and a client honoring the reported region would re-sign with it
// and be refused by a pinned verifier.
func (g *Gateway) frontRegion() string {
	if g.region != "" {
		return g.region
	}
	return "us-east-1"
}

// getBucketLocation answers with the gateway's own region, without calling
// the backend.
func (g *Gateway) getBucketLocation(c *opCtx) error {
	region := g.frontRegion()
	if region == "us-east-1" {
		// S3 convention: us-east-1 is represented as an empty value
		region = ""
	}
	return s3xml.Write(c.w, &s3xml.LocationConstraint{XMLNS: s3xml.Namespace, Value: region})
}

func (g *Gateway) deleteObjects(c *opCtx) error {
	w, r, rt, vr := c.w, c.r, c.rt, c.vr
	var req s3xml.DeleteRequest
	if s3e := readXMLBody(c, &req); s3e != nil {
		return s3e
	}
	if len(req.Objects) == 0 || len(req.Objects) > 1000 {
		return s3err.New(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema.")
	}
	// the policy is evaluated per object, like AWS: denied keys are reported
	// as errors and only the permitted ones reach the backend. The
	// resource-independent parts of the check (user policy, and the bucket
	// policy's matching Deny statements) are resolved once here so that only
	// the resource is tested per key rather than the whole policy per object.
	bypass := bypassGovernanceRetention(c.hdr)
	delAuth := g.perObjectAuthorizer(vr, rt.cfg, "s3:DeleteObject")
	var bypassAuth perObjectAuthorizer
	if bypass {
		bypassAuth = g.perObjectAuthorizer(vr, rt.cfg, "s3:BypassGovernanceRetention")
	}
	// when nothing can deny any key, the per-object check (and building its
	// resource string) is skipped entirely
	checkPerObject := !delAuth.allowsEverything() || (bypass && !bypassAuth.allowsEverything())
	result := &s3xml.DeleteResult{XMLNS: s3xml.Namespace}
	objects := make([]types.ObjectIdentifier, 0, len(req.Objects))
	for _, o := range req.Objects {
		if checkPerObject {
			resource := rt.cfg.Name + "/" + o.Key
			if delAuth.denies(resource) || (bypass && bypassAuth.denies(resource)) {
				s3e := s3err.AccessDenied()
				result.Errors = append(result.Errors, s3xml.DeleteError{
					Key: o.Key, VersionID: o.VersionID, Code: s3e.Code, Message: s3e.Message,
				})
				continue
			}
		}
		oi := types.ObjectIdentifier{Key: aws.String(o.Key)}
		if o.VersionID != "" {
			oi.VersionId = aws.String(o.VersionID)
		}
		// per-object delete preconditions; see deleteObject
		if o.ETag != "" {
			oi.ETag = aws.String(o.ETag)
		}
		if o.LastModifiedTime != "" {
			// the SDKs serialize this member as an HTTP date
			t, err := http.ParseTime(o.LastModifiedTime)
			if err != nil {
				return s3err.New(http.StatusBadRequest, "MalformedXML",
					"The XML you provided was not well-formed or did not validate against our published schema.").WithCause(err)
			}
			oi.LastModifiedTime = aws.Time(t)
		}
		oi.Size = o.Size
		objects = append(objects, oi)
	}
	if len(objects) == 0 {
		return s3xml.Write(w, result)
	}
	in := &s3.DeleteObjectsInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Delete: &types.Delete{
			Objects: objects,
			Quiet:   aws.Bool(req.Quiet),
		},
	}
	if bypassGovernanceRetention(c.hdr) {
		in.BypassGovernanceRetention = aws.Bool(true)
	}
	out, err := rt.client.DeleteObjects(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	for _, d := range out.Deleted {
		deleted := s3xml.DeletedObject{}
		if d.Key != nil {
			deleted.Key = *d.Key
		}
		if d.VersionId != nil {
			deleted.VersionID = *d.VersionId
		}
		if d.DeleteMarker != nil {
			deleted.DeleteMarker = *d.DeleteMarker
		}
		if d.DeleteMarkerVersionId != nil {
			deleted.DeleteMarkerVersionID = *d.DeleteMarkerVersionId
		}
		result.Deleted = append(result.Deleted, deleted)
	}
	for _, e := range out.Errors {
		derr := s3xml.DeleteError{}
		if e.Key != nil {
			derr.Key = *e.Key
		}
		if e.VersionId != nil {
			derr.VersionID = *e.VersionId
		}
		if e.Code != nil {
			derr.Code = *e.Code
		}
		if e.Message != nil {
			derr.Message = *e.Message
		}
		result.Errors = append(result.Errors, derr)
	}
	return s3xml.Write(w, result)
}

func (g *Gateway) headBucket(c *opCtx) error {
	w, r, rt := c.w, c.r, c.rt
	in := &s3.HeadBucketInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
	}
	if _, err := rt.client.HeadBucket(r.Context(), in); err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	// the region discovery header AWS recommends over GetBucketLocation;
	// always a concrete name, us-east-1 included
	w.Header().Set("x-amz-bucket-region", g.frontRegion())
	w.WriteHeader(http.StatusOK)
	return nil
}

// maxListBuckets is the ListBuckets page-size limit and default, as on S3.
const maxListBuckets = 10000

func (g *Gateway) listBuckets(w http.ResponseWriter, r *http.Request, vr *verifiedRequest) error {
	query := r.URL.Query()
	maxBuckets := maxListBuckets
	if v := query.Get("max-buckets"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxListBuckets {
			return s3err.New(http.StatusBadRequest, "InvalidArgument",
				"max-buckets must be an integer between 1 and 10000")
		}
		maxBuckets = n
	}
	token := query.Get("continuation-token")

	entries, err := g.store.ListBuckets(r.Context(), vr.Tenant)
	if err != nil {
		return s3err.Internal(err, "bucket lookup failed")
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	// the continuation token is the last bucket name of the previous page
	if token != "" {
		i := sort.Search(len(entries), func(i int) bool { return entries[i].Name > token })
		entries = entries[i:]
	}
	result := &s3xml.ListAllMyBucketsResult{
		XMLNS: s3xml.Namespace,
		Owner: s3xml.Owner{ID: vr.Tenant, DisplayName: vr.Tenant},
	}
	if len(entries) > maxBuckets {
		entries = entries[:maxBuckets]
		result.ContinuationToken = entries[len(entries)-1].Name
	}
	for _, e := range entries {
		created := e.CreatedAt
		if created.IsZero() {
			// a definition may not track its creation; a fixed date beats
			// inventing one
			created = time.Unix(0, 0)
		}
		result.Buckets.Bucket = append(result.Buckets.Bucket, s3xml.BucketEntry{
			Name:         e.Name,
			CreationDate: s3xml.FormatTime(created),
		})
	}
	return s3xml.Write(w, result)
}

// requestBody returns the payload reader and its decoded length,
// decoding aws-chunked framing when the request declares it.
func requestBody(c *opCtx) (io.Reader, int64, *s3err.Error) {
	r, vr := c.r, c.vr
	switch {
	case sigv4.IsStreaming(vr.PayloadHash):
		decodedLength := c.signed("x-amz-decoded-content-length")
		if decodedLength == "" {
			return nil, 0, s3err.New(http.StatusLengthRequired, "MissingContentLength",
				"You must provide the x-amz-decoded-content-length HTTP header.")
		}
		length, err := strconv.ParseInt(decodedLength, 10, 64)
		if err != nil || length < 0 {
			return nil, 0, s3err.New(http.StatusBadRequest, "InvalidRequest",
				"Invalid x-amz-decoded-content-length header")
		}
		return sigv4.NewChunkedReader(r.Body, vr.Verified, checksum.TrailerAlgorithm(r.Header), length), length, nil
	default:
		if r.ContentLength < 0 {
			return nil, 0, s3err.New(http.StatusLengthRequired, "MissingContentLength",
				"You must provide the Content-Length HTTP header.")
		}
		// When the client signed a concrete payload hash (not UNSIGNED-PAYLOAD),
		// the signature only commits to the header value, not the bytes; the
		// backend is sent the body as UNSIGNED-PAYLOAD, so verify the body
		// against the signed hash here (as S3 does) or a tampered payload would
		// be committed unverified.
		if isHexSHA256(vr.PayloadHash) {
			return newPayloadVerifier(r.Body, vr.PayloadHash, r.ContentLength), r.ContentLength, nil
		}
		return r.Body, r.ContentLength, nil
	}
}

// trailerChecksumAlgorithm returns the ChecksumAlgorithm to hand the backend
// SDK when the client sent its checksum as an aws-chunked trailer. The
// trailer itself is verified by the chunked reader; naming the algorithm
// makes the SDK recompute the checksum toward the backend so it is stored
// (an explicit ChecksumAlgorithm forces calculation even with
// RequestChecksumCalculationWhenRequired).
//
// The SDK can only do that as a trailer of its own, which it sends over
// https exclusively: over plain http it would have to buffer the whole
// (unseekable) body to put the checksum in a header and fails the request
// instead. So over an http backend the algorithm is not forwarded — the
// upload is still integrity-checked by the proxy, the backend just does not
// store a checksum.
func trailerChecksumAlgorithm(rt *bucketRT, h http.Header) types.ChecksumAlgorithm {
	alg := checksum.TrailerAlgorithm(h)
	if alg == "" || !rt.cfg.Backend.IsHTTPS() {
		return ""
	}
	return types.ChecksumAlgorithm(strings.ToUpper(alg))
}

// readXMLBody decodes an XML request body (aws-chunked aware) into v.
func readXMLBody(c *opCtx, v any) *s3err.Error {
	body, _, s3e := requestBody(c)
	if s3e != nil {
		return s3e
	}
	data, err := io.ReadAll(io.LimitReader(body, maxXMLBodySize))
	if err != nil {
		// the body reader verifies payload integrity (the signed hash, chunk
		// signatures, trailer checksums), so a read error may be the S3 error
		// the client must see — same unwrapping as fromSDKError
		if s3e, ok := errors.AsType[*s3err.Error](err); ok {
			return s3e
		}
		return s3err.New(http.StatusBadRequest, "InvalidRequest",
			"failed to read request body").WithCause(err)
	}
	if err := xml.Unmarshal(data, v); err != nil {
		return s3err.New(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema.").WithCause(err)
	}
	return nil
}

// isHexSHA256 reports whether s is a 64-character hex string, i.e. a concrete
// SHA-256 payload hash rather than a sentinel like UNSIGNED-PAYLOAD. Upper
// case is accepted too: skipping verification for a mis-cased but otherwise
// valid hash would fail open.
func isHexSHA256(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// payloadVerifier checks a request body's SHA-256 against the value the client
// signed in x-amz-content-sha256. It verifies as soon as the declared length
// has been read (and on EOF), aborting the stream on mismatch so an altered
// payload never reaches the backend.
type payloadVerifier struct {
	r         io.Reader
	h         hash.Hash
	want      string
	remaining int64
	done      bool
}

func newPayloadVerifier(r io.Reader, want string, length int64) *payloadVerifier {
	// hex.EncodeToString produces lower case, so normalize the expected value
	return &payloadVerifier{r: r, h: sha256.New(), want: strings.ToLower(want), remaining: length}
}

func (v *payloadVerifier) Read(p []byte) (int, error) {
	n, err := v.r.Read(p)
	if n > 0 {
		v.h.Write(p[:n])
		v.remaining -= int64(n)
	}
	if !v.done && (v.remaining <= 0 || err == io.EOF) {
		v.done = true
		got := hex.EncodeToString(v.h.Sum(nil))
		if subtle.ConstantTimeCompare([]byte(got), []byte(v.want)) != 1 {
			return 0, s3err.ContentSHA256Mismatch()
		}
	}
	return n, err
}

// contentMD5Header returns the request's Content-MD5 value after checking
// that a present header — even an empty one, which Header.Get cannot tell
// from an absent one — is the base64 of an MD5 digest. S3 refuses an
// invalid value with InvalidDigest; forwarding it blind would let an empty
// header vanish while the client believes the integrity check applied.
func contentMD5Header(hdr signedHeader) (*string, *s3err.Error) {
	vs := hdr.AttributeValues("Content-MD5")
	if len(vs) == 0 {
		return nil, nil
	}
	b, err := base64.StdEncoding.DecodeString(vs[0])
	if err != nil || len(b) != md5.Size {
		return nil, s3err.New(http.StatusBadRequest, "InvalidDigest",
			"The Content-MD5 you specified was invalid.")
	}
	return aws.String(vs[0]), nil
}

// relayedErrorHeaders are entity and informational headers a backend sets
// on object error responses — the ETag/Last-Modified of a 304, the
// x-amz-delete-marker of a 404 — that describe the tenant's own object and
// must reach the client.
var relayedErrorHeaders = []string{
	"ETag", "Last-Modified", "Cache-Control", "Expires",
	"x-amz-delete-marker", "x-amz-version-id",
}

// relayErrorHeaders copies relayedErrorHeaders out of the backend response
// carried by an SDK error, if it carries one.
func relayErrorHeaders(h http.Header, err error) {
	var respErr *awshttp.ResponseError
	if !errors.As(err, &respErr) || respErr.Response == nil || respErr.Response.Response == nil {
		return
	}
	for _, k := range relayedErrorHeaders {
		if v := respErr.Response.Header.Get(k); v != "" {
			h.Set(k, v)
		}
	}
}

// applyPartNumber forwards the partNumber query parameter (a ranged read
// of one part of a multipart object) on GetObject/HeadObject.
func applyPartNumber(query url.Values, partNumber **int32) *s3err.Error {
	v := query.Get(qpPartNumber)
	if v == "" {
		return nil
	}
	n, err := strconv.ParseInt(v, 10, 32)
	if err != nil || n < 1 || n > 10000 {
		return s3err.New(http.StatusBadRequest, "InvalidArgument",
			"Part number must be an integer between 1 and 10000, inclusive").WithCause(err)
	}
	*partNumber = aws.Int32(int32(n))
	return nil
}

func applyConditionalHeaders(hdr signedHeader, ifMatch, ifNoneMatch **string, ifModifiedSince, ifUnmodifiedSince **time.Time) {
	if v := hdr.Attribute("If-Match"); v != "" {
		*ifMatch = aws.String(v)
	}
	if v := hdr.Attribute("If-None-Match"); v != "" {
		*ifNoneMatch = aws.String(v)
	}
	if v := hdr.Attribute("If-Modified-Since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			*ifModifiedSince = aws.Time(t)
		}
	}
	if v := hdr.Attribute("If-Unmodified-Since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			*ifUnmodifiedSince = aws.Time(t)
		}
	}
}

type objectHeaderValues struct {
	ContentType        *string
	ContentLength      *int64
	ETag               *string
	LastModified       *time.Time
	CacheControl       *string
	ContentDisposition *string
	ContentEncoding    *string
	ContentLanguage    *string
	Expires            *string
	StorageClass       string
	VersionID          *string
	SSE                types.ServerSideEncryption
	SSEKMSKeyID        *string
	Metadata           map[string]string
}

func setObjectHeaders(h http.Header, v objectHeaderValues) {
	if v.ContentType != nil {
		h.Set("Content-Type", *v.ContentType)
	}
	if v.ContentLength != nil {
		h.Set("Content-Length", strconv.FormatInt(*v.ContentLength, 10))
	}
	if v.ETag != nil {
		h.Set("ETag", *v.ETag)
	}
	if v.LastModified != nil {
		h.Set("Last-Modified", v.LastModified.UTC().Format(http.TimeFormat))
	}
	if v.CacheControl != nil {
		h.Set("Cache-Control", *v.CacheControl)
	}
	if v.ContentDisposition != nil {
		h.Set("Content-Disposition", *v.ContentDisposition)
	}
	if v.ContentEncoding != nil {
		h.Set("Content-Encoding", *v.ContentEncoding)
	}
	if v.ContentLanguage != nil {
		h.Set("Content-Language", *v.ContentLanguage)
	}
	if v.Expires != nil {
		h.Set("Expires", *v.Expires)
	}
	if v.StorageClass != "" {
		h.Set("x-amz-storage-class", v.StorageClass)
	}
	if v.VersionID != nil {
		h.Set("x-amz-version-id", *v.VersionID)
	}
	setSSEHeaders(h, v.SSE, v.SSEKMSKeyID)
	for k, val := range v.Metadata {
		// Header.Set would canonicalize the name (turning meta1 into
		// X-Amz-Meta-Meta1 on the wire); S3 metadata keys reach the client
		// lowercase, and clients index the parsed metadata by that suffix
		h[amzMetaPrefix+strings.ToLower(k)] = []string{val}
	}
}

func contentEncodingWithoutAWSChunked(v string) string {
	if v == "" {
		return ""
	}
	var encodings []string
	for e := range strings.SplitSeq(v, ",") {
		if e = strings.TrimSpace(e); e != "" && e != "aws-chunked" {
			encodings = append(encodings, e)
		}
	}
	return strings.Join(encodings, ", ")
}
