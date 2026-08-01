package s3rp

import (
	"net/http"

	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/store"
)

// authorize evaluates the bucket policy for the authenticated user
// performing an action on a resource ("bucket" or "bucket/key").
//
// The baseline is that every user of a tenant has full access to the
// tenant's buckets; an explicit Deny in the bucket policy restricts it.
// Allow statements are accepted but have no effect yet — they will become
// meaningful with anonymous and cross-tenant access.
func (app *S3RP) authorize(vr *verifiedRequest, b *store.Bucket, action, resource string) *S3Error {
	// the user's identity policy gates the action first (default allow all);
	// then the bucket policy may add a resource-specific Deny.
	if !vr.UserPolicy.Allows(action) {
		return errAccessDenied()
	}
	if b.Policy != nil && b.Policy.Evaluate(vr.User, action, resource) == policy.Deny {
		return errAccessDenied()
	}
	return nil
}

// getBucketPolicy returns the raw bucket policy JSON.
func (app *S3RP) getBucketPolicy(c *opCtx) error {
	w, rt := c.w, c.rt
	if rt.cfg.PolicyText == "" {
		return newS3Error(http.StatusNotFound, "NoSuchBucketPolicy",
			"The bucket policy does not exist")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(rt.cfg.PolicyText))
	return nil
}
