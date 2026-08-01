package s3rp_test

import (
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestProxyObjectLockConfiguration(t *testing.T) {
	stub := &stubBackend{
		getLockCfgOut: &s3.GetObjectLockConfigurationOutput{
			ObjectLockConfiguration: &types.ObjectLockConfiguration{
				ObjectLockEnabled: types.ObjectLockEnabledEnabled,
				Rule: &types.ObjectLockRule{
					DefaultRetention: &types.DefaultRetention{
						Mode: types.ObjectLockRetentionModeGovernance,
						Days: aws.Int32(30),
					},
				},
			},
		},
	}
	client, _ := newTestProxy(t, stub)

	if _, err := client.PutObjectLockConfiguration(t.Context(), &s3.PutObjectLockConfigurationInput{
		Bucket: aws.String("testbucket"),
		ObjectLockConfiguration: &types.ObjectLockConfiguration{
			ObjectLockEnabled: types.ObjectLockEnabledEnabled,
			Rule: &types.ObjectLockRule{
				DefaultRetention: &types.DefaultRetention{
					Mode: types.ObjectLockRetentionModeCompliance,
					Days: aws.Int32(7),
				},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}
	in := stub.putLockCfgIn
	if aws.ToString(in.Bucket) != "backend-testbucket" {
		t.Errorf("expect backend-testbucket, got %s", aws.ToString(in.Bucket))
	}
	if in.ObjectLockConfiguration.Rule.DefaultRetention.Mode != types.ObjectLockRetentionModeCompliance {
		t.Errorf("unexpected mode %v", in.ObjectLockConfiguration.Rule.DefaultRetention.Mode)
	}
	if aws.ToInt32(in.ObjectLockConfiguration.Rule.DefaultRetention.Days) != 7 {
		t.Errorf("unexpected days %v", in.ObjectLockConfiguration.Rule.DefaultRetention.Days)
	}

	out, err := client.GetObjectLockConfiguration(t.Context(), &s3.GetObjectLockConfigurationInput{
		Bucket: aws.String("testbucket"),
	})
	if err != nil {
		t.Fatal(err)
	}
	cfg := out.ObjectLockConfiguration
	if cfg.ObjectLockEnabled != types.ObjectLockEnabledEnabled {
		t.Errorf("expect Enabled, got %v", cfg.ObjectLockEnabled)
	}
	if cfg.Rule.DefaultRetention.Mode != types.ObjectLockRetentionModeGovernance || aws.ToInt32(cfg.Rule.DefaultRetention.Days) != 30 {
		t.Errorf("unexpected default retention %+v", cfg.Rule.DefaultRetention)
	}
}

func TestProxyObjectRetention(t *testing.T) {
	until := time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)
	stub := &stubBackend{
		getRetentionOut: &s3.GetObjectRetentionOutput{
			Retention: &types.ObjectLockRetention{
				Mode:            types.ObjectLockRetentionModeGovernance,
				RetainUntilDate: aws.Time(until),
			},
		},
	}
	client, _ := newTestProxy(t, stub)

	if _, err := client.PutObjectRetention(t.Context(), &s3.PutObjectRetentionInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("locked.txt"),
		Retention: &types.ObjectLockRetention{
			Mode:            types.ObjectLockRetentionModeCompliance,
			RetainUntilDate: aws.Time(until),
		},
	}); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(stub.putRetentionIn.Bucket) != "backend-testbucket" {
		t.Errorf("unexpected bucket %v", stub.putRetentionIn.Bucket)
	}
	if stub.putRetentionIn.Retention.Mode != types.ObjectLockRetentionModeCompliance {
		t.Errorf("unexpected mode %v", stub.putRetentionIn.Retention.Mode)
	}
	if !aws.ToTime(stub.putRetentionIn.Retention.RetainUntilDate).Equal(until) {
		t.Errorf("unexpected retain until %v", stub.putRetentionIn.Retention.RetainUntilDate)
	}

	out, err := client.GetObjectRetention(t.Context(), &s3.GetObjectRetentionInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("locked.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Retention.Mode != types.ObjectLockRetentionModeGovernance {
		t.Errorf("unexpected mode %v", out.Retention.Mode)
	}
	if !aws.ToTime(out.Retention.RetainUntilDate).Equal(until) {
		t.Errorf("unexpected retain until %v", out.Retention.RetainUntilDate)
	}
}

func TestProxyObjectLegalHold(t *testing.T) {
	stub := &stubBackend{
		getLegalHoldOut: &s3.GetObjectLegalHoldOutput{
			LegalHold: &types.ObjectLockLegalHold{Status: types.ObjectLockLegalHoldStatusOn},
		},
	}
	client, _ := newTestProxy(t, stub)

	if _, err := client.PutObjectLegalHold(t.Context(), &s3.PutObjectLegalHoldInput{
		Bucket:    aws.String("testbucket"),
		Key:       aws.String("h.txt"),
		LegalHold: &types.ObjectLockLegalHold{Status: types.ObjectLockLegalHoldStatusOn},
	}); err != nil {
		t.Fatal(err)
	}
	if stub.putLegalHoldIn.LegalHold.Status != types.ObjectLockLegalHoldStatusOn {
		t.Errorf("unexpected status %v", stub.putLegalHoldIn.LegalHold.Status)
	}

	out, err := client.GetObjectLegalHold(t.Context(), &s3.GetObjectLegalHoldInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("h.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.LegalHold.Status != types.ObjectLockLegalHoldStatusOn {
		t.Errorf("unexpected status %v", out.LegalHold.Status)
	}
}

func TestProxyPutObjectWithLock(t *testing.T) {
	until := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)}}
	client, _ := newTestProxy(t, stub)
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket:                    aws.String("testbucket"),
		Key:                       aws.String("locked.txt"),
		Body:                      strings.NewReader("x"),
		ObjectLockMode:            types.ObjectLockModeGovernance,
		ObjectLockRetainUntilDate: aws.Time(until),
		ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOn,
	}); err != nil {
		t.Fatal(err)
	}
	if stub.putIn.ObjectLockMode != types.ObjectLockModeGovernance {
		t.Errorf("unexpected lock mode %v", stub.putIn.ObjectLockMode)
	}
	if !aws.ToTime(stub.putIn.ObjectLockRetainUntilDate).Equal(until) {
		t.Errorf("unexpected retain until %v", stub.putIn.ObjectLockRetainUntilDate)
	}
	if stub.putIn.ObjectLockLegalHoldStatus != types.ObjectLockLegalHoldStatusOn {
		t.Errorf("unexpected legal hold %v", stub.putIn.ObjectLockLegalHoldStatus)
	}
}

func TestProxyGetObjectLockHeaders(t *testing.T) {
	until := time.Date(2030, 6, 1, 0, 0, 0, 0, time.UTC)
	stub := &stubBackend{
		headOut: &s3.HeadObjectOutput{
			ObjectLockMode:            types.ObjectLockModeCompliance,
			ObjectLockRetainUntilDate: aws.Time(until),
			ObjectLockLegalHoldStatus: types.ObjectLockLegalHoldStatusOff,
		},
	}
	client, _ := newTestProxy(t, stub)
	out, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("locked.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ObjectLockMode != types.ObjectLockModeCompliance {
		t.Errorf("unexpected lock mode %v", out.ObjectLockMode)
	}
	if !aws.ToTime(out.ObjectLockRetainUntilDate).Equal(until) {
		t.Errorf("unexpected retain until %v", out.ObjectLockRetainUntilDate)
	}
	if out.ObjectLockLegalHoldStatus != types.ObjectLockLegalHoldStatusOff {
		t.Errorf("unexpected legal hold %v", out.ObjectLockLegalHoldStatus)
	}
}

func TestProxyDeleteBypassGovernance(t *testing.T) {
	stub := &stubBackend{delOut: &s3.DeleteObjectOutput{}}
	client, _ := newTestProxy(t, stub)
	if _, err := client.DeleteObject(t.Context(), &s3.DeleteObjectInput{
		Bucket:                    aws.String("testbucket"),
		Key:                       aws.String("locked.txt"),
		BypassGovernanceRetention: aws.Bool(true),
	}); err != nil {
		t.Fatal(err)
	}
	if !aws.ToBool(stub.delIn.BypassGovernanceRetention) {
		t.Error("expect bypass governance retention forwarded to backend")
	}
}
