package s3gw_test

import (
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Keys are opaque: an empty or dot segment is part of the key, not something
// to clean. A ServeMux in front of the handler answered these with a
// redirect to another key.
func TestKeyPathServedAsSent(t *testing.T) {
	for _, key := range []string{"a//b", "a/./b", "a/../b", "a/", "//a"} {
		t.Run(key, func(t *testing.T) {
			stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"etag"`)}}
			client, _ := newTestProxy(t, stub)
			_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
				Bucket: aws.String("testbucket"),
				Key:    aws.String(key),
				Body:   strings.NewReader("x"),
			})
			if err != nil {
				t.Fatal(err)
			}
			if got := aws.ToString(stub.putIn.Key); got != key {
				t.Errorf("backend key %q, want %q", got, key)
			}
		})
	}
}
