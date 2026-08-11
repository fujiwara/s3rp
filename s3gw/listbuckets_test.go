package s3gw_test

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
	"github.com/google/go-cmp/cmp"
)

func listBucketsGateway(t *testing.T, names ...string) *s3.Client {
	t.Helper()
	buckets := make(map[string]*store.Bucket, len(names))
	for _, n := range names {
		buckets[n] = &store.Bucket{Tenant: "testtenant", Name: n, Backend: &store.Backend{}}
	}
	gw := s3gw.New(memStore{
		keys: map[string]*store.Key{
			testAccessKeyID: {
				AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey,
				Tenant: "testtenant", User: "testuser",
			},
		},
		buckets: buckets,
	})
	client, _, _ := newSDKClientFor(t, gw)
	return client
}

func TestListBucketsPagination(t *testing.T) {
	client := listBucketsGateway(t, "b1", "b2", "b3", "b4", "b5")

	names := func(out *s3.ListBucketsOutput) []string {
		var ns []string
		for _, b := range out.Buckets {
			ns = append(ns, aws.ToString(b.Name))
		}
		return ns
	}

	// first page: truncated, with the token for the next one
	out, err := client.ListBuckets(t.Context(), &s3.ListBucketsInput{MaxBuckets: aws.Int32(2)})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"b1", "b2"}, names(out)); diff != "" {
		t.Errorf("first page (-want +got):\n%s", diff)
	}
	token := aws.ToString(out.ContinuationToken)
	if token == "" {
		t.Fatal("expected a continuation token on a truncated listing")
	}

	// second page continues after the token
	out, err = client.ListBuckets(t.Context(), &s3.ListBucketsInput{
		MaxBuckets: aws.Int32(2), ContinuationToken: aws.String(token),
	})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"b3", "b4"}, names(out)); diff != "" {
		t.Errorf("second page (-want +got):\n%s", diff)
	}

	// final page: the rest, no token
	out, err = client.ListBuckets(t.Context(), &s3.ListBucketsInput{
		MaxBuckets: aws.Int32(2), ContinuationToken: out.ContinuationToken,
	})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]string{"b5"}, names(out)); diff != "" {
		t.Errorf("final page (-want +got):\n%s", diff)
	}
	if out.ContinuationToken != nil {
		t.Errorf("final page must carry no token, got %q", *out.ContinuationToken)
	}

	// everything fits: no token
	out, err = client.ListBuckets(t.Context(), &s3.ListBucketsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Buckets) != 5 || out.ContinuationToken != nil {
		t.Errorf("untruncated listing must return all buckets and no token: %d %v",
			len(out.Buckets), out.ContinuationToken)
	}
}

func TestListBucketsRejectsUnknownParams(t *testing.T) {
	client := listBucketsGateway(t, "b1")
	// prefix is not supported; like every unknown query parameter it is
	// refused loudly rather than silently ignored
	_, err := client.ListBuckets(t.Context(), &s3.ListBucketsInput{Prefix: aws.String("b")})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NotImplemented" {
		t.Errorf("expected NotImplemented, got %v", err)
	}
}

func TestListBucketsInvalidMaxBuckets(t *testing.T) {
	client := listBucketsGateway(t, "b1")
	_, err := client.ListBuckets(t.Context(), &s3.ListBucketsInput{MaxBuckets: aws.Int32(0)})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "InvalidArgument" {
		t.Errorf("expected InvalidArgument, got %v", err)
	}
}
