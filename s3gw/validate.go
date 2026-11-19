package s3gw

import (
	"net/http"
	"net/url"
	"regexp"
	"unicode/utf8"

	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"
)

// Request values S3 itself refuses are refused here, before the backend
// sees them, with S3's own code and message. Left to the backend each one
// answers differently — RGW accepts a duplicate tag key and an unknown
// tagging directive, and words its refusals its own way — and the
// backend's wording never reaches the client, so the client would learn
// only "Invalid Argument".

// S3's limits on an object's tag set.
const (
	maxObjectTags     = 10
	maxTagKeyLength   = 128
	maxTagValueLength = 256
)

// tagCharsRegexp is the character class S3 allows in a tag key or value:
// letters, numbers, spaces and + - = . _ : / @. A combining mark or an
// emoji is outside it.
var tagCharsRegexp = regexp.MustCompile(`^[\p{L}\p{Z}\p{N}_.:/=+\-@]*$`)

func errTooManyTags() *s3err.Error {
	return s3err.New(http.StatusBadRequest, "BadRequest", "Object tags cannot be greater than 10")
}

func checkTag(key, value string) *s3err.Error {
	if n := utf8.RuneCountInString(key); n < 1 || n > maxTagKeyLength || !tagCharsRegexp.MatchString(key) {
		return s3err.New(http.StatusBadRequest, "InvalidTag", "The TagKey you have provided is invalid")
	}
	if utf8.RuneCountInString(value) > maxTagValueLength || !tagCharsRegexp.MatchString(value) {
		return s3err.New(http.StatusBadRequest, "InvalidTag", "The TagValue you have provided is invalid")
	}
	return nil
}

// checkTagSet validates the body of a PutObjectTagging.
func checkTagSet(tags []s3xml.Tag) *s3err.Error {
	if len(tags) > maxObjectTags {
		return errTooManyTags()
	}
	seen := make(map[string]bool, len(tags))
	for _, tag := range tags {
		if s3e := checkTag(tag.Key, tag.Value); s3e != nil {
			return s3e
		}
		if seen[tag.Key] {
			return s3err.New(http.StatusBadRequest, "InvalidTag", "Cannot provide multiple Tags with the same key")
		}
		seen[tag.Key] = true
	}
	return nil
}

// checkTaggingValue validates tags given inline with a write, as the
// x-amz-tagging header or the POST form field of that name: URL query
// syntax, each key once.
func checkTaggingValue(v string) *s3err.Error {
	if v == "" {
		return nil
	}
	tags, err := url.ParseQuery(v)
	if err != nil {
		return errTaggingHeader().WithCause(err)
	}
	if len(tags) > maxObjectTags {
		return errTooManyTags()
	}
	for key, values := range tags {
		if len(values) != 1 {
			return errTaggingHeader()
		}
		if s3e := checkTag(key, values[0]); s3e != nil {
			return s3e
		}
	}
	return nil
}

func errTaggingHeader() *s3err.Error {
	return s3err.New(http.StatusBadRequest, "InvalidArgument",
		"The header 'x-amz-tagging' shall be encoded as UTF-8 then URLEncoded URL query parameters without tag name duplicates.")
}

// checkDirective refuses a copy directive other than COPY and REPLACE.
func checkDirective(kind, v string) *s3err.Error {
	switch v {
	case "", "COPY", "REPLACE":
		return nil
	}
	return s3err.New(http.StatusBadRequest, "InvalidArgument", "Unknown "+kind+" directive.")
}
