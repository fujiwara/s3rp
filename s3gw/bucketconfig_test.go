package s3gw_test

import (
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

func TestProxyBucketPolicyStatus(t *testing.T) {
	client, _ := newTestProxy(t, &stubBackend{})
	out, err := client.GetBucketPolicyStatus(t.Context(), &s3.GetBucketPolicyStatusInput{
		Bucket: aws.String("testbucket"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.PolicyStatus == nil || aws.ToBool(out.PolicyStatus.IsPublic) {
		t.Errorf("expect IsPublic=false, got %+v", out.PolicyStatus)
	}
}

func TestProxyBucketOwnershipControls(t *testing.T) {
	client, _ := newTestProxy(t, &stubBackend{})
	out, err := client.GetBucketOwnershipControls(t.Context(), &s3.GetBucketOwnershipControlsInput{
		Bucket: aws.String("testbucket"),
	})
	if err != nil {
		t.Fatal(err)
	}
	rules := out.OwnershipControls.Rules
	if len(rules) != 1 || rules[0].ObjectOwnership != types.ObjectOwnershipBucketOwnerEnforced {
		t.Errorf("expect one BucketOwnerEnforced rule, got %+v", rules)
	}
}

func TestProxyPublicAccessBlock(t *testing.T) {
	client, _ := newTestProxy(t, &stubBackend{})
	out, err := client.GetPublicAccessBlock(t.Context(), &s3.GetPublicAccessBlockInput{
		Bucket: aws.String("testbucket"),
	})
	if err != nil {
		t.Fatal(err)
	}
	c := out.PublicAccessBlockConfiguration
	if c == nil || !aws.ToBool(c.BlockPublicAcls) || !aws.ToBool(c.IgnorePublicAcls) ||
		!aws.ToBool(c.BlockPublicPolicy) || !aws.ToBool(c.RestrictPublicBuckets) {
		t.Errorf("expect every block set, got %+v", c)
	}
}

func TestProxyBucketEncryption(t *testing.T) {
	stub := &stubBackend{
		getEncOut: &s3.GetBucketEncryptionOutput{
			ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
				Rules: []types.ServerSideEncryptionRule{{
					ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{
						SSEAlgorithm:   types.ServerSideEncryptionAwsKms,
						KMSMasterKeyID: aws.String("testkey-1"),
					},
					BucketKeyEnabled: aws.Bool(false),
				}},
			},
		},
	}
	client, _ := newTestProxy(t, stub)
	out, err := client.GetBucketEncryption(t.Context(), &s3.GetBucketEncryptionInput{
		Bucket: aws.String("testbucket"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(stub.getEncIn.Bucket); got != "backend-testbucket" {
		t.Errorf("expect the backend bucket, got %q", got)
	}
	rules := out.ServerSideEncryptionConfiguration.Rules
	if len(rules) != 1 {
		t.Fatalf("expect one rule, got %+v", rules)
	}
	d := rules[0].ApplyServerSideEncryptionByDefault
	if d == nil || d.SSEAlgorithm != types.ServerSideEncryptionAwsKms || aws.ToString(d.KMSMasterKeyID) != "testkey-1" {
		t.Errorf("unexpected default %+v", d)
	}
	if aws.ToBool(rules[0].BucketKeyEnabled) {
		t.Error("expect BucketKeyEnabled=false")
	}
}

func TestProxyBucketEncryptionNotConfigured(t *testing.T) {
	stub := &stubBackend{getEncErr: &smithy.GenericAPIError{
		Code: "ServerSideEncryptionConfigurationNotFoundError", Message: "x",
	}}
	client, _ := newTestProxy(t, stub)
	_, err := client.GetBucketEncryption(t.Context(), &s3.GetBucketEncryptionInput{
		Bucket: aws.String("testbucket"),
	})
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "ServerSideEncryptionConfigurationNotFoundError" {
		t.Errorf("expect the backend's code, got %v", err)
	}
}

func TestProxyBucketConfigurationWritesRefused(t *testing.T) {
	client, _ := newTestProxy(t, &stubBackend{})
	bucket := aws.String("testbucket")
	calls := map[string]func() error{
		"PutBucketEncryption": func() error {
			_, err := client.PutBucketEncryption(t.Context(), &s3.PutBucketEncryptionInput{
				Bucket: bucket, ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{
					Rules: []types.ServerSideEncryptionRule{{ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256}}},
				},
			})
			return err
		},
		"DeleteBucketEncryption": func() error {
			_, err := client.DeleteBucketEncryption(t.Context(), &s3.DeleteBucketEncryptionInput{Bucket: bucket})
			return err
		},
		"PutBucketOwnershipControls": func() error {
			_, err := client.PutBucketOwnershipControls(t.Context(), &s3.PutBucketOwnershipControlsInput{
				Bucket: bucket, OwnershipControls: &types.OwnershipControls{
					Rules: []types.OwnershipControlsRule{{ObjectOwnership: types.ObjectOwnershipBucketOwnerEnforced}},
				},
			})
			return err
		},
		"DeleteBucketOwnershipControls": func() error {
			_, err := client.DeleteBucketOwnershipControls(t.Context(), &s3.DeleteBucketOwnershipControlsInput{Bucket: bucket})
			return err
		},
		"PutPublicAccessBlock": func() error {
			_, err := client.PutPublicAccessBlock(t.Context(), &s3.PutPublicAccessBlockInput{
				Bucket: bucket, PublicAccessBlockConfiguration: &types.PublicAccessBlockConfiguration{},
			})
			return err
		},
		"DeletePublicAccessBlock": func() error {
			_, err := client.DeletePublicAccessBlock(t.Context(), &s3.DeletePublicAccessBlockInput{Bucket: bucket})
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			var apiErr smithy.APIError
			if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NotImplemented" {
				t.Errorf("expect NotImplemented, got %v", err)
			}
		})
	}
}
