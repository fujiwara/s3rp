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

// the destination is a front bucket, in the form S3's schema gives it
const replicateToBackup = `<?xml version="1.0" encoding="UTF-8"?>
<ReplicationConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/">
  <Role>replication</Role>
  <Rule>
    <ID>to-backup</ID>
    <Priority>1</Priority>
    <Status>Enabled</Status>
    <Filter><Prefix>docs/</Prefix></Filter>
    <DeleteMarkerReplication><Status>Disabled</Status></DeleteMarkerReplication>
    <Destination><Bucket>arn:aws:s3:::backup</Bucket></Destination>
  </Rule>
</ReplicationConfiguration>`

func TestGetBucketReplication(t *testing.T) {
	clients, _ := newBucketConfigProxy(t)
	ctx := t.Context()

	out, err := clients["alice"].GetBucketReplication(ctx, &s3.GetBucketReplicationInput{Bucket: aws.String("ruled")})
	if err != nil {
		t.Fatal(err)
	}
	cfg := out.ReplicationConfiguration
	if cfg == nil || aws.ToString(cfg.Role) != "replication" || len(cfg.Rules) != 1 {
		t.Fatalf("the stored configuration did not come back: %+v", cfg)
	}
	r := cfg.Rules[0]
	if aws.ToString(r.ID) != "to-backup" || r.Status != types.ReplicationRuleStatusEnabled || aws.ToInt32(r.Priority) != 1 ||
		r.Filter == nil || aws.ToString(r.Filter.Prefix) != "docs/" ||
		r.Destination == nil || aws.ToString(r.Destination.Bucket) != "arn:aws:s3:::backup" {
		t.Errorf("the stored rule did not come back: %+v", r)
	}

	_, err = clients["alice"].GetBucketReplication(ctx, &s3.GetBucketReplicationInput{Bucket: aws.String("plain")})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "ReplicationConfigurationNotFoundError" ||
		apiErr.ErrorMessage() != "The replication configuration was not found" {
		t.Errorf("a bucket without a configuration: got %v", err)
	}
}

func TestGetBucketReplicationAuthorized(t *testing.T) {
	clients, _ := newBucketConfigProxy(t)
	get := func(user, bucket string) error {
		_, err := clients[user].GetBucketReplication(t.Context(), &s3.GetBucketReplicationInput{Bucket: aws.String(bucket)})
		return err
	}
	// the user policy allows s3:PutObject only
	expectAccessDenied(t, get("writer", "ruled"))
	// a foreign tenant reads it with a grant, and not without
	if err := get("bob", "shared"); err != nil {
		t.Errorf("bob has a grant: %v", err)
	}
	expectAccessDenied(t, get("carol", "shared"))
	expectAccessDenied(t, get("bob", "ruled"))
}

func TestBucketReplicationWritesRefused(t *testing.T) {
	clients, gw := newBucketConfigProxy(t)
	observed := make(chan *s3gw.RequestInfo, 1)
	gw.SetObserver(func(_ context.Context, info *s3gw.RequestInfo) { observed <- info })
	bucket := aws.String("ruled")
	for name, call := range map[string]func() error{
		s3op.OpPutBucketReplication: func() error {
			_, err := clients["alice"].PutBucketReplication(t.Context(), &s3.PutBucketReplicationInput{
				Bucket: bucket,
				ReplicationConfiguration: &types.ReplicationConfiguration{
					Role: aws.String("replication"),
					Rules: []types.ReplicationRule{{
						Status:      types.ReplicationRuleStatusEnabled,
						Destination: &types.Destination{Bucket: aws.String("arn:aws:s3:::backup")},
					}},
				},
			})
			return err
		},
		s3op.OpDeleteBucketReplication: func() error {
			_, err := clients["alice"].DeleteBucketReplication(t.Context(), &s3.DeleteBucketReplicationInput{Bucket: bucket})
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
