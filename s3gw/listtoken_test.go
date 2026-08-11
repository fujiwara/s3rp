package s3gw_test

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// ListObjectsV2 echoes the request's continuation-token — including a
// supplied-but-empty one, which S3 answers with an empty element.
func TestListObjectsV2EchoesContinuationToken(t *testing.T) {
	t.Run("token echoed", func(t *testing.T) {
		stub := &stubBackend{listOut: &s3.ListObjectsV2Output{
			ContinuationToken: aws.String("tok-1"),
		}}
		client, _ := newTestProxy(t, stub)
		out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
			Bucket:            aws.String("testbucket"),
			ContinuationToken: aws.String("tok-1"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := aws.ToString(out.ContinuationToken); got != "tok-1" {
			t.Errorf("token not echoed: %q", got)
		}
	})

	t.Run("empty token echoed as empty", func(t *testing.T) {
		stub := &stubBackend{listOut: &s3.ListObjectsV2Output{}}
		client, _ := newTestProxy(t, stub)
		out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
			Bucket:            aws.String("testbucket"),
			ContinuationToken: aws.String(""),
		})
		if err != nil {
			t.Fatal(err)
		}
		if out.ContinuationToken == nil {
			t.Error("empty token must still be echoed")
		}
		// the empty token is not forwarded to the backend
		if stub.listIn.ContinuationToken != nil {
			t.Errorf("empty token forwarded to backend: %q", *stub.listIn.ContinuationToken)
		}
	})
}
