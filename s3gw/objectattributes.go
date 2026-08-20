package s3gw

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"
)

// getObjectAttributes proxies GetObjectAttributes (?attributes). The
// requested attribute set travels in the x-amz-object-attributes header;
// the response exposes no backend identity, so the output maps through
// unchanged — except the ETag, which this API alone returns unquoted.
func (g *Gateway) getObjectAttributes(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	attrs := objectAttributesFromHeader(c.hdr)
	if len(attrs) == 0 {
		return s3err.New(http.StatusBadRequest, "InvalidArgument",
			"The x-amz-object-attributes header specifying the attributes to be retrieved is required.")
	}
	in := &s3.GetObjectAttributesInput{
		Bucket:           aws.String(rt.cfg.Backend.Bucket),
		Key:              aws.String(key),
		ObjectAttributes: attrs,
	}
	if v := r.URL.Query().Get(qpVersionID); v != "" {
		in.VersionId = aws.String(v)
	}
	if v := c.signed("x-amz-max-parts"); v != "" {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return s3err.New(http.StatusBadRequest, "InvalidArgument",
				"x-amz-max-parts must be an integer").WithCause(err)
		}
		in.MaxParts = aws.Int32(int32(n))
	}
	if v := c.signed("x-amz-part-number-marker"); v != "" {
		in.PartNumberMarker = aws.String(v)
	}

	out, err := rt.client.GetObjectAttributes(r.Context(), in)
	if err != nil {
		relayErrorHeaders(w.Header(), err)
		return s3err.FromSDKError(err, r.URL.Path)
	}

	h := w.Header()
	if out.LastModified != nil {
		h.Set("Last-Modified", out.LastModified.UTC().Format(http.TimeFormat))
	}
	if out.VersionId != nil {
		h.Set("x-amz-version-id", *out.VersionId)
	}
	if out.DeleteMarker != nil && *out.DeleteMarker {
		h.Set("x-amz-delete-marker", "true")
	}

	result := &s3xml.GetObjectAttributesResult{
		XMLNS:        s3xml.Namespace,
		StorageClass: string(out.StorageClass),
		ObjectSize:   out.ObjectSize,
	}
	if out.ETag != nil {
		result.ETag = strings.Trim(*out.ETag, `"`)
	}
	if cs := out.Checksum; cs != nil {
		result.Checksum = &s3xml.AttributesChecksum{
			ChecksumCRC32:     aws.ToString(cs.ChecksumCRC32),
			ChecksumCRC32C:    aws.ToString(cs.ChecksumCRC32C),
			ChecksumCRC64NVME: aws.ToString(cs.ChecksumCRC64NVME),
			ChecksumSHA1:      aws.ToString(cs.ChecksumSHA1),
			ChecksumSHA256:    aws.ToString(cs.ChecksumSHA256),
			ChecksumType:      string(cs.ChecksumType),
		}
	}
	if op := out.ObjectParts; op != nil {
		parts := &s3xml.ObjectAttributeParts{
			IsTruncated:          aws.ToBool(op.IsTruncated),
			MaxParts:             op.MaxParts,
			NextPartNumberMarker: aws.ToString(op.NextPartNumberMarker),
			PartNumberMarker:     aws.ToString(op.PartNumberMarker),
			PartsCount:           op.TotalPartsCount,
		}
		for _, p := range op.Parts {
			parts.Parts = append(parts.Parts, s3xml.ObjectAttributePart{
				PartNumber:        aws.ToInt32(p.PartNumber),
				Size:              aws.ToInt64(p.Size),
				ChecksumCRC32:     aws.ToString(p.ChecksumCRC32),
				ChecksumCRC32C:    aws.ToString(p.ChecksumCRC32C),
				ChecksumCRC64NVME: aws.ToString(p.ChecksumCRC64NVME),
				ChecksumSHA1:      aws.ToString(p.ChecksumSHA1),
				ChecksumSHA256:    aws.ToString(p.ChecksumSHA256),
			})
		}
		result.ObjectParts = parts
	}
	return s3xml.Write(w, result)
}

// objectAttributesFromHeader parses x-amz-object-attributes: SDKs send one
// header value per attribute, other clients a comma-separated list.
func objectAttributesFromHeader(hdr signedHeader) []types.ObjectAttributes {
	var attrs []types.ObjectAttributes
	for _, v := range hdr.SignedValues("x-amz-object-attributes") {
		for part := range strings.SplitSeq(v, ",") {
			if p := strings.TrimSpace(part); p != "" {
				attrs = append(attrs, types.ObjectAttributes(p))
			}
		}
	}
	return attrs
}
