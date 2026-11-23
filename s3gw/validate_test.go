package s3gw_test

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// Values S3 itself refuses are refused by the gateway with S3's code and
// message, and never reach the backend: backends differ on each of them
// and their wording is not relayed. The expectations are what Amazon S3
// answers (asserted by aws-emulator-evaluation-suite's S3 tests), except
// the tag-key and metadata-directive messages, which mirror their verified
// counterparts.
func TestRequestValuesValidated(t *testing.T) {
	inlineTags := func(n int) *string {
		v := url.Values{}
		for i := range n {
			v.Set(fmt.Sprintf("k%d", i), "v")
		}
		return aws.String(v.Encode())
	}
	tagSet := func(pairs ...string) *types.Tagging {
		var tags []types.Tag
		for i := 0; i < len(pairs); i += 2 {
			tags = append(tags, types.Tag{Key: aws.String(pairs[i]), Value: aws.String(pairs[i+1])})
		}
		return &types.Tagging{TagSet: tags}
	}
	manyTags := func(n int) *types.Tagging {
		var pairs []string
		for i := range n {
			pairs = append(pairs, fmt.Sprintf("k%d", i), "v")
		}
		return tagSet(pairs...)
	}
	put := func(mod func(*s3.PutObjectInput)) func(*testing.T, *s3.Client) error {
		return func(t *testing.T, c *s3.Client) error {
			in := &s3.PutObjectInput{Bucket: aws.String("testbucket"), Key: aws.String("k"), Body: strings.NewReader("x")}
			mod(in)
			_, err := c.PutObject(t.Context(), in)
			return err
		}
	}
	putTagging := func(tagging *types.Tagging) func(*testing.T, *s3.Client) error {
		return func(t *testing.T, c *s3.Client) error {
			_, err := c.PutObjectTagging(t.Context(), &s3.PutObjectTaggingInput{
				Bucket: aws.String("testbucket"), Key: aws.String("k"), Tagging: tagging,
			})
			return err
		}
	}
	cases := []struct {
		name    string
		call    func(*testing.T, *s3.Client) error
		code    string // "" = accepted
		message string
	}{
		{name: "ten inline tags", call: put(func(in *s3.PutObjectInput) { in.Tagging = inlineTags(10) })},
		{name: "eleven inline tags", call: put(func(in *s3.PutObjectInput) { in.Tagging = inlineTags(11) }),
			code: "BadRequest", message: "Object tags cannot be greater than 10"},
		{name: "inline tag value at the limit", call: put(func(in *s3.PutObjectInput) { in.Tagging = aws.String("env=" + strings.Repeat("a", 256)) })},
		{name: "inline tag value over the limit", call: put(func(in *s3.PutObjectInput) { in.Tagging = aws.String("env=" + strings.Repeat("a", 257)) }),
			code: "InvalidTag", message: "The TagValue you have provided is invalid"},
		{name: "inline tag key over the limit", call: put(func(in *s3.PutObjectInput) { in.Tagging = aws.String(strings.Repeat("k", 129) + "=v") }),
			code: "InvalidTag", message: "The TagKey you have provided is invalid"},
		{name: "inline duplicate tag key", call: put(func(in *s3.PutObjectInput) { in.Tagging = aws.String("a=1&a=2") }),
			code: "InvalidArgument", message: "The header 'x-amz-tagging' shall be encoded as UTF-8 then URLEncoded URL query parameters without tag name duplicates."},
		{name: "tag set with letters of any script", call: putTagging(tagSet("名前", "café 東京 +-=._:/@"))},
		{name: "eleven tags", call: putTagging(manyTags(11)),
			code: "BadRequest", message: "Object tags cannot be greater than 10"},
		{name: "duplicate tag key", call: putTagging(tagSet("dup", "one", "dup", "two")),
			code: "InvalidTag", message: "Cannot provide multiple Tags with the same key"},
		{name: "emoji tag value", call: putTagging(tagSet("k", "😀")),
			code: "InvalidTag", message: "The TagValue you have provided is invalid"},
		{name: "combining mark in a tag value", call: putTagging(tagSet("k", "é")),
			code: "InvalidTag", message: "The TagValue you have provided is invalid"},
		// storage class names are the backend's and the service's to define
		{name: "any storage class name", call: put(func(in *s3.PutObjectInput) { in.StorageClass = types.StorageClassExpressOnezone })},
		{name: "unknown object attribute", call: func(t *testing.T, c *s3.Client) error {
			_, err := c.GetObjectAttributes(t.Context(), &s3.GetObjectAttributesInput{
				Bucket: aws.String("testbucket"), Key: aws.String("k"),
				ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesEtag, "NotARealAttribute"},
			})
			return err
		}, code: "InvalidArgument", message: "Invalid attribute name specified."},
		{name: "unknown tagging directive", call: func(t *testing.T, c *s3.Client) error {
			_, err := c.CopyObject(t.Context(), &s3.CopyObjectInput{
				Bucket: aws.String("testbucket"), Key: aws.String("dst"), CopySource: aws.String("testbucket/src"),
				TaggingDirective: "MERGE",
			})
			return err
		}, code: "InvalidArgument", message: "Unknown tagging directive."},
		{name: "unknown metadata directive", call: func(t *testing.T, c *s3.Client) error {
			_, err := c.CopyObject(t.Context(), &s3.CopyObjectInput{
				Bucket: aws.String("testbucket"), Key: aws.String("dst"), CopySource: aws.String("testbucket/src"),
				MetadataDirective: "MERGE",
			})
			return err
		}, code: "InvalidArgument", message: "Unknown metadata directive."},
		{name: "annotations excluded from a copy", call: func(t *testing.T, c *s3.Client) error {
			_, err := c.CopyObject(t.Context(), &s3.CopyObjectInput{
				Bucket: aws.String("testbucket"), Key: aws.String("dst"), CopySource: aws.String("testbucket/src"),
				AnnotationDirective: types.AnnotationDirectiveExclude,
			})
			return err
		}},
		{name: "annotations copied", call: func(t *testing.T, c *s3.Client) error {
			_, err := c.CopyObject(t.Context(), &s3.CopyObjectInput{
				Bucket: aws.String("testbucket"), Key: aws.String("dst"), CopySource: aws.String("testbucket/src"),
				AnnotationDirective: types.AnnotationDirectiveCopy,
			})
			return err
		}, code: "NotImplemented", message: "x-amz-object-annotation-directive: COPY is not implemented"},
		// a key is Unicode; Amazon S3 itself answered these so
		{name: "key in any script", call: put(func(in *s3.PutObjectInput) { in.Key = aws.String("写真/été.jpg") })},
		{name: "key that is not UTF-8", call: put(func(in *s3.PutObjectInput) { in.Key = aws.String("k\xff") }),
			code: "InvalidURI", message: "Couldn't parse the specified URI."},
		{name: "key cut inside a character", call: put(func(in *s3.PutObjectInput) { in.Key = aws.String("dir/k\xc3") }),
			code: "InvalidURI", message: "Couldn't parse the specified URI."},
		{name: "key that is a surrogate", call: put(func(in *s3.PutObjectInput) { in.Key = aws.String("k\xed\xa0\x80") }),
			code: "InvalidURI", message: "Couldn't parse the specified URI."},
		// 1024 bytes of UTF-8, not characters; Amazon S3 itself answered so
		{name: "key at the limit", call: put(func(in *s3.PutObjectInput) { in.Key = aws.String(strings.Repeat("a", 1024)) })},
		{name: "key over the limit", call: put(func(in *s3.PutObjectInput) { in.Key = aws.String(strings.Repeat("a", 1025)) }),
			code: "KeyTooLongError", message: "Your key is too long"},
		{name: "key at the limit in two-byte characters", call: put(func(in *s3.PutObjectInput) { in.Key = aws.String(strings.Repeat("é", 512)) })},
		{name: "key over the limit in two-byte characters", call: put(func(in *s3.PutObjectInput) { in.Key = aws.String(strings.Repeat("é", 513)) }),
			code: "KeyTooLongError", message: "Your key is too long"},
		{name: "copy source over the limit", call: func(t *testing.T, c *s3.Client) error {
			_, err := c.CopyObject(t.Context(), &s3.CopyObjectInput{
				Bucket: aws.String("testbucket"), Key: aws.String("dst"), CopySource: aws.String("testbucket/" + strings.Repeat("a", 1025)),
			})
			return err
		}, code: "KeyTooLongError", message: "Your key is too long"},
		{name: "copy source that is not UTF-8", call: func(t *testing.T, c *s3.Client) error {
			_, err := c.CopyObject(t.Context(), &s3.CopyObjectInput{
				Bucket: aws.String("testbucket"), Key: aws.String("dst"), CopySource: aws.String("testbucket/k%FF"),
			})
			return err
		}, code: "InvalidArgument", message: "Invalid copy source"},
		{name: "parts out of order", call: func(t *testing.T, c *s3.Client) error {
			_, err := c.CompleteMultipartUpload(t.Context(), &s3.CompleteMultipartUploadInput{
				Bucket: aws.String("testbucket"), Key: aws.String("k"), UploadId: aws.String("u"),
				MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
					{PartNumber: aws.Int32(2), ETag: aws.String(`"b"`)},
					{PartNumber: aws.Int32(1), ETag: aws.String(`"a"`)},
				}},
			})
			return err
		}, code: "InvalidPartOrder", message: "The list of parts was not in ascending order. Parts must be ordered by part number."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubBackend{putOut: &s3.PutObjectOutput{}, copyOut: &s3.CopyObjectOutput{CopyObjectResult: &types.CopyObjectResult{}}}
			client, _ := newTestProxy(t, stub)
			err := tc.call(t, client)
			reached := stub.putIn != nil || stub.putTagIn != nil || stub.getAttrIn != nil || stub.copyIn != nil || stub.completeMPUIn != nil
			if tc.code == "" {
				if err != nil {
					t.Fatalf("expect success, got %v", err)
				}
				if !reached {
					t.Error("an accepted request must reach the backend")
				}
				return
			}
			var apiErr smithy.APIError
			if !errors.As(err, &apiErr) {
				t.Fatalf("expect %s, got %v", tc.code, err)
			}
			if apiErr.ErrorCode() != tc.code || apiErr.ErrorMessage() != tc.message {
				t.Errorf("got (%s, %q), want (%s, %q)", apiErr.ErrorCode(), apiErr.ErrorMessage(), tc.code, tc.message)
			}
			if reached {
				t.Error("a refused request must not reach the backend")
			}
		})
	}
}

// The key of a POST upload is a form field, and held to what a key is.
func TestPostKeyChecked(t *testing.T) {
	for _, tc := range []struct{ name, key, code string }{
		{"not UTF-8", "k\xff", "InvalidArgument"},
		{"over the limit", strings.Repeat("a", 1025), "KeyTooLongError"},
		// ${filename} is part of the key by the time it is one
		{"over the limit once the file name is in it", strings.Repeat("a", 1020) + "${filename}", "KeyTooLongError"},
		{"at the limit", strings.Repeat("a", 1024), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gw := newTestGateway(t)
			stub := &stubPost{}
			if err := gw.SetBackend("testbucket", stub); err != nil {
				t.Fatal(err)
			}
			form := &postForm{
				conditions: []string{`["starts-with", "$key", ""]`},
				fields:     [][2]string{{"key", tc.key}},
				filename:   "a.txt",
				content:    "hello post",
			}
			w := httptest.NewRecorder()
			gw.Handler().ServeHTTP(w, form.request(t))
			if tc.code == "" {
				if w.Code != http.StatusNoContent || stub.putIn == nil {
					t.Errorf("status %d: %s", w.Code, w.Body.String())
				}
				return
			}
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "<Code>"+tc.code+"</Code>") {
				t.Errorf("status %d: %s", w.Code, w.Body.String())
			}
			if stub.putIn != nil {
				t.Error("the backend was reached")
			}
		})
	}
}

// Nor does a key in a DeleteObjects body pass: XML that is not UTF-8 is not
// XML.
func TestDeleteObjectsKeyMustBeUTF8(t *testing.T) {
	gw := newTestGateway(t)
	stub := &stubBackend{}
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	body := []byte("<Delete><Object><Key>k\xff</Key></Object></Delete>")
	w := httptest.NewRecorder()
	gw.Handler().ServeHTTP(w, signedRequest(t, "POST", "http://s3.example.com/testbucket?delete",
		body, "UNSIGNED-PAYLOAD", time.Now(), testCreds(), nil))
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "<Code>MalformedXML</Code>") {
		t.Errorf("status %d: %s", w.Code, w.Body.String())
	}
	if stub.delObjsIn != nil {
		t.Error("the backend was reached")
	}
}
