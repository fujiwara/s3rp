package s3gw_test

import (
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsmiddleware "github.com/aws/aws-sdk-go-v2/aws/middleware"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/middleware"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

// GetObject/HeadObject with partNumber read a single part of a multipart
// object; the response carries x-amz-mp-parts-count and 206 semantics.
func TestGetObjectPartNumber(t *testing.T) {
	stub := &stubBackend{
		getOut: &s3.GetObjectOutput{
			Body:          io.NopCloser(strings.NewReader("part-one")),
			ContentLength: aws.Int64(8),
			ContentRange:  aws.String("bytes 0-7/16"),
			PartsCount:    aws.Int32(2),
			ETag:          aws.String(`"e"`),
		},
		headOut: &s3.HeadObjectOutput{
			ContentLength: aws.Int64(8),
			ContentRange:  aws.String("bytes 8-15/16"),
			PartsCount:    aws.Int32(2),
			ETag:          aws.String(`"e"`),
		},
	}
	client, _ := newTestProxy(t, stub)

	out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket:     aws.String("testbucket"),
		Key:        aws.String("k"),
		PartNumber: aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	if aws.ToInt32(stub.getIn.PartNumber) != 1 {
		t.Errorf("partNumber not forwarded: %+v", stub.getIn.PartNumber)
	}
	if aws.ToInt32(out.PartsCount) != 2 {
		t.Errorf("x-amz-mp-parts-count not returned: %+v", out.PartsCount)
	}
	if aws.ToString(out.ContentRange) != "bytes 0-7/16" {
		t.Errorf("Content-Range not returned: %+v", out.ContentRange)
	}

	hout, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket:     aws.String("testbucket"),
		Key:        aws.String("k"),
		PartNumber: aws.Int32(2),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToInt32(stub.headIn.PartNumber) != 2 {
		t.Errorf("HEAD partNumber not forwarded: %+v", stub.headIn.PartNumber)
	}
	if aws.ToInt32(hout.PartsCount) != 2 {
		t.Errorf("HEAD x-amz-mp-parts-count not returned: %+v", hout.PartsCount)
	}
	// the HEAD of a part says which bytes it is, as its GET does
	if aws.ToString(hout.ContentRange) != "bytes 8-15/16" {
		t.Errorf("HEAD Content-Range not returned: %+v", hout.ContentRange)
	}
	if status := httpStatusOf(hout.ResultMetadata); status != http.StatusPartialContent {
		t.Errorf("HEAD of a part answered %d, want 206", status)
	}
}

// A HEAD of the whole object stays a plain 200 without Content-Range.
func TestHeadObjectWholeIsNotPartial(t *testing.T) {
	stub := &stubBackend{headOut: &s3.HeadObjectOutput{ContentLength: aws.Int64(16), ETag: aws.String(`"e"`)}}
	client, _ := newTestProxy(t, stub)
	hout, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String("testbucket"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	if hout.ContentRange != nil || httpStatusOf(hout.ResultMetadata) != http.StatusOK {
		t.Errorf("whole-object HEAD: Content-Range %v, status %d", hout.ContentRange, httpStatusOf(hout.ResultMetadata))
	}
}

func httpStatusOf(md middleware.Metadata) int {
	if resp, ok := awsmiddleware.GetRawResponse(md).(*smithyhttp.Response); ok {
		return resp.StatusCode
	}
	return 0
}

func TestGetObjectPartNumberOutOfRange(t *testing.T) {
	stub := &stubBackend{}
	client, _ := newTestProxy(t, stub)
	for _, n := range []int32{0, 10001} {
		_, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket:     aws.String("testbucket"),
			Key:        aws.String("k"),
			PartNumber: aws.Int32(n),
		})
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InvalidArgument" {
			t.Errorf("partNumber=%d: expected InvalidArgument, got %v", n, err)
		}
	}
	if stub.getIn != nil {
		t.Error("an out-of-range part number must not reach the backend")
	}
}

// The tag count is reported on both reads: a client learns from
// x-amz-tagging-count that an object is tagged without s3:GetObjectTagging.
func TestObjectTagCount(t *testing.T) {
	stub := &stubBackend{
		getOut: &s3.GetObjectOutput{
			Body:          io.NopCloser(strings.NewReader("x")),
			ContentLength: aws.Int64(1),
			TagCount:      aws.Int32(3),
		},
		headOut: &s3.HeadObjectOutput{ContentLength: aws.Int64(1), TagCount: aws.Int32(3)},
	}
	client, _ := newTestProxy(t, stub)

	out, err := client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("testbucket"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	if aws.ToInt32(out.TagCount) != 3 {
		t.Errorf("GetObject TagCount %v, want 3", out.TagCount)
	}
	hout, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String("testbucket"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToInt32(hout.TagCount) != 3 {
		t.Errorf("HeadObject TagCount %v, want 3", hout.TagCount)
	}
}
