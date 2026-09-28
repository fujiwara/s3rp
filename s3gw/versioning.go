package s3gw

import (
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
		XMLNS:               s3xml.Namespace,
		Name:                rt.cfg.Name, // the front bucket name, not the backend one
		Prefix:              aws.ToString(out.Prefix),
		Delimiter:           aws.ToString(out.Delimiter),
		KeyMarker:           aws.ToString(out.KeyMarker),
		VersionIDMarker:     aws.ToString(out.VersionIdMarker),
		NextKeyMarker:       aws.ToString(out.NextKeyMarker),
		NextVersionIDMarker: aws.ToString(out.NextVersionIdMarker),
		MaxKeys:             aws.ToInt32(out.MaxKeys),
		EncodingType:        string(out.EncodingType),
		IsTruncated:         aws.ToBool(out.IsTruncated),
	}
	// AWS always carries an Owner on versions and delete markers; the
	// tenant, never the backend's account
	owner := tenantOwner(c.rt.cfg.Tenant)
	for _, v := range out.Versions {
		version := s3xml.ObjectVersion{
			StorageClass:      c.clientStorageClass(string(v.StorageClass)),
			Owner:             owner,
			Key:               aws.ToString(v.Key),
			VersionID:         aws.ToString(v.VersionId),
			IsLatest:          aws.ToBool(v.IsLatest),
			ETag:              aws.ToString(v.ETag),
			Size:              aws.ToInt64(v.Size),
			ChecksumAlgorithm: c.reportAlgorithms(v.ChecksumAlgorithm),
			ChecksumType:      string(v.ChecksumType),
		}
		if v.LastModified != nil {
			version.LastModified = s3xml.FormatTime(*v.LastModified)
		}
		result.Versions = append(result.Versions, version)
	}
	for _, d := range out.DeleteMarkers {
		marker := s3xml.DeleteMarkerEntry{Owner: owner}
		marker.Key = aws.ToString(d.Key)
		marker.VersionID = aws.ToString(d.VersionId)
		marker.IsLatest = aws.ToBool(d.IsLatest)
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
