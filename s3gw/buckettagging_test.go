package s3gw_test

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/s3op"
	"github.com/google/go-cmp/cmp"
)

// a value that needs escaping in XML, and keys out of order
var bucketTags = map[string]string{"team": "storage", "env": "prod", "note": "a<b & c"}

func TestGetBucketTagging(t *testing.T) {
	clients, _ := newBucketConfigProxy(t)
	ctx := t.Context()

	out, err := clients["alice"].GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String("ruled")})
	if err != nil {
		t.Fatal(err)
	}
	var got [][2]string
	for _, tag := range out.TagSet {
		got = append(got, [2]string{aws.ToString(tag.Key), aws.ToString(tag.Value)})
	}
	// sorted by key, so a response does not differ from the one before
	want := [][2]string{{"env", "prod"}, {"note", "a<b & c"}, {"team", "storage"}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("tags (-want +got):\n%s", diff)
	}

	_, err = clients["alice"].GetBucketTagging(ctx, &s3.GetBucketTaggingInput{Bucket: aws.String("plain")})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NoSuchTagSet" || apiErr.ErrorMessage() != "The TagSet does not exist" {
		t.Errorf("a bucket without tags: got %v", err)
	}
}

func TestGetBucketTaggingAuthorized(t *testing.T) {
	clients, _ := newBucketConfigProxy(t)
	get := func(user, bucket string) error {
		_, err := clients[user].GetBucketTagging(t.Context(), &s3.GetBucketTaggingInput{Bucket: aws.String(bucket)})
		return err
	}
	// the user policy allows s3:PutObject only
	expectAccessDenied(t, get("writer", "ruled"))
	// a foreign tenant reads them with a grant, and not without
	if err := get("bob", "shared"); err != nil {
		t.Errorf("bob has a grant: %v", err)
	}
	expectAccessDenied(t, get("carol", "shared"))
	expectAccessDenied(t, get("bob", "ruled"))
}

func TestBucketTaggingWritesRefused(t *testing.T) {
	clients, gw := newBucketConfigProxy(t)
	observed := make(chan *s3gw.RequestInfo, 1)
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { observed <- info })
	bucket := aws.String("ruled")
	for name, call := range map[string]func() error{
		s3op.OpPutBucketTagging: func() error {
			_, err := clients["alice"].PutBucketTagging(t.Context(), &s3.PutBucketTaggingInput{
				Bucket:  bucket,
				Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("env"), Value: aws.String("dev")}}},
			})
			return err
		},
		s3op.OpDeleteBucketTagging: func() error {
			_, err := clients["alice"].DeleteBucketTagging(t.Context(), &s3.DeleteBucketTaggingInput{Bucket: bucket})
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := call()
			var apiErr smithy.APIError
			if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NotImplemented" {
				t.Fatalf("expect NotImplemented, got %v", err)
			}
			if info := <-observed; info.Route != name {
				t.Errorf("the attempt is recorded as route %q, want %s", info.Route, name)
			}
		})
	}
}
