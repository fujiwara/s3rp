package s3gw

import (
	"encoding/xml"
	"io"
	"net/http"
	"strconv"

	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func (g *Gateway) getBucketVersioning(c *opCtx) error {
	w, r, rt := c.w, c.r, c.rt
	in := &s3.GetBucketVersioningInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
	}
	out, err := rt.client.GetBucketVersioning(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	return s3xml.Write(w, &s3xml.VersioningConfiguration{
		XMLNS:  s3xml.Namespace,
		Status: string(out.Status),
	})
}

func (g *Gateway) putBucketVersioning(c *opCtx) error {
	w, r, rt, vr := c.w, c.r, c.rt, c.vr
	body, _, s3e := requestBody(r, vr)
	if s3e != nil {
		return s3e
	}
	data, err := io.ReadAll(io.LimitReader(body, maxXMLBodySize))
	if err != nil {
		return s3err.New(http.StatusBadRequest, "InvalidRequest", "failed to read request body")
	}
	var req s3xml.VersioningConfiguration
	if err := xml.Unmarshal(data, &req); err != nil {
		return s3err.New(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema.")
	}
	switch req.Status {
	case "Enabled", "Suspended":
	default:
		return s3err.New(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema.")
	}
	in := &s3.PutBucketVersioningInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatus(req.Status),
		},
	}
	if _, err := rt.client.PutBucketVersioning(r.Context(), in); err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (g *Gateway) listObjectVersions(c *opCtx) error {
	w, r, rt := c.w, c.r, c.rt
	query := r.URL.Query()
	in := &s3.ListObjectVersionsInput{
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
	if v := query.Get("version-id-marker"); v != "" {
		in.VersionIdMarker = aws.String(v)
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
	out, err := rt.client.ListObjectVersions(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	result := &s3xml.ListVersionsResult{
		XMLNS: s3xml.Namespace,
		Name:  rt.cfg.Name, // the front bucket name, not the backend one
	}
	if out.Prefix != nil {
		result.Prefix = *out.Prefix
	}
	if out.Delimiter != nil {
		result.Delimiter = *out.Delimiter
	}
	if out.KeyMarker != nil {
		result.KeyMarker = *out.KeyMarker
	}
	if out.VersionIdMarker != nil {
		result.VersionIDMarker = *out.VersionIdMarker
	}
	if out.NextKeyMarker != nil {
		result.NextKeyMarker = *out.NextKeyMarker
	}
	if out.NextVersionIdMarker != nil {
		result.NextVersionIDMarker = *out.NextVersionIdMarker
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
	// AWS always carries an Owner on versions and delete markers; the
	// tenant, never the backend's account
	owner := tenantOwner(c.vr.Tenant)
	for _, v := range out.Versions {
		version := s3xml.ObjectVersion{
			StorageClass: string(v.StorageClass),
			Owner:        owner,
		}
		if v.Key != nil {
			version.Key = *v.Key
		}
		if v.VersionId != nil {
			version.VersionID = *v.VersionId
		}
		if v.IsLatest != nil {
			version.IsLatest = *v.IsLatest
		}
		if v.LastModified != nil {
			version.LastModified = s3xml.FormatTime(*v.LastModified)
		}
		if v.ETag != nil {
			version.ETag = *v.ETag
		}
		if v.Size != nil {
			version.Size = *v.Size
		}
		result.Versions = append(result.Versions, version)
	}
	for _, d := range out.DeleteMarkers {
		marker := s3xml.DeleteMarkerEntry{Owner: owner}
		if d.Key != nil {
			marker.Key = *d.Key
		}
		if d.VersionId != nil {
			marker.VersionID = *d.VersionId
		}
		if d.IsLatest != nil {
			marker.IsLatest = *d.IsLatest
		}
		if d.LastModified != nil {
			marker.LastModified = s3xml.FormatTime(*d.LastModified)
		}
		result.DeleteMarkers = append(result.DeleteMarkers, marker)
	}
	for _, cp := range out.CommonPrefixes {
		if cp.Prefix != nil {
			result.CommonPrefixes = append(result.CommonPrefixes, s3xml.CommonPrefix{Prefix: *cp.Prefix})
		}
	}
	return s3xml.Write(w, result)
}
