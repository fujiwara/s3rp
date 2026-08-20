package s3gw

import (
	"net/http"

	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// s3rp mimics buckets with ACLs disabled (Object Ownership = bucket owner
// enforced), which is the AWS default since 2023 and the recommended mode.
// GetBucketAcl / GetObjectAcl return a fixed full-control-by-owner policy,
// modifying ACLs is rejected, and only canned ACLs that are no-ops on an
// ACL-disabled bucket are accepted on uploads.

func errACLNotSupported() *s3err.Error {
	return s3err.New(http.StatusBadRequest, "AccessControlListNotSupported",
		"The bucket does not allow ACLs")
}

// checkACLHeader rejects canned ACLs other than the ones an ACL-disabled
// bucket accepts. Read as an attribute so the refusal sees the value signed
// or not — a refusal check must never treat a header as absent.
func checkACLHeader(hdr signedHeader) *s3err.Error {
	switch hdr.Attribute("x-amz-acl") {
	case "", "private", "bucket-owner-full-control":
		return nil
	}
	return errACLNotSupported()
}

func ownerFullControlPolicy(owner string) *s3xml.AccessControlPolicy {
	policy := &s3xml.AccessControlPolicy{
		XMLNS: s3xml.Namespace,
		Owner: s3xml.Owner{ID: owner, DisplayName: owner},
	}
	policy.AccessControlList.Grants = []s3xml.Grant{
		{
			Grantee: s3xml.Grantee{
				XMLNSXSI:    "http://www.w3.org/2001/XMLSchema-instance",
				Type:        "CanonicalUser",
				ID:          owner,
				DisplayName: owner,
			},
			Permission: "FULL_CONTROL",
		},
	}
	return policy
}

func (g *Gateway) getBucketACL(c *opCtx) error {
	// the owner is the bucket's tenant, not the requester's — they differ
	// for a cross-tenant request
	return s3xml.Write(c.w, ownerFullControlPolicy(c.rt.cfg.Tenant))
}

func (g *Gateway) getObjectACL(c *opCtx) error {
	w, r, rt, key := c.w, c.r, c.rt, c.key
	// verify the object exists so that a missing key still errors
	in := &s3.HeadObjectInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
		Key:    aws.String(key),
	}
	if v := r.URL.Query().Get(qpVersionID); v != "" {
		in.VersionId = aws.String(v)
	}
	if _, err := rt.client.HeadObject(r.Context(), in); err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	return s3xml.Write(w, ownerFullControlPolicy(rt.cfg.Tenant))
}
