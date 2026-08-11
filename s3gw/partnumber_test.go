package s3gw_test

import (
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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
}
