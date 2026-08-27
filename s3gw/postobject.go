package s3gw

import (
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp/cors"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"
	"github.com/fujiwara/s3rp/sigv4"
)

// Browser-based POST uploads: the form carries the authentication (a signed
// POST policy), so this path runs before header/query signature
// verification. The form fields are read up to the file part — everything
// after the file is ignored, as on AWS — and the file itself is streamed to
// the backend without buffering.

const (
	// maxPostFieldBytes bounds one form field value. The largest legitimate
	// field is the base64 policy, capped separately at 20 KB decoded.
	maxPostFieldBytes = 32 * kib
	// maxPostFields bounds the number of form fields before the file.
	maxPostFields = 64
)

// isMultipartForm reports whether the request is a multipart/form-data
// POST, i.e. a browser-based upload rather than an S3 API POST
// (?delete, ?uploads, ?uploadId).
func isMultipartForm(r *http.Request) bool {
	mt, _, err := mime.ParseMediaType(newSignedHeader(r, nil).Attribute("Content-Type"))
	return err == nil && mt == "multipart/form-data"
}

// readPostForm reads the form fields up to the file part, which it returns
// still unread so the caller can stream it. Field names are lower-cased
// (POST form field names are case-insensitive).
func readPostForm(r *http.Request) (fields map[string]string, file *multipart.Part, filename string, s3e *s3err.Error) {
	mr, err := r.MultipartReader()
	if err != nil {
		return nil, nil, "", s3err.New(http.StatusBadRequest, "InvalidArgument", "malformed multipart form: "+err.Error())
	}
	fields = make(map[string]string)
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return nil, nil, "", s3err.New(http.StatusBadRequest, "InvalidArgument", "Bucket POST must contain a field named 'file'.")
		}
		if err != nil {
			return nil, nil, "", s3err.New(http.StatusBadRequest, "InvalidArgument", "malformed multipart form: "+err.Error())
		}
		name := strings.ToLower(part.FormName())
		if name == "" {
			part.Close()
			continue
		}
		if name == "file" {
			return fields, part, part.FileName(), nil
		}
		if len(fields) >= maxPostFields {
			return nil, nil, "", s3err.New(http.StatusBadRequest, "InvalidArgument", "too many form fields")
		}
		v, err := io.ReadAll(io.LimitReader(part, maxPostFieldBytes+1))
		part.Close()
		if err != nil {
			return nil, nil, "", s3err.New(http.StatusBadRequest, "InvalidArgument", "malformed multipart form: "+err.Error())
		}
		if len(v) > maxPostFieldBytes {
			return nil, nil, "", s3err.New(http.StatusBadRequest, "InvalidArgument", "form field "+name+" is too large")
		}
		fields[name] = string(v)
	}
}

// postFieldMapped are the form fields the upload consumes besides the
// authentication fields; anything not listed here (or x-amz-meta-*) is
// refused loudly, mirroring how unknown query subresources get a 501.
var postFieldMapped = map[string]bool{
	"key": true, "bucket": true, "acl": true,
	"success_action_redirect": true, "success_action_status": true,
	"content-type": true, "content-md5": true, "cache-control": true,
	"content-disposition": true, "content-encoding": true,
	"content-language": true, "expires": true,
	hdrStorageClass: true, "x-amz-tagging": true,
	hdrSSE: true, hdrSSEKMSKeyID: true,
	"x-amz-algorithm": true, "x-amz-credential": true,
	"x-amz-date": true, "x-amz-signature": true, "policy": true,
	"x-amz-security-token": true,
}

func checkPostFields(fields map[string]string) *s3err.Error {
	for name := range fields {
		if postFieldMapped[name] || strings.HasPrefix(name, amzMetaPrefix) {
			continue
		}
		return s3err.NotImplemented("form field " + name)
	}
	return nil
}

func (g *Gateway) handlePostObject(w http.ResponseWriter, r *http.Request, t target) error {
	bucket := t.bucket
	if err := (paramSet{}).check(r.URL.Query()); err != nil {
		return err
	}
	// SSE-C headers are as meaningless on a POST upload as anywhere else,
	// and just as dangerous to drop silently. The nil coverage set is fine:
	// checkSSEC reads presence, signed or not.
	if err := checkSSEC(newSignedHeader(r, nil)); err != nil {
		return err
	}
	fields, file, filename, s3e := readPostForm(r)
	if s3e != nil {
		return s3e
	}
	defer file.Close()
	// x-ignore-* fields are exempt from policy conditions by convention and
	// are not forwarded
	for k := range fields {
		if strings.HasPrefix(k, "x-ignore-") {
			delete(fields, k)
		}
	}
	keyField := fields["key"]
	if keyField == "" {
		return s3err.New(http.StatusBadRequest, "InvalidArgument", "Bucket POST must contain a field named 'key'.")
	}
	key := strings.ReplaceAll(keyField, "${filename}", filename)
	if key == "" {
		return s3err.New(http.StatusBadRequest, "InvalidArgument", "The key must not be empty.")
	}
	// conditions are evaluated against what will actually happen: the
	// substituted key, and the bucket the URL targets
	fields["key"] = key
	fields["bucket"] = bucket

	vr, pp, s3e := g.verifyPostRequest(r, fields)
	if s3e != nil {
		return s3e
	}
	if info := recordOf(r.Context()); info != nil {
		info.Tenant, info.User = vr.Tenant, vr.User
		info.AccessKeyID = vr.AccessKeyID
	}

	b, s3e := g.resolveBucket(r.Context(), vr, bucket)
	if s3e != nil {
		return s3e
	}
	client, err := g.backendClient(r.Context(), b.Backend)
	if err != nil {
		return s3err.Internal(err, "backend client failed")
	}
	rt := &bucketRT{cfg: b, client: client, target: t}
	cors.SetHeaders(w, r, b.CORS)

	// the acl form field follows the same rule as the x-amz-acl header on
	// an ACL-disabled bucket
	switch fields["acl"] {
	case "", "private", "bucket-owner-full-control":
	default:
		return errACLNotSupported()
	}
	if s3e := checkPostFields(fields); s3e != nil {
		return s3e
	}
	// validated before the hooks run, so Op.SSE only ever carries a
	// supported value
	if s3e := checkSSE(postFieldHeader(fields)); s3e != nil {
		return s3e
	}

	c := &opCtx{g: g, w: w, r: r, rt: rt, vr: vr, query: r.URL.Query(), key: key}
	if s3e := c.authorize("s3:PutObject"); s3e != nil {
		return s3e
	}
	op := &Op{
		Method:         r.Method,
		Operation:      "PostObject",
		Action:         "s3:PutObject",
		Tenant:         vr.Tenant,
		User:           vr.User,
		Bucket:         b.Name,
		Key:            key,
		Request:        postObjectRequest(fields),
		BucketMetadata: b.Metadata,
		KeyMetadata:    vr.KeyMetadata,
	}
	c.op = op
	if info := recordOf(r.Context()); info != nil {
		info.Op = op
	}
	return g.runOp(r.Context(), op, c, func() error {
		return g.postPutObject(c, fields, file, pp)
	})
}

// postObjectRequest is opCtx.objectRequest for a POST upload, whose values
// arrive as form fields rather than headers. Hooks see the same Op either
// way, which is the point of normalizing here rather than in each hook.
func postObjectRequest(fields map[string]string) *OpRequest {
	req := OpRequest{
		SSE:          fields[hdrSSE],
		SSEKMSKeyID:  fields[hdrSSEKMSKeyID],
		StorageClass: fields[hdrStorageClass],
		Metadata:     postFieldMeta(fields),
	}
	if req.empty() {
		return nil
	}
	return &req
}

// postFieldMeta collects the x-amz-meta-* form fields, prefix removed, the
// way signedHeader.AmzMeta does for headers.
func postFieldMeta(fields map[string]string) map[string]string {
	var md map[string]string
	for name, v := range fields {
		if meta, ok := strings.CutPrefix(name, amzMetaPrefix); ok {
			if md == nil {
				md = make(map[string]string)
			}
			md[meta] = v
		}
	}
	return md
}

// lengthBoundedBody enforces the policy's content-length-range maximum
// while the file streams to the backend; the minimum can only be checked
// once the stream ends.
type lengthBoundedBody struct {
	r   io.Reader
	n   int64
	max int64
}

func (b *lengthBoundedBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	b.n += int64(n)
	if b.n > b.max {
		return n, s3err.New(http.StatusBadRequest, "EntityTooLarge",
			"Your proposed upload exceeds the maximum allowed size")
	}
	return n, err
}

func (g *Gateway) postPutObject(c *opCtx, fields map[string]string, file *multipart.Part, pp *sigv4.PostPolicy) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	body := &lengthBoundedBody{r: file, max: pp.MaxLength}
	in := &s3.PutObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
		Body:   body,
	}
	if v := fields["content-type"]; v != "" {
		in.ContentType = aws.String(v)
	}
	if v := fields["content-md5"]; v != "" {
		in.ContentMD5 = aws.String(v)
	}
	if v := fields["cache-control"]; v != "" {
		in.CacheControl = aws.String(v)
	}
	if v := fields["content-disposition"]; v != "" {
		in.ContentDisposition = aws.String(v)
	}
	if v := fields["content-encoding"]; v != "" {
		in.ContentEncoding = aws.String(v)
	}
	if v := fields["content-language"]; v != "" {
		in.ContentLanguage = aws.String(v)
	}
	if v := fields["expires"]; v != "" {
		if t, err := http.ParseTime(v); err == nil {
			in.Expires = aws.Time(t)
		}
	}
	if v := fields[hdrStorageClass]; v != "" {
		in.StorageClass = types.StorageClass(v)
	}
	if v := fields["x-amz-tagging"]; v != "" {
		in.Tagging = aws.String(v)
	}
	if s3e := applySSE(postFieldHeader(fields), &in.ServerSideEncryption, &in.SSEKMSKeyId); s3e != nil {
		return s3e
	}
	if md := postFieldMeta(fields); len(md) > 0 {
		in.Metadata = md
	}

	out, err := rt.client.PutObject(r.Context(), in)
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	if body.n < pp.MinLength {
		// the size is only known now; undo the write rather than keep an
		// object the policy did not permit. On a versioned bucket the
		// version id targets the exact version just written — a plain
		// delete would only add a delete marker on top of it.
		rt.client.DeleteObject(r.Context(), &s3.DeleteObjectInput{
			Bucket:    aws.String(rt.cfg.Backend.Bucket),
			Key:       aws.String(key),
			VersionId: out.VersionId,
		})
		return s3err.New(http.StatusBadRequest, "EntityTooSmall",
			"Your proposed upload is smaller than the minimum allowed size")
	}

	c.setResponse(OpResponse{
		SSE:         string(out.ServerSideEncryption),
		SSEKMSKeyID: aws.ToString(out.SSEKMSKeyId),
		ETag:        aws.ToString(out.ETag),
		VersionID:   aws.ToString(out.VersionId),
	})

	etag := aws.ToString(out.ETag)
	w.Header().Set("ETag", etag)
	if out.VersionId != nil {
		w.Header().Set("x-amz-version-id", *out.VersionId)
	}
	setSSEHeaders(w.Header(), out.ServerSideEncryption, out.SSEKMSKeyId)
	// the front URL of the object, never the backend's
	location := objectURL(r, rt.target, key)

	if redirect := fields["success_action_redirect"]; redirect != "" {
		if u, err := url.Parse(redirect); err == nil && u.IsAbs() {
			q := u.Query()
			q.Set("bucket", rt.cfg.Name)
			q.Set("key", key)
			q.Set("etag", etag)
			u.RawQuery = q.Encode()
			w.Header().Set("Location", u.String())
			w.WriteHeader(http.StatusSeeOther)
			return nil
		}
		// an unparseable redirect falls back to the status response, as on AWS
	}
	w.Header().Set("Location", location)
	switch fields["success_action_status"] {
	case "200":
		w.WriteHeader(http.StatusOK)
	case "201":
		body, err := xml.Marshal(&s3xml.PostResponse{
			XMLNS:    s3xml.Namespace,
			Location: location, Bucket: rt.cfg.Name, Key: key, ETag: etag,
		})
		if err != nil {
			return s3err.Internal(err, "failed to marshal response")
		}
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(http.StatusCreated)
		w.Write([]byte(xml.Header))
		w.Write(body)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
	return nil
}
