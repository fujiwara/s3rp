package s3rp

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/xml"
	"github.com/fujiwara/s3rp/checksum"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"
	"github.com/fujiwara/s3rp/sigv4"
	"hash"
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

func (app *S3RP) getObject(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	in := &s3.GetObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	applyConditionalHeaders(r, &in.IfMatch, &in.IfNoneMatch, &in.IfModifiedSince, &in.IfUnmodifiedSince)
	if v := r.Header.Get("Range"); v != "" {
		in.Range = aws.String(v)
	}
	if strings.EqualFold(r.Header.Get("x-amz-checksum-mode"), "enabled") {
		in.ChecksumMode = types.ChecksumModeEnabled
	}
	query := r.URL.Query()
	if v := query.Get(qpVersionID); v != "" {
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
		return s3err.FromSDKError(err, r.URL.Path)
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

func (app *S3RP) headObject(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	in := &s3.HeadObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	applyConditionalHeaders(r, &in.IfMatch, &in.IfNoneMatch, &in.IfModifiedSince, &in.IfUnmodifiedSince)
	if v := r.Header.Get("Range"); v != "" {
		in.Range = aws.String(v)
	}
	if v := r.URL.Query().Get(qpVersionID); v != "" {
		in.VersionId = aws.String(v)
	}
	if strings.EqualFold(r.Header.Get("x-amz-checksum-mode"), "enabled") {
		in.ChecksumMode = types.ChecksumModeEnabled
	}
	out, err := rt.client.HeadObject(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
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

func (app *S3RP) putObject(c *opCtx) error {
	w, r, rt, key, vr := c.w, c.r, c.rt, c.key, c.vr
	in := &s3.PutObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	body, length, s3e := requestBody(r, vr)
	if s3e != nil {
		return s3e
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
	applyObjectLockHeaders(r, &in.ObjectLockMode, &in.ObjectLockRetainUntilDate, &in.ObjectLockLegalHoldStatus)
	if md := metadataFromHeaders(r.Header); len(md) > 0 {
		in.Metadata = md
	}
	cs := checksum.FromHeaders(r.Header)
	in.ChecksumCRC32 = cs.CRC32
	in.ChecksumCRC32C = cs.CRC32C
	in.ChecksumCRC64NVME = cs.CRC64NVME
	in.ChecksumSHA1 = cs.SHA1
	in.ChecksumSHA256 = cs.SHA256
	if alg := checksum.TrailerAlgorithm(r.Header); alg != "" {
		// the client sends the checksum as an aws-chunked trailer, which
		// is verified by the chunked reader; the backend SDK recomputes
		// and stores it (an explicit ChecksumAlgorithm forces calculation
		// even with RequestChecksumCalculationWhenRequired)
		in.ChecksumAlgorithm = types.ChecksumAlgorithm(strings.ToUpper(alg))
	}

	out, err := rt.client.PutObject(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	if out.ETag != nil {
		w.Header().Set("ETag", *out.ETag)
	}
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
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

func (app *S3RP) deleteObject(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	in := &s3.DeleteObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	if v := r.URL.Query().Get(qpVersionID); v != "" {
		in.VersionId = aws.String(v)
	}
	if bypassGovernanceRetention(r) {
		in.BypassGovernanceRetention = aws.Bool(true)
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

func (app *S3RP) listObjectsV2(c *opCtx) error {
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
			result.CommonPrefixes = append(result.CommonPrefixes, s3xml.CommonPrefix{Prefix: *cp.Prefix})
		}
	}
	return s3xml.Write(w, result)
}

func objectsFromSDK(objects []types.Object) []s3xml.Object {
	result := make([]s3xml.Object, 0, len(objects))
	for _, obj := range objects {
		o := s3xml.Object{
			StorageClass: string(obj.StorageClass),
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
		if obj.Owner != nil {
			o.Owner = &s3xml.Owner{}
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

func (app *S3RP) listObjectsV1(c *opCtx) error {
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
	result.Contents = objectsFromSDK(out.Contents)
	for _, cp := range out.CommonPrefixes {
		if cp.Prefix != nil {
			result.CommonPrefixes = append(result.CommonPrefixes, s3xml.CommonPrefix{Prefix: *cp.Prefix})
		}
	}
	return s3xml.Write(w, result)
}

// getBucketLocation answers from the config without calling the backend.
func (app *S3RP) getBucketLocation(c *opCtx) error {
	w, rt := c.w, c.rt
	region := rt.cfg.Backend.Region
	if region == "us-east-1" {
		// S3 convention: us-east-1 is represented as an empty value
		region = ""
	}
	return s3xml.Write(w, &s3xml.LocationConstraint{XMLNS: s3xml.Namespace, Value: region})
}

func (app *S3RP) deleteObjects(c *opCtx) error {
	w, r, rt, vr := c.w, c.r, c.rt, c.vr
	body, _, s3e := requestBody(r, vr)
	if s3e != nil {
		return s3e
	}
	data, err := io.ReadAll(io.LimitReader(body, maxXMLBodySize))
	if err != nil {
		return s3err.New(http.StatusBadRequest, "InvalidRequest", "failed to read request body")
	}
	var req s3xml.DeleteRequest
	if err := xml.Unmarshal(data, &req); err != nil {
		return s3err.New(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema.")
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
	bypass := bypassGovernanceRetention(r)
	delAuth := app.perObjectAuthorizer(vr, rt.cfg, "s3:DeleteObject")
	var bypassAuth perObjectAuthorizer
	if bypass {
		bypassAuth = app.perObjectAuthorizer(vr, rt.cfg, "s3:BypassGovernanceRetention")
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
	if bypassGovernanceRetention(r) {
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

func (app *S3RP) headBucket(c *opCtx) error {
	w, r, rt := c.w, c.r, c.rt
	in := &s3.HeadBucketInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
	}
	if _, err := rt.client.HeadBucket(r.Context(), in); err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (app *S3RP) listBuckets(w http.ResponseWriter, r *http.Request, vr *verifiedRequest) error {
	names, err := app.store.ListBucketNames(r.Context(), vr.Tenant)
	if err != nil {
		slog.ErrorContext(r.Context(), "failed to list buckets", "error", err)
		return s3err.New(http.StatusInternalServerError, "InternalError", "bucket lookup failed")
	}
	sort.Strings(names)
	result := &s3xml.ListAllMyBucketsResult{
		XMLNS: s3xml.Namespace,
		Owner: s3xml.Owner{ID: vr.Tenant, DisplayName: vr.Tenant},
	}
	for _, name := range names {
		result.Buckets.Bucket = append(result.Buckets.Bucket, s3xml.BucketEntry{
			Name: name,
			// buckets are static definitions; expose a fixed date
			CreationDate: s3xml.FormatTime(time.Unix(0, 0)),
		})
	}
	return s3xml.Write(w, result)
}

// requestBody returns the payload reader and its decoded length,
// decoding aws-chunked framing when the request declares it.
func requestBody(r *http.Request, vr *verifiedRequest) (io.Reader, int64, *s3err.Error) {
	switch {
	case sigv4.IsStreaming(vr.PayloadHash):
		decodedLength := r.Header.Get("x-amz-decoded-content-length")
		if decodedLength == "" {
			return nil, 0, s3err.New(http.StatusLengthRequired, "MissingContentLength",
				"You must provide the x-amz-decoded-content-length HTTP header.")
		}
		length, err := strconv.ParseInt(decodedLength, 10, 64)
		if err != nil || length < 0 {
			return nil, 0, s3err.New(http.StatusBadRequest, "InvalidRequest",
				"Invalid x-amz-decoded-content-length header")
		}
		return sigv4.NewChunkedReader(r.Body, vr.Verified, checksum.TrailerAlgorithm(r.Header)), length, nil
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
