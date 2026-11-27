package s3gw

import (
	"net/http"

	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func (g *Gateway) getObjectTagging(c *opCtx) error {
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

// readTagSet reads and validates the body of a PutObjectTagging, keeps it
// for the handler and returns it as Op.Request reports it.
func (c *opCtx) readTagSet() (map[string]string, *s3err.Error) {
	var req s3xml.Tagging
	if s3e := readXMLBody(c, &req); s3e != nil {
		return nil, s3e
	}
	set, s3e := checkTagSet(req.TagSet.Tags)
	if s3e != nil {
		return nil, s3e
	}
	c.tagSet = req.TagSet.Tags
	return set, nil
}

func (g *Gateway) putObjectTagging(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	tags := make([]types.Tag, 0, len(c.tagSet))
	for _, tag := range c.tagSet {
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

func (g *Gateway) deleteObjectTagging(c *opCtx) error {
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
