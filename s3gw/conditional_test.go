package s3gw_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

// Write preconditions must reach the backend: a dropped If-Match makes the
// write unconditional while the client believes it is protected.

func TestPutObjectForwardsPreconditions(t *testing.T) {
	stub := &stubBackend{putOut: &s3.PutObjectOutput{}}
	client, _ := newTestProxy(t, stub)

	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket:      aws.String("testbucket"),
		Key:         aws.String("k"),
		Body:        strings.NewReader("body"),
		IfMatch:     aws.String(`"etag"`),
		IfNoneMatch: aws.String("*"),
	}); err != nil {
		t.Fatal(err)
	}
	if v := aws.ToString(stub.putIn.IfMatch); v != `"etag"` {
		t.Errorf("IfMatch not forwarded: %q", v)
	}
	if v := aws.ToString(stub.putIn.IfNoneMatch); v != "*" {
		t.Errorf("IfNoneMatch not forwarded: %q", v)
	}
}

func TestDeleteObjectForwardsPreconditions(t *testing.T) {
	stub := &stubBackend{delOut: &s3.DeleteObjectOutput{}}
	client, _ := newTestProxy(t, stub)

	lm := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	if _, err := client.DeleteObject(t.Context(), &s3.DeleteObjectInput{
		Bucket:                  aws.String("testbucket"),
		Key:                     aws.String("k"),
		IfMatch:                 aws.String(`"etag"`),
		IfMatchLastModifiedTime: aws.Time(lm),
		IfMatchSize:             aws.Int64(42),
	}); err != nil {
		t.Fatal(err)
	}
	if v := aws.ToString(stub.delIn.IfMatch); v != `"etag"` {
		t.Errorf("IfMatch not forwarded: %q", v)
	}
	if got := aws.ToTime(stub.delIn.IfMatchLastModifiedTime); !got.Equal(lm) {
		t.Errorf("IfMatchLastModifiedTime not forwarded: %v", got)
	}
	if got := aws.ToInt64(stub.delIn.IfMatchSize); got != 42 {
		t.Errorf("IfMatchSize not forwarded: %d", got)
	}
}

func TestDeleteObjectsForwardsPreconditions(t *testing.T) {
	stub := &stubBackend{delObjsOut: &s3.DeleteObjectsOutput{}}
	client, _ := newTestProxy(t, stub)

	lm := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	if _, err := client.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
		Bucket: aws.String("testbucket"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{
				Key:              aws.String("k"),
				ETag:             aws.String(`"etag"`),
				LastModifiedTime: aws.Time(lm),
				Size:             aws.Int64(42),
			},
			{Key: aws.String("unconditional")},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	objs := stub.delObjsIn.Delete.Objects
	if len(objs) != 2 {
		t.Fatalf("expected 2 objects, got %d", len(objs))
	}
	if v := aws.ToString(objs[0].ETag); v != `"etag"` {
		t.Errorf("ETag not forwarded: %q", v)
	}
	if got := aws.ToTime(objs[0].LastModifiedTime); !got.Equal(lm) {
		t.Errorf("LastModifiedTime not forwarded: %v", got)
	}
	if got := aws.ToInt64(objs[0].Size); got != 42 {
		t.Errorf("Size not forwarded: %d", got)
	}
	if objs[1].ETag != nil || objs[1].LastModifiedTime != nil || objs[1].Size != nil {
		t.Errorf("unconditional object grew preconditions: %+v", objs[1])
	}
}

func TestCompleteMultipartUploadForwardsPreconditions(t *testing.T) {
	stub := &stubBackend{completeMPUOut: &s3.CompleteMultipartUploadOutput{ETag: aws.String(`"e"`)}}
	client, _ := newTestProxy(t, stub)

	if _, err := client.CompleteMultipartUpload(t.Context(), &s3.CompleteMultipartUploadInput{
		Bucket:   aws.String("testbucket"),
		Key:      aws.String("k"),
		UploadId: aws.String("upload-1"),
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: aws.String(`"p1"`)},
		}},
		IfMatch:     aws.String(`"etag"`),
		IfNoneMatch: aws.String("*"),
	}); err != nil {
		t.Fatal(err)
	}
	if v := aws.ToString(stub.completeMPUIn.IfMatch); v != `"etag"` {
		t.Errorf("IfMatch not forwarded: %q", v)
	}
	if v := aws.ToString(stub.completeMPUIn.IfNoneMatch); v != "*" {
		t.Errorf("IfNoneMatch not forwarded: %q", v)
	}
}
