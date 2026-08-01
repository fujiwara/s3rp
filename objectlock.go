package s3rp

import (
	"encoding/xml"
	"io"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Object Lock is passed through to the backend, which enforces the WORM
// semantics. The bucket must have been created with Object Lock enabled
// on the backend (s3rp does not proxy CreateBucket).

const objectLockTimeFormat = "2006-01-02T15:04:05.000Z"

func (app *S3RP) getObjectLockConfiguration(w http.ResponseWriter, r *http.Request, rt *bucketRT) error {
	out, err := rt.client.GetObjectLockConfiguration(r.Context(), &s3.GetObjectLockConfigurationInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
	})
	if err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	result := &ObjectLockConfiguration{XMLNS: s3XMLNS}
	if c := out.ObjectLockConfiguration; c != nil {
		result.ObjectLockEnabled = string(c.ObjectLockEnabled)
		if c.Rule != nil && c.Rule.DefaultRetention != nil {
			dr := c.Rule.DefaultRetention
			result.Rule = &ObjectLockRule{DefaultRetention: &DefaultRetention{
				Mode: string(dr.Mode),
			}}
			if dr.Days != nil {
				result.Rule.DefaultRetention.Days = *dr.Days
			}
			if dr.Years != nil {
				result.Rule.DefaultRetention.Years = *dr.Years
			}
		}
	}
	return writeXML(w, result)
}

func (app *S3RP) putObjectLockConfiguration(w http.ResponseWriter, r *http.Request, rt *bucketRT, vr *verifiedRequest) error {
	var req ObjectLockConfiguration
	if err := readXMLBody(r, vr, &req); err != nil {
		return err
	}
	in := &s3.PutObjectLockConfigurationInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		ObjectLockConfiguration: &types.ObjectLockConfiguration{
			ObjectLockEnabled: types.ObjectLockEnabled(req.ObjectLockEnabled),
		},
	}
	if req.Rule != nil && req.Rule.DefaultRetention != nil {
		dr := req.Rule.DefaultRetention
		in.ObjectLockConfiguration.Rule = &types.ObjectLockRule{
			DefaultRetention: &types.DefaultRetention{
				Mode: types.ObjectLockRetentionMode(dr.Mode),
			},
		}
		if dr.Days > 0 {
			in.ObjectLockConfiguration.Rule.DefaultRetention.Days = aws.Int32(dr.Days)
		}
		if dr.Years > 0 {
			in.ObjectLockConfiguration.Rule.DefaultRetention.Years = aws.Int32(dr.Years)
		}
	}
	if v := r.Header.Get("x-amz-bucket-object-lock-token"); v != "" {
		in.Token = aws.String(v)
	}
	if _, err := rt.client.PutObjectLockConfiguration(r.Context(), in); err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (app *S3RP) getObjectRetention(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string) error {
	in := &s3.GetObjectRetentionInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	if v := r.URL.Query().Get("versionId"); v != "" {
		in.VersionId = aws.String(v)
	}
	out, err := rt.client.GetObjectRetention(r.Context(), in)
	if err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	result := &ObjectLockRetention{XMLNS: s3XMLNS}
	if out.Retention != nil {
		result.Mode = string(out.Retention.Mode)
		if out.Retention.RetainUntilDate != nil {
			result.RetainUntilDate = out.Retention.RetainUntilDate.UTC().Format(objectLockTimeFormat)
		}
	}
	return writeXML(w, result)
}

func (app *S3RP) putObjectRetention(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string, vr *verifiedRequest) error {
	var req ObjectLockRetention
	if err := readXMLBody(r, vr, &req); err != nil {
		return err
	}
	in := &s3.PutObjectRetentionInput{
		Bucket:    aws.String(rt.cfg.Backend.Bucket),
		Key:       aws.String(key),
		Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionMode(req.Mode)},
	}
	if req.RetainUntilDate != "" {
		t, err := time.Parse(objectLockTimeFormat, req.RetainUntilDate)
		if err != nil {
			t, err = time.Parse(time.RFC3339, req.RetainUntilDate)
		}
		if err != nil {
			return newS3Error(http.StatusBadRequest, "InvalidRequest", "invalid RetainUntilDate")
		}
		in.Retention.RetainUntilDate = aws.Time(t)
	}
	if v := r.URL.Query().Get("versionId"); v != "" {
		in.VersionId = aws.String(v)
	}
	if bypassGovernanceRetention(r) {
		in.BypassGovernanceRetention = aws.Bool(true)
	}
	if _, err := rt.client.PutObjectRetention(r.Context(), in); err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

func (app *S3RP) getObjectLegalHold(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string) error {
	in := &s3.GetObjectLegalHoldInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	if v := r.URL.Query().Get("versionId"); v != "" {
		in.VersionId = aws.String(v)
	}
	out, err := rt.client.GetObjectLegalHold(r.Context(), in)
	if err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	result := &ObjectLockLegalHold{XMLNS: s3XMLNS}
	if out.LegalHold != nil {
		result.Status = string(out.LegalHold.Status)
	}
	return writeXML(w, result)
}

func (app *S3RP) putObjectLegalHold(w http.ResponseWriter, r *http.Request, rt *bucketRT, key string, vr *verifiedRequest) error {
	var req ObjectLockLegalHold
	if err := readXMLBody(r, vr, &req); err != nil {
		return err
	}
	in := &s3.PutObjectLegalHoldInput{
		Bucket:    aws.String(rt.cfg.Backend.Bucket),
		Key:       aws.String(key),
		LegalHold: &types.ObjectLockLegalHold{Status: types.ObjectLockLegalHoldStatus(req.Status)},
	}
	if v := r.URL.Query().Get("versionId"); v != "" {
		in.VersionId = aws.String(v)
	}
	if _, err := rt.client.PutObjectLegalHold(r.Context(), in); err != nil {
		return fromSDKError(err, r.URL.Path)
	}
	w.WriteHeader(http.StatusOK)
	return nil
}

// bypassGovernanceRetention reports whether the request asks to bypass
// governance-mode retention.
func bypassGovernanceRetention(r *http.Request) bool {
	return r.Header.Get("x-amz-bypass-governance-retention") == "true"
}

// applyObjectLockInput copies the object-lock upload headers onto a
// PutObject/CreateMultipartUpload-style input via the provided setters.
func applyObjectLockHeaders(r *http.Request, mode *types.ObjectLockMode, retainUntil **time.Time, legalHold *types.ObjectLockLegalHoldStatus) {
	if v := r.Header.Get("x-amz-object-lock-mode"); v != "" {
		*mode = types.ObjectLockMode(v)
	}
	if v := r.Header.Get("x-amz-object-lock-retain-until-date"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			*retainUntil = aws.Time(t)
		}
	}
	if v := r.Header.Get("x-amz-object-lock-legal-hold"); v != "" {
		*legalHold = types.ObjectLockLegalHoldStatus(v)
	}
}

// setObjectLockResponseHeaders copies object-lock fields from a
// GetObject/HeadObject output onto the response.
func setObjectLockResponseHeaders(h http.Header, mode types.ObjectLockMode, retainUntil *time.Time, legalHold types.ObjectLockLegalHoldStatus) {
	if mode != "" {
		h.Set("x-amz-object-lock-mode", string(mode))
	}
	if retainUntil != nil {
		h.Set("x-amz-object-lock-retain-until-date", retainUntil.UTC().Format(time.RFC3339))
	}
	if legalHold != "" {
		h.Set("x-amz-object-lock-legal-hold", string(legalHold))
	}
}

// readXMLBody decodes an XML request body (aws-chunked aware) into v.
func readXMLBody(r *http.Request, vr *verifiedRequest, v any) *S3Error {
	body, _, s3err := requestBody(r, vr)
	if s3err != nil {
		return s3err
	}
	data, err := io.ReadAll(io.LimitReader(body, maxXMLBodySize))
	if err != nil {
		return newS3Error(http.StatusBadRequest, "InvalidRequest", "failed to read request body")
	}
	if err := xml.Unmarshal(data, v); err != nil {
		return newS3Error(http.StatusBadRequest, "MalformedXML",
			"The XML you provided was not well-formed or did not validate against our published schema.")
	}
	return nil
}
