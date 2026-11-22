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
)

// the target is a front bucket
const logToAudit = `<?xml version="1.0" encoding="UTF-8"?>
<BucketLoggingStatus xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <LoggingEnabled>
    <TargetBucket>audit</TargetBucket>
    <TargetPrefix>access/ruled/</TargetPrefix>
  </LoggingEnabled>
</BucketLoggingStatus>`

func TestGetBucketLogging(t *testing.T) {
	clients, _ := newBucketConfigProxy(t)
	ctx := t.Context()

	out, err := clients["alice"].GetBucketLogging(ctx, &s3.GetBucketLoggingInput{Bucket: aws.String("ruled")})
	if err != nil {
		t.Fatal(err)
	}
	if l := out.LoggingEnabled; l == nil || aws.ToString(l.TargetBucket) != "audit" || aws.ToString(l.TargetPrefix) != "access/ruled/" {
		t.Errorf("the stored status did not come back: %+v", out.LoggingEnabled)
	}

	// S3 answers a bucket whose logging is off with an empty status
	out, err = clients["alice"].GetBucketLogging(ctx, &s3.GetBucketLoggingInput{Bucket: aws.String("plain")})
	if err != nil {
		t.Fatalf("a bucket without logging: %v", err)
	}
	if out.LoggingEnabled != nil {
		t.Errorf("a bucket without logging: got %+v, want no LoggingEnabled", out.LoggingEnabled)
	}
}

func TestGetBucketLoggingAuthorized(t *testing.T) {
	clients, _ := newBucketConfigProxy(t)
	get := func(user, bucket string) error {
		_, err := clients[user].GetBucketLogging(t.Context(), &s3.GetBucketLoggingInput{Bucket: aws.String(bucket)})
		return err
	}
	// the user policy allows s3:PutObject only
	expectAccessDenied(t, get("writer", "ruled"))
	// a foreign tenant reads it with a grant, and not without: the empty
	// answer is no less a read than a status is
	if err := get("bob", "shared"); err != nil {
		t.Errorf("bob has a grant: %v", err)
	}
	expectAccessDenied(t, get("carol", "shared"))
	expectAccessDenied(t, get("bob", "ruled"))
	expectAccessDenied(t, get("bob", "plain"))
}

func TestBucketLoggingWriteRefused(t *testing.T) {
	clients, gw := newBucketConfigProxy(t)
	observed := make(chan *s3gw.RequestInfo, 1)
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { observed <- info })
	_, err := clients["alice"].PutBucketLogging(t.Context(), &s3.PutBucketLoggingInput{
		Bucket: aws.String("ruled"),
		BucketLoggingStatus: &types.BucketLoggingStatus{
			LoggingEnabled: &types.LoggingEnabled{TargetBucket: aws.String("audit"), TargetPrefix: aws.String("x/")},
		},
	})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NotImplemented" {
		t.Fatalf("expect NotImplemented, got %v", err)
	}
	if info := <-observed; info.Route != s3op.OpPutBucketLogging {
		t.Errorf("the attempt is recorded as route %q, want %s", info.Route, s3op.OpPutBucketLogging)
	}
}
