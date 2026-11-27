package s3gw

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"
	"github.com/fujiwara/s3rp/store"
)

// Request values S3 itself refuses are refused here, before the backend
// sees them, with S3's own code and message. Left to the backend each one
// answers differently — RGW accepts a duplicate tag key and an unknown
// tagging directive, and words its refusals its own way — and the
// backend's wording never reaches the client, so the client would learn
// only "Invalid Argument".

// S3's limit on an object's tag set. What one tag may be is the store
// package's (ValidTagKey, ValidTagValue), shared with a bucket's tags.
const maxObjectTags = 10

func errTooManyTags() *s3err.Error {
	return s3err.New(http.StatusBadRequest, "BadRequest", "Object tags cannot be greater than 10")
}

func checkTag(key, value string) *s3err.Error {
	if !store.ValidTagKey(key) {
		return s3err.New(http.StatusBadRequest, "InvalidTag", "The TagKey you have provided is invalid")
	}
	if !store.ValidTagValue(value) {
		return s3err.New(http.StatusBadRequest, "InvalidTag", "The TagValue you have provided is invalid")
	}
	return nil
}

// checkTagSet validates the body of a PutObjectTagging and returns its
// tags, nil for an empty set. A key appears once, so the map loses nothing.
func checkTagSet(tags []s3xml.Tag) (map[string]string, *s3err.Error) {
	if len(tags) > maxObjectTags {
		return nil, errTooManyTags()
	}
	var set map[string]string
	for _, tag := range tags {
		if s3e := checkTag(tag.Key, tag.Value); s3e != nil {
			return nil, s3e
		}
		if _, dup := set[tag.Key]; dup {
			return nil, s3err.New(http.StatusBadRequest, "InvalidTag", "Cannot provide multiple Tags with the same key")
		}
		if set == nil {
			set = make(map[string]string, len(tags))
		}
		set[tag.Key] = tag.Value
	}
	return set, nil
}

// checkTaggingValue validates tags given inline with a write, as the
// x-amz-tagging header or the POST form field of that name: URL query
// syntax, each key once. It returns the tags, nil when there are none.
func checkTaggingValue(v string) (map[string]string, *s3err.Error) {
	if v == "" {
		return nil, nil
	}
	tags, err := url.ParseQuery(v)
	if err != nil {
		return nil, errTaggingHeader().WithCause(err)
	}
	if len(tags) > maxObjectTags {
		return nil, errTooManyTags()
	}
	var set map[string]string
	for key, values := range tags {
		if len(values) != 1 {
			return nil, errTaggingHeader()
		}
		if s3e := checkTag(key, values[0]); s3e != nil {
			return nil, s3e
		}
		if set == nil {
			set = make(map[string]string, len(tags))
		}
		set[key] = values[0]
	}
	return set, nil
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

// checkAnnotationDirective accepts the one object-annotation directive a
// gateway without annotations honors exactly: EXCLUDE, which aws-cli sends
// on every `aws s3 cp` between buckets. COPY promises the source's
// annotations on the destination, so it is refused, as S3 refuses it on a
// directory bucket.
func checkAnnotationDirective(v string) *s3err.Error {
	switch v {
	case "", "EXCLUDE":
		return nil
	}
	return s3err.NotImplemented(hdrAnnotationDirective + ": " + v)
}

// optionalObjectAttributes reads what a listing is asked to add to each
// entry. RestoreStatus is the one attribute S3 defines and the one the
// listings relay from the backend; another name asks for an element the
// gateway would not pass on, so it is refused.
func (c *opCtx) optionalObjectAttributes() ([]types.OptionalObjectAttributes, error) {
	var attrs []types.OptionalObjectAttributes
	// one header line per attribute (the AWS SDK for Go) or one
	// comma-separated line
	for _, line := range c.hdr.SignedValues(hdrOptionalObjectAttributes) {
		for name := range strings.SplitSeq(line, ",") {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			if name != string(types.OptionalObjectAttributesRestoreStatus) {
				return nil, s3err.NotImplemented(hdrOptionalObjectAttributes + ": " + name)
			}
			if len(attrs) == 0 {
				attrs = append(attrs, types.OptionalObjectAttributesRestoreStatus)
			}
		}
	}
	return attrs, nil
}

// restoreStatusFromSDK converts a listing entry's restore status, nil when
// the backend reported none or the request did not ask (asked = the
// request's optionalObjectAttributes): a backend that reports it unasked
// must not make the listing differ from S3's.
func restoreStatusFromSDK(rs *types.RestoreStatus, asked []types.OptionalObjectAttributes) *s3xml.RestoreStatus {
	if rs == nil || len(asked) == 0 {
		return nil
	}
	out := &s3xml.RestoreStatus{IsRestoreInProgress: aws.ToBool(rs.IsRestoreInProgress)}
	if rs.RestoreExpiryDate != nil {
		out.RestoreExpiryDate = s3xml.FormatTime(*rs.RestoreExpiryDate)
	}
	return out
}
