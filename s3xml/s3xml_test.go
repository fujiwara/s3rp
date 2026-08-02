package s3xml_test

import (
	"encoding/xml"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/fujiwara/s3rp/s3xml"
	"github.com/google/go-cmp/cmp"
)

// These pin the wire format. An S3 client reads these documents by element
// name, so a renamed or re-nested tag is a break no compiler catches and no
// backend rejects — it simply produces a document the client misreads.

func TestMarshalWireFormat(t *testing.T) {
	modified := time.Date(2026, 3, 1, 12, 30, 45, 123000000, time.UTC)

	cases := []struct {
		name  string
		value any
		want  string
	}{
		{
			name: "ListBucketResult",
			value: &s3xml.ListBucketResult{
				XMLNS: s3xml.Namespace,
				Name:  "photos", Prefix: "dir/", KeyCount: 1, MaxKeys: 1000,
				Delimiter: "/", IsTruncated: false,
				Contents: []s3xml.Object{{
					Key: "dir/a.txt", LastModified: s3xml.FormatTime(modified),
					ETag: `"abc"`, Size: 12, StorageClass: "STANDARD",
				}},
				CommonPrefixes: []s3xml.CommonPrefix{{Prefix: "dir/sub/"}},
			},
			want: `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
				`<Name>photos</Name><Prefix>dir/</Prefix><KeyCount>1</KeyCount><MaxKeys>1000</MaxKeys>` +
				`<Delimiter>/</Delimiter><IsTruncated>false</IsTruncated>` +
				`<Contents><Key>dir/a.txt</Key><LastModified>2026-03-01T12:30:45.123Z</LastModified>` +
				`<ETag>&#34;abc&#34;</ETag><Size>12</Size><StorageClass>STANDARD</StorageClass></Contents>` +
				`<CommonPrefixes><Prefix>dir/sub/</Prefix></CommonPrefixes></ListBucketResult>`,
		},
		{
			name: "DeleteResult",
			value: &s3xml.DeleteResult{
				XMLNS:   s3xml.Namespace,
				Deleted: []s3xml.DeletedObject{{Key: "gone.txt"}},
				Errors:  []s3xml.DeleteError{{Key: "kept.txt", Code: "AccessDenied", Message: "Access Denied"}},
			},
			want: `<DeleteResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
				`<Deleted><Key>gone.txt</Key></Deleted>` +
				`<Error><Key>kept.txt</Key><Code>AccessDenied</Code><Message>Access Denied</Message></Error>` +
				`</DeleteResult>`,
		},
		{
			name: "CopyObjectResult",
			value: &s3xml.CopyObjectResult{
				XMLNS: s3xml.Namespace, ETag: `"copy"`, LastModified: s3xml.FormatTime(modified),
			},
			want: `<CopyObjectResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
				`<ETag>&#34;copy&#34;</ETag><LastModified>2026-03-01T12:30:45.123Z</LastModified>` +
				`</CopyObjectResult>`,
		},
		{
			name: "InitiateMultipartUploadResult",
			value: &s3xml.InitiateMultipartUploadResult{
				XMLNS: s3xml.Namespace, Bucket: "photos", Key: "big.bin", UploadID: "upload-1",
			},
			want: `<InitiateMultipartUploadResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
				`<Bucket>photos</Bucket><Key>big.bin</Key><UploadId>upload-1</UploadId>` +
				`</InitiateMultipartUploadResult>`,
		},
		{
			name: "Tagging",
			value: &s3xml.Tagging{
				XMLNS:  s3xml.Namespace,
				TagSet: s3xml.TagSet{Tags: []s3xml.Tag{{Key: "env", Value: "prod"}}},
			},
			want: `<Tagging xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
				`<TagSet><Tag><Key>env</Key><Value>prod</Value></Tag></TagSet></Tagging>`,
		},
		{
			name: "LocationConstraint",
			value: &s3xml.LocationConstraint{
				XMLNS: s3xml.Namespace, Value: "ap-northeast-1",
			},
			want: `<LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
				`ap-northeast-1</LocationConstraint>`,
		},
		{
			name: "ListAllMyBucketsResult",
			value: func() any {
				r := &s3xml.ListAllMyBucketsResult{
					XMLNS: s3xml.Namespace,
					Owner: s3xml.Owner{ID: "acme", DisplayName: "acme"},
				}
				r.Buckets.Bucket = []s3xml.BucketEntry{{
					Name: "photos", CreationDate: s3xml.FormatTime(modified),
				}}
				return r
			}(),
			want: `<ListAllMyBucketsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
				`<Owner><ID>acme</ID><DisplayName>acme</DisplayName></Owner>` +
				`<Buckets><Bucket><Name>photos</Name>` +
				`<CreationDate>2026-03-01T12:30:45.123Z</CreationDate></Bucket></Buckets>` +
				`</ListAllMyBucketsResult>`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b, err := xml.Marshal(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if diff := cmp.Diff(tc.want, string(b)); diff != "" {
				t.Errorf("wire format changed (-want +got):\n%s", diff)
			}
		})
	}
}

// The request bodies are what clients send, so they must unmarshal from the
// documents the AWS SDK produces.
func TestUnmarshalRequestBodies(t *testing.T) {
	t.Run("Delete", func(t *testing.T) {
		const body = `<Delete xmlns="http://s3.amazonaws.com/doc/2006-03-01/">` +
			`<Quiet>true</Quiet>` +
			`<Object><Key>a.txt</Key></Object>` +
			`<Object><Key>b.txt</Key><VersionId>v2</VersionId></Object>` +
			`</Delete>`
		var got s3xml.DeleteRequest
		if err := xml.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if !got.Quiet {
			t.Error("expect quiet mode")
		}
		want := []s3xml.DeleteRequestObject{{Key: "a.txt"}, {Key: "b.txt", VersionID: "v2"}}
		if diff := cmp.Diff(want, got.Objects); diff != "" {
			t.Errorf("unexpected objects (-want +got):\n%s", diff)
		}
	})

	t.Run("CompleteMultipartUpload", func(t *testing.T) {
		const body = `<CompleteMultipartUpload>` +
			`<Part><PartNumber>1</PartNumber><ETag>"one"</ETag></Part>` +
			`<Part><PartNumber>2</PartNumber><ETag>"two"</ETag></Part>` +
			`</CompleteMultipartUpload>`
		var got s3xml.CompleteMultipartUploadRequest
		if err := xml.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Parts) != 2 || got.Parts[0].PartNumber != 1 || got.Parts[1].ETag != `"two"` {
			t.Errorf("unexpected parts %+v", got.Parts)
		}
	})

	t.Run("Tagging", func(t *testing.T) {
		const body = `<Tagging><TagSet><Tag><Key>env</Key><Value>prod</Value></Tag></TagSet></Tagging>`
		var got s3xml.Tagging
		if err := xml.Unmarshal([]byte(body), &got); err != nil {
			t.Fatal(err)
		}
		want := []s3xml.Tag{{Key: "env", Value: "prod"}}
		if diff := cmp.Diff(want, got.TagSet.Tags); diff != "" {
			t.Errorf("unexpected tags (-want +got):\n%s", diff)
		}
	})
}

// FormatTime is the S3 timestamp format: UTC, milliseconds, trailing Z.
func TestFormatTime(t *testing.T) {
	// a non-UTC input must still render as UTC
	jst := time.FixedZone("JST", 9*60*60)
	got := s3xml.FormatTime(time.Date(2026, 3, 1, 21, 30, 45, 123456789, jst))
	if want := "2026-03-01T12:30:45.123Z"; got != want {
		t.Errorf("expect %q, got %q", want, got)
	}
}

func TestWrite(t *testing.T) {
	w := httptest.NewRecorder()
	err := s3xml.Write(w, &s3xml.LocationConstraint{XMLNS: s3xml.Namespace, Value: "us-west-2"})
	if err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 {
		t.Errorf("unexpected status %d", w.Code)
	}
	if got := w.Header().Get("Content-Type"); got != "application/xml" {
		t.Errorf("unexpected content type %q", got)
	}
	body := w.Body.String()
	// the declaration must come first: some clients reject a body without it
	if !strings.HasPrefix(body, xml.Header) {
		t.Errorf("expect an XML declaration, got %q", body)
	}
	if !strings.Contains(body, "<Location>us-west-2</Location>") &&
		!strings.Contains(body, ">us-west-2<") {
		t.Errorf("unexpected body %q", body)
	}
}
