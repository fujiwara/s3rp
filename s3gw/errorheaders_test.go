package s3gw_test

import (
	"errors"
	"net/http"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	smithyhttp "github.com/aws/smithy-go/transport/http"
)

func backendError(status int, header http.Header) error {
	return &awshttp.ResponseError{
		ResponseError: &smithyhttp.ResponseError{
			Response: &smithyhttp.Response{Response: &http.Response{
				StatusCode: status,
				Header:     header,
			}},
			Err: errors.New("backend refused"),
		},
	}
}

// Entity and informational headers on a backend error response — the ETag
// of a 304, the delete marker of a 404 — describe the tenant's own object
// and must reach the client.
func TestErrorResponsesRelayEntityHeaders(t *testing.T) {
	t.Run("304 carries ETag and Last-Modified", func(t *testing.T) {
		stub := &stubBackend{getErr: backendError(http.StatusNotModified, http.Header{
			"Etag":          []string{`"etag-304"`},
			"Last-Modified": []string{"Tue, 11 Aug 2026 00:00:00 GMT"},
		})}
		client, _ := newTestProxy(t, stub)

		_, err := client.GetObject(t.Context(), &s3.GetObjectInput{
			Bucket:      aws.String("testbucket"),
			Key:         aws.String("k"),
			IfNoneMatch: aws.String(`"etag-304"`),
		})
		var respErr *awshttp.ResponseError
		if !errors.As(err, &respErr) {
			t.Fatalf("expected a response error, got %v", err)
		}
		if respErr.HTTPStatusCode() != http.StatusNotModified {
			t.Fatalf("expected 304, got %d", respErr.HTTPStatusCode())
		}
		h := respErr.Response.Header
		if got := h.Get("ETag"); got != `"etag-304"` {
			t.Errorf("ETag not relayed on 304: %q", got)
		}
		if got := h.Get("Last-Modified"); got == "" {
			t.Error("Last-Modified not relayed on 304")
		}
	})

	t.Run("404 carries the delete marker", func(t *testing.T) {
		stub := &stubBackend{headErr: backendError(http.StatusNotFound, http.Header{
			"X-Amz-Delete-Marker": []string{"true"},
			"X-Amz-Version-Id":    []string{"v1"},
		})}
		client, _ := newTestProxy(t, stub)

		_, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{
			Bucket: aws.String("testbucket"),
			Key:    aws.String("k"),
		})
		var respErr *awshttp.ResponseError
		if !errors.As(err, &respErr) {
			t.Fatalf("expected a response error, got %v", err)
		}
		h := respErr.Response.Header
		if got := h.Get("x-amz-delete-marker"); got != "true" {
			t.Errorf("x-amz-delete-marker not relayed on 404: %q", got)
		}
		if got := h.Get("x-amz-version-id"); got != "v1" {
			t.Errorf("x-amz-version-id not relayed on 404: %q", got)
		}
	})
}
