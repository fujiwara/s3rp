package s3rp

import (
	"encoding/xml"
	"io"
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func (app *S3RP) getObjectTagging(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string) error {
	in := &s3.GetObjectTaggingInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	out, err := rt.client.GetObjectTagging(r.Context(), in)
	if err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	result := &Tagging{XMLNS: s3XMLNS}
	for _, tag := range out.TagSet {
		result.TagSet.Tags = append(result.TagSet.Tags, Tag{
			Key:   aws.ToString(tag.Key),
			Value: aws.ToString(tag.Value),
		})
	}
	return writeXML(w, result)
}

func (app *S3RP) putObjectTagging(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string, vr *verifiedRequest) error {
	body, _, s3err := requestBody(r, vr)
	if s3err != nil {
		return s3err
	}
	data, err := io.ReadAll(io.LimitReader(body, maxXMLBodySize))
	if err != nil {
		return newS3Error(http.StatusBadRequest, "InvalidRequest", "failed to read request body")
	}
	var req Tagging
	if err := xml.Unmarshal(data, &req); err != nil {
		return newS3Error(http.StatusBadRequest, "MalformedXML",
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
	out, err := rt.client.PutObjectTagging(r.Context(), in)
	if err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (app *S3RP) deleteObjectTagging(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string) error {
	in := &s3.DeleteObjectTaggingInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	out, err := rt.client.DeleteObjectTagging(r.Context(), in)
	if err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
