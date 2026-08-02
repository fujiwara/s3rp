package s3gw

import (
	"encoding/xml"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/fujiwara/s3rp/checksum"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// maxXMLBodySize limits XML request bodies (CompleteMultipartUpload,
// DeleteObjects); the maximum entries of both fit well within this.
const maxXMLBodySize = 16 << 20

func (g *Gateway) createMultipartUpload(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	in := &s3.CreateMultipartUploadInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	if v := r.Header.Get("Content-Type"); v != "" {
		in.ContentType = aws.String(v)
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
	if v := r.Header.Get("x-amz-checksum-algorithm"); v != "" {
		in.ChecksumAlgorithm = types.ChecksumAlgorithm(strings.ToUpper(v))
	}
	if v := r.Header.Get("x-amz-checksum-type"); v != "" {
		in.ChecksumType = types.ChecksumType(strings.ToUpper(v))
	}
	if s3e := applySSE(r.Header.Get, &in.ServerSideEncryption, &in.SSEKMSKeyId); s3e != nil {
		return s3e
	}
	out, err := rt.client.CreateMultipartUpload(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	if out.ChecksumAlgorithm != "" {
		w.Header().Set("x-amz-checksum-algorithm", string(out.ChecksumAlgorithm))
	}
	if out.ChecksumType != "" {
		w.Header().Set("x-amz-checksum-type", string(out.ChecksumType))
	}
	setSSEHeaders(w.Header(), out.ServerSideEncryption, out.SSEKMSKeyId)
	return s3xml.Write(w, &s3xml.InitiateMultipartUploadResult{
		XMLNS:    s3xml.Namespace,
		Bucket:   rt.cfg.Name, // the front bucket name, not the backend one
		Key:      key,
		UploadID: aws.ToString(out.UploadId),
	})
}

func (g *Gateway) uploadPart(c *opCtx) error {
	w, r, rt, key, vr := c.w, c.r, c.rt, c.key, c.vr
	query := r.URL.Query()
	partNumber, err := strconv.ParseInt(query.Get("partNumber"), 10, 32)
	if err != nil {
		return s3err.New(http.StatusBadRequest, "InvalidArgument",
			"Argument partNumber must be an integer.")
	}
	in := &s3.UploadPartInput{
		Bucket:     aws.String(rt.cfg.Backend.Bucket),
		Key:        aws.String(key),
		UploadId:   aws.String(query.Get("uploadId")),
		PartNumber: aws.Int32(int32(partNumber)),
	}
	body, length, s3e := requestBody(r, vr)
	if s3e != nil {
		return s3e
	}
	in.Body = body
	in.ContentLength = aws.Int64(length)
	if v := r.Header.Get("Content-MD5"); v != "" {
		in.ContentMD5 = aws.String(v)
	}
	cs := checksum.FromHeaders(r.Header)
	in.ChecksumCRC32 = cs.CRC32
	in.ChecksumCRC32C = cs.CRC32C
	in.ChecksumCRC64NVME = cs.CRC64NVME
	in.ChecksumSHA1 = cs.SHA1
	in.ChecksumSHA256 = cs.SHA256
	if alg := checksum.TrailerAlgorithm(r.Header); alg != "" {
		in.ChecksumAlgorithm = types.ChecksumAlgorithm(strings.ToUpper(alg))
	}
	out, err := rt.client.UploadPart(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	if out.ETag != nil {
		w.Header().Set("ETag", *out.ETag)
	}
	setSSEHeaders(w.Header(), out.ServerSideEncryption, out.SSEKMSKeyId)
	checksum.SetHeaders(w.Header(), checksum.Values{
		CRC32:     out.ChecksumCRC32,
		CRC32C:    out.ChecksumCRC32C,
		CRC64NVME: out.ChecksumCRC64NVME,
		SHA1:      out.ChecksumSHA1,
		SHA256:    out.ChecksumSHA256,
	}, "")
	w.WriteHeader(http.StatusOK)
	return nil
}

func (g *Gateway) completeMultipartUpload(c *opCtx) error {
	w, r, rt, key, vr := c.w, c.r, c.rt, c.key, c.vr
	body, _, s3e := requestBody(r, vr)
	if s3e != nil {
		return s3e
	}
	data, err := io.ReadAll(io.LimitReader(body, maxXMLBodySize))
	if err != nil {
		return s3err.New(http.StatusBadRequest, "InvalidRequest", "failed to read request body")
	}
	var req s3xml.CompleteMultipartUploadRequest
	if err := xml.Unmarshal(data, &req); err != nil {
		return s3err.New(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema.")
	}
	if len(req.Parts) == 0 {
		return s3err.New(http.StatusBadRequest, "InvalidRequest",
			"You must specify at least one part")
	}
	parts := make([]types.CompletedPart, 0, len(req.Parts))
	for _, p := range req.Parts {
		cp := types.CompletedPart{
			PartNumber: aws.Int32(p.PartNumber),
			ETag:       aws.String(p.ETag),
		}
		setIfNotEmpty := func(dst **string, v string) {
			if v != "" {
				*dst = aws.String(v)
			}
		}
		setIfNotEmpty(&cp.ChecksumCRC32, p.ChecksumCRC32)
		setIfNotEmpty(&cp.ChecksumCRC32C, p.ChecksumCRC32C)
		setIfNotEmpty(&cp.ChecksumCRC64NVME, p.ChecksumCRC64NVME)
		setIfNotEmpty(&cp.ChecksumSHA1, p.ChecksumSHA1)
		setIfNotEmpty(&cp.ChecksumSHA256, p.ChecksumSHA256)
		parts = append(parts, cp)
	}
	in := &s3.CompleteMultipartUploadInput{
		Bucket:          aws.String(rt.cfg.Backend.Bucket),
		Key:             aws.String(key),
		UploadId:        aws.String(r.URL.Query().Get("uploadId")),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	}
	if v := r.Header.Get("x-amz-checksum-type"); v != "" {
		in.ChecksumType = types.ChecksumType(strings.ToUpper(v))
	}
	cs := checksum.FromHeaders(r.Header)
	in.ChecksumCRC32 = cs.CRC32
	in.ChecksumCRC32C = cs.CRC32C
	in.ChecksumCRC64NVME = cs.CRC64NVME
	in.ChecksumSHA1 = cs.SHA1
	in.ChecksumSHA256 = cs.SHA256
	if v := r.Header.Get("x-amz-mp-object-size"); v != "" {
		if size, err := strconv.ParseInt(v, 10, 64); err == nil {
			in.MpuObjectSize = aws.Int64(size)
		}
	}
	out, err := rt.client.CompleteMultipartUpload(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	location := (&url.URL{
		Scheme: scheme,
		Host:   r.Host,
		Path:   "/" + rt.cfg.Name + "/" + key,
	}).String()
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	setSSEHeaders(w.Header(), out.ServerSideEncryption, out.SSEKMSKeyId)
	return s3xml.Write(w, &s3xml.CompleteMultipartUploadResult{
		XMLNS:             s3xml.Namespace,
		Location:          location,
		Bucket:            rt.cfg.Name,
		Key:               key,
		ETag:              aws.ToString(out.ETag),
		ChecksumCRC32:     aws.ToString(out.ChecksumCRC32),
		ChecksumCRC32C:    aws.ToString(out.ChecksumCRC32C),
		ChecksumCRC64NVME: aws.ToString(out.ChecksumCRC64NVME),
		ChecksumSHA1:      aws.ToString(out.ChecksumSHA1),
		ChecksumSHA256:    aws.ToString(out.ChecksumSHA256),
		ChecksumType:      string(out.ChecksumType),
	})
}

func (g *Gateway) abortMultipartUpload(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	in := &s3.AbortMultipartUploadInput{
		Bucket:   aws.String(rt.cfg.Backend.Bucket),
		Key:      aws.String(key),
		UploadId: aws.String(r.URL.Query().Get("uploadId")),
	}
	if _, err := rt.client.AbortMultipartUpload(r.Context(), in); err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

func (g *Gateway) listParts(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	query := r.URL.Query()
	in := &s3.ListPartsInput{
		Bucket:   aws.String(rt.cfg.Backend.Bucket),
		Key:      aws.String(key),
		UploadId: aws.String(query.Get("uploadId")),
	}
	if v := query.Get("max-parts"); v != "" {
		maxParts, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return s3err.New(http.StatusBadRequest, "InvalidArgument",
				"Argument max-parts must be an integer.")
		}
		in.MaxParts = aws.Int32(int32(maxParts))
	}
	if v := query.Get("part-number-marker"); v != "" {
		in.PartNumberMarker = aws.String(v)
	}
	out, err := rt.client.ListParts(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	result := &s3xml.ListPartsResult{
		XMLNS:        s3xml.Namespace,
		Bucket:       rt.cfg.Name,
		Key:          key,
		UploadID:     aws.ToString(in.UploadId),
		StorageClass: string(out.StorageClass),
	}
	if out.PartNumberMarker != nil {
		result.PartNumberMarker = *out.PartNumberMarker
	}
	if out.NextPartNumberMarker != nil {
		result.NextPartNumberMarker = *out.NextPartNumberMarker
	}
	if out.MaxParts != nil {
		result.MaxParts = *out.MaxParts
	}
	if out.IsTruncated != nil {
		result.IsTruncated = *out.IsTruncated
	}
	for _, p := range out.Parts {
		part := s3xml.Part{}
		if p.PartNumber != nil {
			part.PartNumber = *p.PartNumber
		}
		if p.LastModified != nil {
			part.LastModified = s3xml.FormatTime(*p.LastModified)
		}
		if p.ETag != nil {
			part.ETag = *p.ETag
		}
		if p.Size != nil {
			part.Size = *p.Size
		}
		result.Parts = append(result.Parts, part)
	}
	return s3xml.Write(w, result)
}

func (g *Gateway) listMultipartUploads(c *opCtx) error {
	w, r, rt := c.w, c.r, c.rt
	query := r.URL.Query()
	in := &s3.ListMultipartUploadsInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
	}
	if v := query.Get("prefix"); v != "" {
		in.Prefix = aws.String(v)
	}
	if v := query.Get("delimiter"); v != "" {
		in.Delimiter = aws.String(v)
	}
	if v := query.Get("key-marker"); v != "" {
		in.KeyMarker = aws.String(v)
	}
	if v := query.Get("upload-id-marker"); v != "" {
		in.UploadIdMarker = aws.String(v)
	}
	if v := query.Get("max-uploads"); v != "" {
		maxUploads, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return s3err.New(http.StatusBadRequest, "InvalidArgument",
				"Argument max-uploads must be an integer.")
		}
		in.MaxUploads = aws.Int32(int32(maxUploads))
	}
	if v := query.Get("encoding-type"); v != "" {
		in.EncodingType = types.EncodingType(v)
	}
	out, err := rt.client.ListMultipartUploads(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	result := &s3xml.ListMultipartUploadsResult{
		XMLNS:  s3xml.Namespace,
		Bucket: rt.cfg.Name,
	}
	if out.KeyMarker != nil {
		result.KeyMarker = *out.KeyMarker
	}
	if out.UploadIdMarker != nil {
		result.UploadIDMarker = *out.UploadIdMarker
	}
	if out.NextKeyMarker != nil {
		result.NextKeyMarker = *out.NextKeyMarker
	}
	if out.NextUploadIdMarker != nil {
		result.NextUploadIDMarker = *out.NextUploadIdMarker
	}
	if out.Delimiter != nil {
		result.Delimiter = *out.Delimiter
	}
	if out.Prefix != nil {
		result.Prefix = *out.Prefix
	}
	if out.MaxUploads != nil {
		result.MaxUploads = *out.MaxUploads
	}
	if out.IsTruncated != nil {
		result.IsTruncated = *out.IsTruncated
	}
	for _, u := range out.Uploads {
		upload := s3xml.Upload{
			StorageClass: string(u.StorageClass),
		}
		if u.Key != nil {
			upload.Key = *u.Key
		}
		if u.UploadId != nil {
			upload.UploadID = *u.UploadId
		}
		if u.Initiated != nil {
			upload.Initiated = s3xml.FormatTime(*u.Initiated)
		}
		result.Uploads = append(result.Uploads, upload)
	}
	for _, cp := range out.CommonPrefixes {
		if cp.Prefix != nil {
			result.CommonPrefixes = append(result.CommonPrefixes, s3xml.CommonPrefix{Prefix: *cp.Prefix})
		}
	}
	return s3xml.Write(w, result)
}
