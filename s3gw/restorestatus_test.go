package s3gw_test

import (
	"errors"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/google/go-cmp/cmp"
)

// A listing asked for RestoreStatus asks the backend for it and relays what
// the backend reports per entry; a listing that did not ask sends nothing.
func TestListingsRelayRestoreStatus(t *testing.T) {
	expiry := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	restored := &types.RestoreStatus{IsRestoreInProgress: aws.Bool(false), RestoreExpiryDate: aws.Time(expiry)}
	restoring := &types.RestoreStatus{IsRestoreInProgress: aws.Bool(true)}
	objects := []types.Object{
		{Key: aws.String("restored"), LastModified: aws.Time(expiry), RestoreStatus: restored},
		{Key: aws.String("restoring"), LastModified: aws.Time(expiry), RestoreStatus: restoring},
		{Key: aws.String("plain"), LastModified: aws.Time(expiry)},
	}
	stub := &stubBackend{
		listOut:   &s3.ListObjectsV2Output{Contents: objects},
		listV1Out: &s3.ListObjectsOutput{Contents: objects},
		listVerOut: &s3.ListObjectVersionsOutput{Versions: []types.ObjectVersion{
			{Key: aws.String("restored"), VersionId: aws.String("v1"), RestoreStatus: restored},
			{Key: aws.String("restoring"), VersionId: aws.String("v1"), RestoreStatus: restoring},
			{Key: aws.String("plain"), VersionId: aws.String("v1")},
		}},
	}
	client, _ := newTestProxy(t, stub)
	ctx := t.Context()
	bucket := aws.String("testbucket")
	ask := []types.OptionalObjectAttributes{types.OptionalObjectAttributesRestoreStatus}
	want := []*types.RestoreStatus{restored, restoring, nil}
	opt := cmp.AllowUnexported(types.RestoreStatus{})

	t.Run("ListObjectsV2", func(t *testing.T) {
		out, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket, OptionalObjectAttributes: ask})
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(ask, stub.listIn.OptionalObjectAttributes); diff != "" {
			t.Errorf("sent to the backend (-want +got):\n%s", diff)
		}
		var got []*types.RestoreStatus
		for _, o := range out.Contents {
			got = append(got, o.RestoreStatus)
		}
		if diff := cmp.Diff(want, got, opt); diff != "" {
			t.Errorf("restore status (-want +got):\n%s", diff)
		}
	})
	t.Run("ListObjects", func(t *testing.T) {
		out, err := client.ListObjects(ctx, &s3.ListObjectsInput{Bucket: bucket, OptionalObjectAttributes: ask})
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(ask, stub.listV1In.OptionalObjectAttributes); diff != "" {
			t.Errorf("sent to the backend (-want +got):\n%s", diff)
		}
		var got []*types.RestoreStatus
		for _, o := range out.Contents {
			got = append(got, o.RestoreStatus)
		}
		if diff := cmp.Diff(want, got, opt); diff != "" {
			t.Errorf("restore status (-want +got):\n%s", diff)
		}
	})
	t.Run("ListObjectVersions", func(t *testing.T) {
		out, err := client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: bucket, OptionalObjectAttributes: ask})
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(ask, stub.listVerIn.OptionalObjectAttributes); diff != "" {
			t.Errorf("sent to the backend (-want +got):\n%s", diff)
		}
		var got []*types.RestoreStatus
		for _, v := range out.Versions {
			got = append(got, v.RestoreStatus)
		}
		if diff := cmp.Diff(want, got, opt); diff != "" {
			t.Errorf("restore status (-want +got):\n%s", diff)
		}
	})
	// a backend that reports the status unasked does not get it relayed
	t.Run("not asked", func(t *testing.T) {
		v2, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket})
		if err != nil {
			t.Fatal(err)
		}
		if got := stub.listIn.OptionalObjectAttributes; len(got) != 0 {
			t.Errorf("sent to the backend unasked: %v", got)
		}
		v1, err := client.ListObjects(ctx, &s3.ListObjectsInput{Bucket: bucket})
		if err != nil {
			t.Fatal(err)
		}
		ver, err := client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: bucket})
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range append(v2.Contents, v1.Contents...) {
			if o.RestoreStatus != nil {
				t.Errorf("%s: restore status shown unasked", aws.ToString(o.Key))
			}
		}
		for _, v := range ver.Versions {
			if v.RestoreStatus != nil {
				t.Errorf("version %s: restore status shown unasked", aws.ToString(v.Key))
			}
		}
	})
	// an attribute the listings would not relay is refused, not dropped
	t.Run("unknown attribute", func(t *testing.T) {
		stub.listIn = nil
		_, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:                   bucket,
			OptionalObjectAttributes: []types.OptionalObjectAttributes{"RestoreStatus", "Other"},
		})
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NotImplemented" {
			t.Fatalf("expect NotImplemented, got %v", err)
		}
		if stub.listIn != nil {
			t.Error("the backend was called")
		}
	})
}
