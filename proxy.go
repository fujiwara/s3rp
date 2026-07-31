package s3rp

import (
	"encoding/xml"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func (app *S3RP) getObject(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string) error {
	in := &s3.GetObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	applyConditionalHeaders(r, &in.IfMatch, &in.IfNoneMatch, &in.IfModifiedSince, &in.IfUnmodifiedSince)
	if v := r.Header.Get("Range"); v != "" {
		in.Range = aws.String(v)
	}
	query := r.URL.Query()
	if v := query.Get("versionId"); v != "" {
		in.VersionId = aws.String(v)
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
		return fromSDKError(err, r.URL.Path)
	}
	defer out.Body.Close()

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
		Metadata:           out.Metadata,
	})
	if out.AcceptRanges != nil {
		h.Set("Accept-Ranges", *out.AcceptRanges)
	}
	if out.TagCount != nil {
		h.Set("x-amz-tagging-count", strconv.FormatInt(int64(*out.TagCount), 10))
	}
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

func (app *S3RP) headObject(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string) error {
	in := &s3.HeadObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	applyConditionalHeaders(r, &in.IfMatch, &in.IfNoneMatch, &in.IfModifiedSince, &in.IfUnmodifiedSince)
	if v := r.Header.Get("Range"); v != "" {
		in.Range = aws.String(v)
	}
	if v := r.URL.Query().Get("versionId"); v != "" {
		in.VersionId = aws.String(v)
	}
	out, err := rt.client.HeadObject(r.Context(), in)
	if err != nil {
		return fromSDKError(err, r.URL.Path)
	}
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
		Metadata:           out.Metadata,
	})
	if out.AcceptRanges != nil {
		w.Header().Set("Accept-Ranges", *out.AcceptRanges)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (app *S3RP) putObject(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string, vr *verifiedRequest) error {
	in := &s3.PutObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	body, length, s3err := requestBody(r, vr)
	if s3err != nil {
		return s3err
	}
	in.Body = body
	in.ContentLength = aws.Int64(length)

	if v := r.Header.Get("Content-Type"); v != "" {
		in.ContentType = aws.String(v)
	}
	if v := r.Header.Get("Content-MD5"); v != "" {
		in.ContentMD5 = aws.String(v)
	}
	if v := r.Header.Get("Cache-Control"); v != "" {
		in.CacheControl = aws.String(v)
	}
	if v := r.Header.Get("Content-Disposition"); v != "" {
		in.ContentDisposition = aws.String(v)
	}
	if v := contentEncodingWithoutAWSChunked(r.Header.Get("Content-Encoding")); v != "" {
		in.ContentEncoding = aws.String(v)
	}
	if v := r.Header.Get("Content-Language"); v != "" {
		in.ContentLanguage = aws.String(v)
	}
	if v := r.Header.Get("Expires"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			in.Expires = aws.Time(t)
		}
	}
	if v := r.Header.Get("x-amz-storage-class"); v != "" {
		in.StorageClass = types.StorageClass(v)
	}
	if v := r.Header.Get("x-amz-tagging"); v != "" {
		in.Tagging = aws.String(v)
	}
	if md := metadataFromHeaders(r.Header); len(md) > 0 {
		in.Metadata = md
	}

	out, err := rt.client.PutObject(r.Context(), in)
	if err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	if out.ETag != nil {
		w.Header().Set("ETag", *out.ETag)
	}
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (app *S3RP) deleteObject(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string) error {
	in := &s3.DeleteObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	if v := r.URL.Query().Get("versionId"); v != "" {
		in.VersionId = aws.String(v)
	}
	out, err := rt.client.DeleteObject(r.Context(), in)
	if err != nil {
		return fromSDKError(err, r.URL.Path)
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

func (app *S3RP) listObjectsV2(w http.ResponseWriter, r *http.Request, rt *bucketRT) error {
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
			return newS3Error(http.StatusBadRequest, "InvalidArgument",
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
		return fromSDKError(err, r.URL.Path)
	}
	result := &ListBucketResult{
		XMLNS: s3XMLNS,
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
	if out.ContinuationToken != nil {
		result.ContinuationToken = *out.ContinuationToken
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
	result.Contents = objectsFromSDK(out.Contents)
	for _, cp := range out.CommonPrefixes {
		if cp.Prefix != nil {
			result.CommonPrefixes = append(result.CommonPrefixes, CommonPrefix{Prefix: *cp.Prefix})
		}
	}
	return writeXML(w, result)
}

func objectsFromSDK(objects []types.Object) []Object {
	result := make([]Object, 0, len(objects))
	for _, obj := range objects {
		o := Object{
			StorageClass: string(obj.StorageClass),
		}
		if obj.Key != nil {
			o.Key = *obj.Key
		}
		if obj.LastModified != nil {
			o.LastModified = s3Time(*obj.LastModified)
		}
		if obj.ETag != nil {
			o.ETag = *obj.ETag
		}
		if obj.Size != nil {
			o.Size = *obj.Size
		}
		if obj.Owner != nil {
			o.Owner = &Owner{}
			if obj.Owner.ID != nil {
				o.Owner.ID = *obj.Owner.ID
			}
			if obj.Owner.DisplayName != nil {
				o.Owner.DisplayName = *obj.Owner.DisplayName
			}
		}
		result = append(result, o)
	}
	return result
}

func (app *S3RP) listObjectsV1(w http.ResponseWriter, r *http.Request, rt *bucketRT) error {
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
			return newS3Error(http.StatusBadRequest, "InvalidArgument",
				"Argument max-keys must be an integer.")
		}
		in.MaxKeys = aws.Int32(int32(maxKeys))
	}
	if v := query.Get("encoding-type"); v != "" {
		in.EncodingType = types.EncodingType(v)
	}
	out, err := rt.client.ListObjects(r.Context(), in)
	if err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	result := &ListBucketResultV1{
		XMLNS: s3XMLNS,
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
	result.Contents = objectsFromSDK(out.Contents)
	for _, cp := range out.CommonPrefixes {
		if cp.Prefix != nil {
			result.CommonPrefixes = append(result.CommonPrefixes, CommonPrefix{Prefix: *cp.Prefix})
		}
	}
	return writeXML(w, result)
}

// getBucketLocation answers from the config without calling the backend.
func (app *S3RP) getBucketLocation(w http.ResponseWriter, rt *bucketRT) error {
	region := rt.cfg.Backend.Region
	if region == "us-east-1" {
		// S3 convention: us-east-1 is represented as an empty value
		region = ""
	}
	return writeXML(w, &LocationConstraint{XMLNS: s3XMLNS, Value: region})
}

func (app *S3RP) deleteObjects(w http.ResponseWriter, r *http.Request, rt *bucketRT, vr *verifiedRequest) error {
	body, _, s3err := requestBody(r, vr)
	if s3err != nil {
		return s3err
	}
	data, err := io.ReadAll(io.LimitReader(body, maxXMLBodySize))
	if err != nil {
		return newS3Error(http.StatusBadRequest, "InvalidRequest", "failed to read request body")
	}
	var req deleteRequest
	if err := xml.Unmarshal(data, &req); err != nil {
		return newS3Error(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema.")
	}
	if len(req.Objects) == 0 || len(req.Objects) > 1000 {
		return newS3Error(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema.")
	}
	objects := make([]types.ObjectIdentifier, 0, len(req.Objects))
	for _, o := range req.Objects {
		oi := types.ObjectIdentifier{Key: aws.String(o.Key)}
		if o.VersionID != "" {
			oi.VersionId = aws.String(o.VersionID)
		}
		objects = append(objects, oi)
	}
	in := &s3.DeleteObjectsInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Delete: &types.Delete{
			Objects: objects,
			Quiet:   aws.Bool(req.Quiet),
		},
	}
	out, err := rt.client.DeleteObjects(r.Context(), in)
	if err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	result := &DeleteResult{XMLNS: s3XMLNS}
	for _, d := range out.Deleted {
		deleted := DeletedObject{}
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
		derr := DeleteError{}
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
	return writeXML(w, result)
}

func (app *S3RP) headBucket(w http.ResponseWriter, r *http.Request, rt *bucketRT) error {
	in := &s3.HeadBucketInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
	}
	if _, err := rt.client.HeadBucket(r.Context(), in); err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (app *S3RP) listBuckets(w http.ResponseWriter, vr *verifiedRequest) error {
	buckets := app.keys[vr.AccessKeyID].buckets
	names := make([]string, 0, len(buckets))
	for name := range buckets {
		names = append(names, name)
	}
	sort.Strings(names)
	result := &ListAllMyBucketsResult{
		XMLNS: s3XMLNS,
		Owner: Owner{ID: vr.AccessKeyID, DisplayName: vr.AccessKeyID},
	}
	for _, name := range names {
		result.Buckets.Bucket = append(result.Buckets.Bucket, Bucket{
			Name: name,
			// buckets are defined in the config; expose a fixed date
			CreationDate: s3Time(time.Unix(0, 0)),
		})
	}
	return writeXML(w, result)
}

// requestBody returns the payload reader and its decoded length,
// decoding aws-chunked framing when the request declares it.
func requestBody(r *http.Request, vr *verifiedRequest) (io.Reader, int64, *S3Error) {
	switch vr.PayloadHash {
	case streamingSHA256, streamingSHA256T, streamingUnsignedT:
		decodedLength := r.Header.Get("x-amz-decoded-content-length")
		if decodedLength == "" {
			return nil, 0, newS3Error(http.StatusLengthRequired, "MissingContentLength",
				"You must provide the x-amz-decoded-content-length HTTP header.")
		}
		length, err := strconv.ParseInt(decodedLength, 10, 64)
		if err != nil || length < 0 {
			return nil, 0, newS3Error(http.StatusBadRequest, "InvalidRequest",
				"Invalid x-amz-decoded-content-length header")
		}
		return newChunkedReader(r.Body, vr), length, nil
	default:
		if r.ContentLength < 0 {
			return nil, 0, newS3Error(http.StatusLengthRequired, "MissingContentLength",
				"You must provide the Content-Length HTTP header.")
		}
		return r.Body, r.ContentLength, nil
	}
}

func writeXML(w http.ResponseWriter, v any) error {
	b, err := xml.Marshal(v)
	if err != nil {
		return fmt.Errorf("failed to marshal XML response: %w", err)
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(xml.Header))
	w.Write(b)
	return nil
}

func applyConditionalHeaders(r *http.Request, ifMatch, ifNoneMatch **string, ifModifiedSince, ifUnmodifiedSince **time.Time) {
	if v := r.Header.Get("If-Match"); v != "" {
		*ifMatch = aws.String(v)
	}
	if v := r.Header.Get("If-None-Match"); v != "" {
		*ifNoneMatch = aws.String(v)
	}
	if v := r.Header.Get("If-Modified-Since"); v != "" {
		if t, err := http.ParseTime(v); err == nil {
			*ifModifiedSince = aws.Time(t)
		}
	}
	if v := r.Header.Get("If-Unmodified-Since"); v != "" {
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
	for k, val := range v.Metadata {
		h.Set("x-amz-meta-"+k, val)
	}
}

// contentEncodingWithoutAWSChunked strips the aws-chunked token from a
// Content-Encoding header value.
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

// metadataFromHeaders extracts x-amz-meta-* headers.
func metadataFromHeaders(h http.Header) map[string]string {
	md := make(map[string]string)
	for k, vs := range h {
		lk := strings.ToLower(k)
		if name, ok := strings.CutPrefix(lk, "x-amz-meta-"); ok && len(vs) > 0 {
			md[name] = vs[0]
		}
	}
	return md
}
