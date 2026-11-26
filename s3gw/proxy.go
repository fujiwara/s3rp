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
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/fujiwara/s3rp/checksum"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3op"
	"github.com/fujiwara/s3rp/s3xml"
	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"

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
		StorageClass:       c.clientStorageClass(string(out.StorageClass)),
		VersionID:          out.VersionId,
		SSE:                out.ServerSideEncryption,
		SSEKMSKeyID:        c.clientKMSKeyID(out.SSEKMSKeyId),
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
	c.setChecksumHeaders(h, checksumsFromGetObjectOutput(out), string(out.ChecksumType))
	setObjectLockResponseHeaders(h, out.ObjectLockMode, out.ObjectLockRetainUntilDate, out.ObjectLockLegalHoldStatus)
	status := http.StatusOK
	if out.ContentRange != nil {
		h.Set("Content-Range", *out.ContentRange)
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
	src := &sourceReader{Reader: out.Body}
	if _, err := io.Copy(w, src); err != nil {
		// the status is sent and the client sees a broken body; returned
		// so that the observer at least is told
		// a read can hand over bytes together with its error, and the
		// write of those bytes can fail too: io.Copy then returns the
		// write's error, so the read is to blame only when it is the one
		// returned
		return bodyCopyError(r, err, err == src.err)
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
		StorageClass:       c.clientStorageClass(string(out.StorageClass)),
		VersionID:          out.VersionId,
		SSE:                out.ServerSideEncryption,
		SSEKMSKeyID:        c.clientKMSKeyID(out.SSEKMSKeyId),
		Metadata:           out.Metadata,
	})
	if out.AcceptRanges != nil {
		w.Header().Set("Accept-Ranges", *out.AcceptRanges)
	}
	if out.TagCount != nil {
		w.Header().Set("x-amz-tagging-count", strconv.FormatInt(int64(*out.TagCount), 10))
	}
	if out.PartsCount != nil {
		w.Header().Set("x-amz-mp-parts-count", strconv.FormatInt(int64(*out.PartsCount), 10))
	}
	c.setChecksumHeaders(w.Header(), checksumsFromHeadObjectOutput(out), string(out.ChecksumType))
	setObjectLockResponseHeaders(w.Header(), out.ObjectLockMode, out.ObjectLockRetainUntilDate, out.ObjectLockLegalHoldStatus)
	// a HEAD of one part or of a range is answered as its GET is
	status := http.StatusOK
	if out.ContentRange != nil {
		w.Header().Set("Content-Range", *out.ContentRange)
		status = http.StatusPartialContent
	}
	w.WriteHeader(status)
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
	in.StorageClass = c.backendStorageClass(c.signed(hdrStorageClass), length)
	if v := c.signed(hdrTagging); v != "" {
		in.Tagging = aws.String(v)
	}
	if s3e := c.applySSE(c.hdr, &in.ServerSideEncryption, &in.SSEKMSKeyId); s3e != nil {
		return s3e
	}
	applyObjectLockHeaders(c.hdr, &in.ObjectLockMode, &in.ObjectLockRetainUntilDate, &in.ObjectLockLegalHoldStatus)
	if md := c.hdr.AmzMeta(); len(md) > 0 {
		in.Metadata = md
	}
	cs := checksum.FromHeaders(r.Header)
	setChecksumsPutObjectInput(in, cs)
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
	setSSEHeaders(w.Header(), out.ServerSideEncryption, c.clientKMSKeyID(out.SSEKMSKeyId))
	c.setChecksumHeaders(w.Header(), checksumsFromPutObjectOutput(out), string(out.ChecksumType))
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
	attrs, err := c.optionalObjectAttributes()
	if err != nil {
		return err
	}
	in.OptionalObjectAttributes = attrs
	out, err := rt.client.ListObjectsV2(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	result := &s3xml.ListBucketResult{
		XMLNS:                 s3xml.Namespace,
		Name:                  rt.cfg.Name, // the front bucket name, not the backend one
		Prefix:                aws.ToString(out.Prefix),
		Delimiter:             aws.ToString(out.Delimiter),
		StartAfter:            aws.ToString(out.StartAfter),
		NextContinuationToken: aws.ToString(out.NextContinuationToken),
		KeyCount:              aws.ToInt32(out.KeyCount),
		MaxKeys:               aws.ToInt32(out.MaxKeys),
		EncodingType:          string(out.EncodingType),
		IsTruncated:           aws.ToBool(out.IsTruncated),
	}
	// echo the request's token, not the backend's: an empty token is not
	// forwarded, but S3 still echoes it (as an empty element)
	if query.Has("continuation-token") {
		result.ContinuationToken = aws.String(query.Get("continuation-token"))
	}
	// owner presence follows the request (fetch-owner), like AWS
	var owner *s3xml.Owner
	if in.FetchOwner != nil && *in.FetchOwner {
		owner = tenantOwner(c.rt.cfg.Tenant)
	}
	result.Contents = c.objectsFromSDK(out.Contents, owner, attrs)
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
// omitted) replaces whatever owner the backend reported, and attrs is what
// the request asked each entry to carry.
func (c *opCtx) objectsFromSDK(objects []types.Object, owner *s3xml.Owner, attrs []types.OptionalObjectAttributes) []s3xml.Object {
	result := make([]s3xml.Object, 0, len(objects))
	for _, obj := range objects {
		o := s3xml.Object{
			StorageClass:      c.clientStorageClass(string(obj.StorageClass)),
			Owner:             owner,
			Key:               aws.ToString(obj.Key),
			ETag:              aws.ToString(obj.ETag),
			Size:              aws.ToInt64(obj.Size),
			ChecksumAlgorithm: c.reportAlgorithms(obj.ChecksumAlgorithm),
			ChecksumType:      string(obj.ChecksumType),
			RestoreStatus:     restoreStatusFromSDK(obj.RestoreStatus, attrs),
		}
		if obj.LastModified != nil {
			o.LastModified = s3xml.FormatTime(*obj.LastModified)
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
	attrs, err := c.optionalObjectAttributes()
	if err != nil {
		return err
	}
	in.OptionalObjectAttributes = attrs
	out, err := rt.client.ListObjects(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	result := &s3xml.ListBucketResultV1{
		XMLNS:        s3xml.Namespace,
		Name:         rt.cfg.Name, // the front bucket name, not the backend one
		Prefix:       aws.ToString(out.Prefix),
		Delimiter:    aws.ToString(out.Delimiter),
		Marker:       aws.ToString(out.Marker),
		NextMarker:   aws.ToString(out.NextMarker),
		MaxKeys:      aws.ToInt32(out.MaxKeys),
		EncodingType: string(out.EncodingType),
		IsTruncated:  aws.ToBool(out.IsTruncated),
	}
	// ListObjects (V1) always carries an Owner on AWS
	result.Contents = c.objectsFromSDK(out.Contents, tenantOwner(c.rt.cfg.Tenant), attrs)
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
			"The XML you provided was not well-formed or did not validate against our published schema")
	}
	// the policy is evaluated per object, like AWS: denied keys are reported
	// as errors and only the permitted ones reach the backend. The
	// resource-independent parts of the check (user policy, and the bucket
	// policy's matching Deny statements) are resolved once here so that only
	// the resource is tested per key rather than the whole policy per object.
	// an entry naming a version needs s3:DeleteObjectVersion instead of
	// s3:DeleteObject, as on AWS; each authorizer is built only when some
	// entry needs it
	var hasPlain, hasVersion bool
	for _, o := range req.Objects {
		if o.VersionID != "" {
			hasVersion = true
		} else {
			hasPlain = true
		}
	}
	// when nothing can deny any key, the per-object check (and building its
	// resource string) is skipped entirely
	var checkPerObject bool
	var delAuth, verAuth, bypassAuth perObjectAuthorizer
	if hasPlain {
		delAuth = g.perObjectAuthorizer(vr, rt.cfg, s3op.ActionDeleteObject)
		checkPerObject = !delAuth.allowsEverything()
	}
	if hasVersion {
		verAuth = g.perObjectAuthorizer(vr, rt.cfg, s3op.ActionDeleteObjectVersion)
		checkPerObject = checkPerObject || !verAuth.allowsEverything()
	}
	bypass := bypassGovernanceRetention(c.hdr)
	if bypass {
		bypassAuth = g.perObjectAuthorizer(vr, rt.cfg, s3op.ActionBypassGovernanceRetention)
		checkPerObject = checkPerObject || !bypassAuth.allowsEverything()
	}
	result := &s3xml.DeleteResult{XMLNS: s3xml.Namespace}
	objects := make([]types.ObjectIdentifier, 0, len(req.Objects))
	summary := &DeleteSummary{Requested: len(req.Objects), Objects: make([]DeletedObject, len(req.Objects))}
	for i, o := range req.Objects {
		summary.Objects[i] = DeletedObject{Key: o.Key, VersionID: o.VersionID}
		// no object has such a key, but a versioned bucket would be given
		// a delete marker for it that no request could name again
		if len(o.Key) > maxKeyLen {
			s3e := keyTooLong(o.Key)
			result.Errors = append(result.Errors, s3xml.DeleteError{
				Key: o.Key, VersionID: o.VersionID, Code: s3e.Code, Message: s3e.Message,
			})
			summary.Objects[i].Code = s3e.Code
			continue
		}
		if checkPerObject {
			resource := rt.cfg.Name + "/" + o.Key
			auth := delAuth
			if o.VersionID != "" {
				auth = verAuth
			}
			denied := auth.denies(resource)
			if !denied && bypass && bypassAuth.denies(resource) {
				auth, denied = bypassAuth, true
			}
			if denied {
				s3e := s3err.AccessDenied()
				result.Errors = append(result.Errors, s3xml.DeleteError{
					Key: o.Key, VersionID: o.VersionID, Code: s3e.Code, Message: s3e.Message,
				})
				// the request itself succeeds, so the observer learns of
				// the refusals through the op, one entry per statement
				c.op.addDenial(auth.denyReason(resource))
				summary.Objects[i].Code = s3e.Code
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
					"The XML you provided was not well-formed or did not validate against our published schema").WithCause(err)
			}
			oi.LastModifiedTime = aws.Time(t)
		}
		oi.Size = o.Size
		objects = append(objects, oi)
	}
	if len(objects) == 0 {
		c.setDeleted(summary, nil)
		return s3xml.Write(w, result)
	}
	in := &s3.DeleteObjectsInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Delete: &types.Delete{
			Objects: objects,
			Quiet:   aws.Bool(req.Quiet),
		},
	}
	if bypass {
		in.BypassGovernanceRetention = aws.Bool(true)
	}
	out, err := rt.client.DeleteObjects(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	for _, d := range out.Deleted {
		deleted := s3xml.DeletedObject{
			Key:                   aws.ToString(d.Key),
			VersionID:             aws.ToString(d.VersionId),
			DeleteMarker:          aws.ToBool(d.DeleteMarker),
			DeleteMarkerVersionID: aws.ToString(d.DeleteMarkerVersionId),
		}
		result.Deleted = append(result.Deleted, deleted)
	}
	for _, e := range out.Errors {
		derr := s3xml.DeleteError{
			Key:       aws.ToString(e.Key),
			VersionID: aws.ToString(e.VersionId),
			Code:      aws.ToString(e.Code),
			Message:   aws.ToString(e.Message),
		}
		result.Errors = append(result.Errors, derr)
	}
	c.setDeleted(summary, out.Errors)
	return s3xml.Write(w, result)
}

// setDeleted completes the summary of a DeleteObjects request with the
// backend's per-key errors and puts it on the op. The outcome is derived
// from the errors rather than from the backend's Deleted list, which a
// Quiet request leaves empty.
func (c *opCtx) setDeleted(summary *DeleteSummary, errs []types.Error) {
	if c.op == nil {
		return
	}
	// an error the backend reports without a version id is taken to cover
	// every entry for that key: recording a delete that did not happen is
	// the worse mistake in an audit trail
	type entry struct{ key, versionID string }
	failed := make(map[entry]string, len(errs))
	for _, e := range errs {
		failed[entry{aws.ToString(e.Key), aws.ToString(e.VersionId)}] = aws.ToString(e.Code)
	}
	for i := range summary.Objects {
		o := &summary.Objects[i]
		if o.Code == "" {
			if code, ok := failed[entry{o.Key, o.VersionID}]; ok {
				o.Code = code
			} else if code, ok := failed[entry{o.Key, ""}]; ok {
				o.Code = code
			}
		}
		if o.Code == "" {
			summary.Deleted++
		} else {
			summary.Failed++
		}
	}
	c.op.Deleted = summary
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
	prefix := query.Get("prefix")
	if prefix != "" {
		entries = slices.DeleteFunc(entries, func(e store.BucketEntry) bool { return !strings.HasPrefix(e.Name, prefix) })
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	// the continuation token is the last bucket name of the previous page
	if token != "" {
		i := sort.Search(len(entries), func(i int) bool { return entries[i].Name > token })
		entries = entries[i:]
	}
	result := &s3xml.ListAllMyBucketsResult{
		XMLNS:  s3xml.Namespace,
		Owner:  s3xml.Owner{ID: vr.Tenant, DisplayName: vr.Tenant},
		Prefix: prefix,
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
	trailerAlg, err := checksum.TrailerAlgorithm(r.Header)
	if err != nil {
		return nil, 0, s3err.New(http.StatusNotImplemented, "NotImplemented",
			"The trailer declared by x-amz-trailer is not supported.").WithCause(err)
	}
	if s3e := c.checkTrailer(trailerAlg); s3e != nil {
		return nil, 0, s3e
	}
	if trailerAlg != "" && !strings.HasSuffix(vr.PayloadHash, "-TRAILER") {
		// the declared checksum would never be read, let alone verified
		return nil, 0, s3err.New(http.StatusBadRequest, "InvalidRequest",
			"x-amz-trailer is declared but the payload carries no trailer.")
	}
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
		return sigv4.NewChunkedReader(r.Body, vr.Verified, trailerAlg, length), length, nil
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
// store a checksum. A client that is not the SDK's answers for itself
// (StreamChecksummer).
func trailerChecksumAlgorithm(rt *bucketRT, h http.Header) types.ChecksumAlgorithm {
	// requestBody has already refused a trailer it cannot verify
	alg, err := checksum.TrailerAlgorithm(h)
	if err != nil || alg == "" {
		return ""
	}
	capable := rt.cfg.Backend.IsHTTPS()
	if sc, ok := rt.client.(StreamChecksummer); ok {
		capable = sc.ChecksumsStream()
	}
	if !capable {
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
			"The XML you provided was not well-formed or did not validate against our published schema").WithCause(err)
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
	phase     payloadPhase
}

// payloadPhase is how far the check of a body has come.
type payloadPhase int

const (
	// payloadReading: the declared length is not read yet
	payloadReading payloadPhase = iota + 1
	// payloadVerified: all of it is read and is what was signed
	payloadVerified
	// payloadRefused: it is not, and no later Read says otherwise
	payloadRefused
	numPayloadPhases
)

func (p payloadPhase) String() string {
	return [...]string{"", "reading", "verified", "refused"}[p]
}

// payloadEvent is what a Read of the body comes to.
type payloadEvent int

const (
	// payloadMore: bytes, with more of the declared length to come
	payloadMore payloadEvent = iota
	// payloadMatched, payloadMismatched: the declared length or the end of
	// the body is reached, and the hash is or is not the signed one
	payloadMatched
	payloadMismatched
	// payloadExtra: bytes beyond what was hashed
	payloadExtra
	// payloadEnd: a Read that yields no bytes
	payloadEnd
	numPayloadEvents
)

func (e payloadEvent) String() string {
	return [...]string{"more", "matched", "mismatched", "extra", "end"}[e]
}

// payloadTransitions is the verifier; a zero entry is an event that cannot
// happen in that phase. It is held to tla/PayloadVerifier.tla, which is
// checked for what the verifier promises: the bytes that complete a body
// are handed out only when the whole of it is what was signed, and a
// refusal stays one (TestPayloadTable).
var payloadTransitions = [numPayloadPhases][numPayloadEvents]payloadPhase{
	payloadReading: {
		payloadMore:       payloadReading,
		payloadEnd:        payloadReading,
		payloadMatched:    payloadVerified,
		payloadMismatched: payloadRefused,
	},
	payloadVerified: {
		payloadEnd: payloadVerified,
		// the hash that matched does not cover them
		payloadExtra: payloadRefused,
	},
	payloadRefused: {
		payloadMore:       payloadRefused,
		payloadEnd:        payloadRefused,
		payloadMatched:    payloadRefused,
		payloadMismatched: payloadRefused,
		payloadExtra:      payloadRefused,
	},
}

func newPayloadVerifier(r io.Reader, want string, length int64) *payloadVerifier {
	// hex.EncodeToString produces lower case, so normalize the expected value
	return &payloadVerifier{r: r, h: sha256.New(), want: strings.ToLower(want), remaining: length, phase: payloadReading}
}

func (v *payloadVerifier) Read(p []byte) (int, error) {
	if v.phase == payloadRefused {
		return 0, s3err.ContentSHA256Mismatch()
	}
	n, err := v.r.Read(p)
	if v.phase == payloadVerified {
		if n > 0 {
			return 0, v.advance(payloadExtra)
		}
		return 0, err
	}
	if n > 0 {
		v.h.Write(p[:n])
		v.remaining -= int64(n)
	}
	switch {
	case v.remaining <= 0 || err == io.EOF:
		event := payloadMismatched
		got := hex.EncodeToString(v.h.Sum(nil))
		if subtle.ConstantTimeCompare([]byte(got), []byte(v.want)) == 1 {
			event = payloadMatched
		}
		if verr := v.advance(event); verr != nil {
			return 0, verr
		}
	case n > 0:
		v.advance(payloadMore)
	}
	return n, err
}

// advance moves the verifier as the table says and returns the refusal when
// that is where it ends up. An event the table does not have in this phase
// is a bug in this file, and refuses the body.
func (v *payloadVerifier) advance(e payloadEvent) error {
	v.phase = payloadTransitions[v.phase][e]
	if v.phase == payloadVerified || v.phase == payloadReading {
		return nil
	}
	v.phase = payloadRefused
	return s3err.ContentSHA256Mismatch()
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
