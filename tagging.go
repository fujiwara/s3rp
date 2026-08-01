package s3rp

import (
	"encoding/xml"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func (app *S3RP) getObjectTagging(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	in := &s3.GetObjectTaggingInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	if v := r.URL.Query().Get(qpVersionID); v != "" {
		in.VersionId = aws.String(v)
	}
	out, err := rt.client.GetObjectTagging(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	result := &s3xml.Tagging{XMLNS: s3xml.Namespace}
	for _, tag := range out.TagSet {
		result.TagSet.Tags = append(result.TagSet.Tags, s3xml.Tag{
			Key:   aws.ToString(tag.Key),
			Value: aws.ToString(tag.Value),
		})
	}
	return s3xml.Write(w, result)
}

func (app *S3RP) putObjectTagging(c *opCtx) error {
	w, r, rt, key, vr := c.w, c.r, c.rt, c.key, c.vr
	body, _, s3e := requestBody(r, vr)
	if s3e != nil {
		return s3e
	}
	data, err := io.ReadAll(io.LimitReader(body, maxXMLBodySize))
	if err != nil {
		return s3err.New(http.StatusBadRequest, "InvalidRequest", "failed to read request body")
	}
	var req s3xml.Tagging
	if err := xml.Unmarshal(data, &req); err != nil {
		return s3err.New(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema.")
	}
	tags := make([]types.Tag, 0, len(req.TagSet.Tags))
	for _, tag := range req.TagSet.Tags {
		tags = append(tags, types.Tag{
			Key:   aws.String(tag.Key),
			Value: aws.String(tag.Value),
		})
	}
	in := &s3.PutObjectTaggingInput{
		Bucket:  aws.String(rt.cfg.Backend.Bucket),
		Key:     aws.String(key),
		Tagging: &types.Tagging{TagSet: tags},
	}
	if v := r.URL.Query().Get(qpVersionID); v != "" {
		in.VersionId = aws.String(v)
	}
	out, err := rt.client.PutObjectTagging(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (app *S3RP) deleteObjectTagging(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	in := &s3.DeleteObjectTaggingInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	if v := r.URL.Query().Get(qpVersionID); v != "" {
		in.VersionId = aws.String(v)
	}
	out, err := rt.client.DeleteObjectTagging(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
