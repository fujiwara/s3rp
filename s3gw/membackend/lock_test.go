package membackend_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func TestObjectLock(t *testing.T) {
	f := newFixture(t)
	f.createBucket("vault", true)
	f.createBucket("plain", false)
	now := time.Now()
	f.backend.SetNow(func() time.Time { return now })
	until := now.Add(24 * time.Hour).UTC().Truncate(time.Second)

	putLocked := func(key string, mode types.ObjectLockMode) *string {
		t.Helper()
		out, err := f.client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("vault"), Key: aws.String(key), Body: strings.NewReader("worm"),
			ObjectLockMode: mode, ObjectLockRetainUntilDate: aws.Time(until),
		})
		if err != nil {
			t.Fatal(err)
		}
		return out.VersionId
	}
	del := func(key string, versionID *string, bypass bool) error {
		in := &s3.DeleteObjectInput{Bucket: aws.String("vault"), Key: aws.String(key), VersionId: versionID}
		if bypass {
			in.BypassGovernanceRetention = aws.Bool(true)
		}
		_, err := f.client.DeleteObject(t.Context(), in)
		return err
	}

	t.Run("a bucket without Object Lock refuses lock settings", func(t *testing.T) {
		_, err := f.client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("plain"), Key: aws.String("k"), Body: strings.NewReader("x"),
			ObjectLockMode: types.ObjectLockModeGovernance, ObjectLockRetainUntilDate: aws.Time(until),
		})
		wantError(t, err, http.StatusBadRequest, "InvalidRequest")
		_, err = f.client.GetObjectLockConfiguration(t.Context(), &s3.GetObjectLockConfigurationInput{Bucket: aws.String("plain")})
		wantError(t, err, http.StatusNotFound, "ObjectLockConfigurationNotFoundError")
	})

	t.Run("governance", func(t *testing.T) {
		v := putLocked("gov", types.ObjectLockModeGovernance)
		ret, err := f.client.GetObjectRetention(t.Context(), &s3.GetObjectRetentionInput{Bucket: aws.String("vault"), Key: aws.String("gov")})
		if err != nil {
			t.Fatal(err)
		}
		if ret.Retention.Mode != types.ObjectLockRetentionModeGovernance || !ret.Retention.RetainUntilDate.Equal(until) {
			t.Errorf("retention = %+v", ret.Retention)
		}
		// a plain delete only adds a delete marker; the version is what
		// retention protects
		if err := del("gov", nil, false); err != nil {
			t.Errorf("a delete marker over a retained version: %v", err)
		}
		wantError(t, del("gov", v, false), http.StatusForbidden, "AccessDenied")
		// shortening the retention needs the bypass too
		_, err = f.client.PutObjectRetention(t.Context(), &s3.PutObjectRetentionInput{
			Bucket: aws.String("vault"), Key: aws.String("gov"), VersionId: v,
			Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeGovernance, RetainUntilDate: aws.Time(until.Add(-time.Hour))},
		})
		wantError(t, err, http.StatusForbidden, "AccessDenied")
		if err := del("gov", v, true); err != nil {
			t.Errorf("the governance bypass: %v", err)
		}
	})

	t.Run("compliance", func(t *testing.T) {
		v := putLocked("comp", types.ObjectLockModeCompliance)
		wantError(t, del("comp", v, true), http.StatusForbidden, "AccessDenied")
		// extending is always allowed
		if _, err := f.client.PutObjectRetention(t.Context(), &s3.PutObjectRetentionInput{
			Bucket: aws.String("vault"), Key: aws.String("comp"), VersionId: v,
			Retention: &types.ObjectLockRetention{Mode: types.ObjectLockRetentionModeCompliance, RetainUntilDate: aws.Time(until.Add(time.Hour))},
		}); err != nil {
			t.Errorf("extending a compliance retention: %v", err)
		}
		// once the retention has run out the version can go, and not a
		// moment before
		ends := until.Add(time.Hour)
		f.backend.SetNow(func() time.Time { return ends.Add(-time.Millisecond) })
		wantError(t, del("comp", v, false), http.StatusForbidden, "AccessDenied")
		f.backend.SetNow(func() time.Time { return ends.Add(time.Millisecond) })
		defer f.backend.SetNow(func() time.Time { return now })
		if err := del("comp", v, false); err != nil {
			t.Errorf("after the retention ended: %v", err)
		}
	})

	t.Run("legal hold", func(t *testing.T) {
		out, err := f.client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("vault"), Key: aws.String("held"), Body: strings.NewReader("x"),
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = f.client.GetObjectLegalHold(t.Context(), &s3.GetObjectLegalHoldInput{Bucket: aws.String("vault"), Key: aws.String("held")})
		wantError(t, err, http.StatusNotFound, "NoSuchObjectLockConfiguration")
		hold := func(status types.ObjectLockLegalHoldStatus) {
			t.Helper()
			if _, err := f.client.PutObjectLegalHold(t.Context(), &s3.PutObjectLegalHoldInput{
				Bucket: aws.String("vault"), Key: aws.String("held"),
				LegalHold: &types.ObjectLockLegalHold{Status: status},
			}); err != nil {
				t.Fatal(err)
			}
		}
		hold(types.ObjectLockLegalHoldStatusOn)
		wantError(t, del("held", out.VersionId, true), http.StatusForbidden, "AccessDenied")
		hold(types.ObjectLockLegalHoldStatusOff)
		if err := del("held", out.VersionId, false); err != nil {
			t.Errorf("after the hold was lifted: %v", err)
		}
	})

	t.Run("default retention", func(t *testing.T) {
		if _, err := f.backend.PutObjectLockConfiguration(t.Context(), &s3.PutObjectLockConfigurationInput{
			Bucket: aws.String(backendName("vault")),
			ObjectLockConfiguration: &types.ObjectLockConfiguration{
				ObjectLockEnabled: types.ObjectLockEnabledEnabled,
				Rule:              &types.ObjectLockRule{DefaultRetention: &types.DefaultRetention{Mode: types.ObjectLockRetentionModeGovernance, Days: aws.Int32(7)}},
			},
		}); err != nil {
			t.Fatal(err)
		}
		cfg, err := f.client.GetObjectLockConfiguration(t.Context(), &s3.GetObjectLockConfigurationInput{Bucket: aws.String("vault")})
		if err != nil {
			t.Fatal(err)
		}
		if aws.ToInt32(cfg.ObjectLockConfiguration.Rule.DefaultRetention.Days) != 7 {
			t.Errorf("configuration = %+v", cfg.ObjectLockConfiguration)
		}
		if _, err := f.client.PutObject(t.Context(), &s3.PutObjectInput{
			Bucket: aws.String("vault"), Key: aws.String("defaulted"), Body: strings.NewReader("x"),
		}); err != nil {
			t.Fatal(err)
		}
		head, err := f.client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String("vault"), Key: aws.String("defaulted")})
		if err != nil {
			t.Fatal(err)
		}
		want := now.UTC().Truncate(time.Second).AddDate(0, 0, 7)
		if head.ObjectLockMode != types.ObjectLockModeGovernance || !head.ObjectLockRetainUntilDate.Equal(want) {
			t.Errorf("default retention gave %q until %v, want GOVERNANCE until %v", head.ObjectLockMode, head.ObjectLockRetainUntilDate, want)
		}
	})
}
