package localbackend

import (
	"context"
	"net/http"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
)

func missingLockConfiguration() *failure {
	return fail(http.StatusBadRequest, "InvalidRequest", "Bucket is missing Object Lock Configuration")
}

// applyLockOnWrite checks the Object Lock settings a write carries and
// gives an object written without any the bucket's default retention.
func applyLockOnWrite(bkt *bucket, o *object, now time.Time) *failure {
	if o.lockMode != "" || o.retainUntil != nil || o.legalHold != "" {
		if bkt.lock == nil {
			return missingLockConfiguration()
		}
		if (o.lockMode == "") != (o.retainUntil == nil) {
			return invalidArgument("x-amz-object-lock-retain-until-date and x-amz-object-lock-mode must both be supplied")
		}
		if o.lockMode != "" && o.lockMode != types.ObjectLockModeGovernance && o.lockMode != types.ObjectLockModeCompliance {
			return invalidArgument("Unknown wormMode directive.")
		}
		if o.retainUntil != nil && !o.retainUntil.After(now) {
			return invalidArgument("The retain until date must be in the future!")
		}
		if o.legalHold != "" && o.legalHold != types.ObjectLockLegalHoldStatusOn && o.legalHold != types.ObjectLockLegalHoldStatusOff {
			return invalidArgument("Legal Hold must be either of 'ON' or 'OFF'")
		}
	}
	if o.lockMode == "" && bkt.lock != nil && bkt.lock.Rule != nil && bkt.lock.Rule.DefaultRetention != nil {
		dr := bkt.lock.Rule.DefaultRetention
		until := now.AddDate(int(aws.ToInt32(dr.Years)), 0, int(aws.ToInt32(dr.Days)))
		o.lockMode = types.ObjectLockMode(dr.Mode)
		o.retainUntil = &until
	}
	return nil
}

// locked reports whether Object Lock keeps a version from being deleted:
// a legal hold always, compliance retention until it ends, governance
// retention unless the request bypasses it.
func (o *object) locked(now time.Time, bypass bool) bool {
	if o.legalHold == types.ObjectLockLegalHoldStatusOn {
		return true
	}
	if o.retainUntil == nil || !o.retainUntil.After(now) {
		return false
	}
	return o.lockMode == types.ObjectLockModeCompliance || !bypass
}

func noObjectLockConfiguration() *failure {
	return fail(http.StatusNotFound, "NoSuchObjectLockConfiguration",
		"The specified object does not have a ObjectLock configuration")
}

func (b *Backend) GetObjectRetention(ctx context.Context, in *s3.GetObjectRetentionInput, _ ...func(*s3.Options)) (*s3.GetObjectRetentionOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	if bkt.lock == nil {
		return nil, missingLockConfiguration().err()
	}
	o, f := bkt.find(in.Key, in.VersionId)
	if f != nil {
		return nil, f.err()
	}
	if o.lockMode == "" {
		return nil, noObjectLockConfiguration().err()
	}
	return &s3.GetObjectRetentionOutput{Retention: &types.ObjectLockRetention{
		Mode:            types.ObjectLockRetentionMode(o.lockMode),
		RetainUntilDate: o.retainUntil,
	}}, nil
}

func (b *Backend) PutObjectRetention(ctx context.Context, in *s3.PutObjectRetentionInput, _ ...func(*s3.Options)) (*s3.PutObjectRetentionOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer b.mutate()()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	if bkt.lock == nil {
		return nil, missingLockConfiguration().err()
	}
	o, f := bkt.find(in.Key, in.VersionId)
	if f != nil {
		return nil, f.err()
	}
	r := in.Retention
	if r == nil || r.RetainUntilDate == nil ||
		(r.Mode != types.ObjectLockRetentionModeGovernance && r.Mode != types.ObjectLockRetentionModeCompliance) {
		return nil, malformedXML().err()
	}
	// the real time, not the whole second objects are stamped with: a
	// retention ends when it says, not up to a second later
	now := b.now()
	if !r.RetainUntilDate.After(now) {
		return nil, invalidArgument("The retain until date must be in the future!").err()
	}
	// a retention still in force may be extended in its own mode; anything
	// else — shortening it, changing the mode either way — compliance never
	// allows and governance only with the bypass
	if o.retainUntil != nil && o.retainUntil.After(now) {
		changes := types.ObjectLockMode(r.Mode) != o.lockMode || r.RetainUntilDate.Before(*o.retainUntil)
		if changes && (o.lockMode == types.ObjectLockModeCompliance || !aws.ToBool(in.BypassGovernanceRetention)) {
			return nil, fail(http.StatusForbidden, "AccessDenied",
				"Access Denied because object protected by object lock.").err()
		}
	}
	until := *r.RetainUntilDate
	o.lockMode, o.retainUntil = types.ObjectLockMode(r.Mode), &until
	return &s3.PutObjectRetentionOutput{}, nil
}

func (b *Backend) GetObjectLegalHold(ctx context.Context, in *s3.GetObjectLegalHoldInput, _ ...func(*s3.Options)) (*s3.GetObjectLegalHoldOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	b.mu.RLock()
	defer b.mu.RUnlock()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	if bkt.lock == nil {
		return nil, missingLockConfiguration().err()
	}
	o, f := bkt.find(in.Key, in.VersionId)
	if f != nil {
		return nil, f.err()
	}
	if o.legalHold == "" {
		return nil, noObjectLockConfiguration().err()
	}
	return &s3.GetObjectLegalHoldOutput{LegalHold: &types.ObjectLockLegalHold{Status: o.legalHold}}, nil
}

func (b *Backend) PutObjectLegalHold(ctx context.Context, in *s3.PutObjectLegalHoldInput, _ ...func(*s3.Options)) (*s3.PutObjectLegalHoldOutput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	defer b.mutate()()
	bkt, f := b.getBucket(in.Bucket)
	if f != nil {
		return nil, f.err()
	}
	if bkt.lock == nil {
		return nil, missingLockConfiguration().err()
	}
	o, f := bkt.find(in.Key, in.VersionId)
	if f != nil {
		return nil, f.err()
	}
	h := in.LegalHold
	if h == nil || (h.Status != types.ObjectLockLegalHoldStatusOn && h.Status != types.ObjectLockLegalHoldStatusOff) {
		return nil, malformedXML().err()
	}
	o.legalHold = h.Status
	return &s3.PutObjectLegalHoldOutput{}, nil
}
