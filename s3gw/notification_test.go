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

// the topic is the service's name for the destination
const notifyOnCreate = `<?xml version="1.0" encoding="UTF-8"?>
<NotificationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <TopicConfiguration>
    <Id>on-create</Id>
    <Topic>arn:aws:sns:us-east-1:000000000000:bucket-events</Topic>
    <Event>s3:ObjectCreated:*</Event>
    <Event>s3:ObjectRemoved:Delete</Event>
    <Filter><S3Key><FilterRule><Name>prefix</Name><Value>incoming/</Value></FilterRule></S3Key></Filter>
  </TopicConfiguration>
</NotificationConfiguration>`

func TestGetBucketNotificationConfiguration(t *testing.T) {
	clients, _ := newBucketConfigProxy(t)
	ctx := t.Context()

	out, err := clients["alice"].GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String("ruled")})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.TopicConfigurations) != 1 {
		t.Fatalf("got %d topic configurations, want 1", len(out.TopicConfigurations))
	}
	topic := out.TopicConfigurations[0]
	if aws.ToString(topic.Id) != "on-create" || aws.ToString(topic.TopicArn) != "arn:aws:sns:us-east-1:000000000000:bucket-events" {
		t.Errorf("the stored configuration did not come back: %+v", topic)
	}
	if diff := cmp.Diff([]types.Event{"s3:ObjectCreated:*", "s3:ObjectRemoved:Delete"}, topic.Events); diff != "" {
		t.Errorf("events (-want +got):\n%s", diff)
	}
	if topic.Filter == nil || topic.Filter.Key == nil || len(topic.Filter.Key.FilterRules) != 1 ||
		aws.ToString(topic.Filter.Key.FilterRules[0].Value) != "incoming/" {
		t.Errorf("the filter did not come back: %+v", topic.Filter)
	}

	// S3 answers a bucket without a configuration with an empty one
	out, err = clients["alice"].GetBucketNotificationConfiguration(ctx, &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String("plain")})
	if err != nil {
		t.Fatalf("a bucket without a configuration: %v", err)
	}
	if len(out.TopicConfigurations)+len(out.QueueConfigurations)+len(out.LambdaFunctionConfigurations) != 0 || out.EventBridgeConfiguration != nil {
		t.Errorf("a bucket without a configuration: got %+v, want an empty one", out)
	}
}

func TestGetBucketNotificationConfigurationAuthorized(t *testing.T) {
	clients, _ := newBucketConfigProxy(t)
	get := func(user, bucket string) error {
		_, err := clients[user].GetBucketNotificationConfiguration(t.Context(), &s3.GetBucketNotificationConfigurationInput{Bucket: aws.String(bucket)})
		return err
	}
	// the user policy allows s3:PutObject only
	expectAccessDenied(t, get("writer", "ruled"))
	// a foreign tenant reads it with a grant, and not without: the empty
	// answer is no less a read than a configuration is
	if err := get("bob", "shared"); err != nil {
		t.Errorf("bob has a grant: %v", err)
	}
	expectAccessDenied(t, get("carol", "shared"))
	expectAccessDenied(t, get("bob", "ruled"))
	expectAccessDenied(t, get("bob", "plain"))
}

func TestBucketNotificationWriteRefused(t *testing.T) {
	clients, gw := newBucketConfigProxy(t)
	observed := make(chan *s3gw.RequestInfo, 1)
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { observed <- info })
	_, err := clients["alice"].PutBucketNotificationConfiguration(t.Context(), &s3.PutBucketNotificationConfigurationInput{
		Bucket:                    aws.String("ruled"),
		NotificationConfiguration: &types.NotificationConfiguration{},
	})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NotImplemented" {
		t.Fatalf("expect NotImplemented, got %v", err)
	}
	if info := <-observed; info.Route != s3op.OpPutBucketNotificationConfiguration {
		t.Errorf("the attempt is recorded as route %q, want %s", info.Route, s3op.OpPutBucketNotificationConfiguration)
	}
}
