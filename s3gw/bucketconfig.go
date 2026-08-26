package s3gw

import (
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3xml"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Bucket-configuration reads that follow from the gateway's own model
// rather than from the backend. Tools that inspect a bucket (consoles,
// Terraform, auditors) call these alongside the ACL read and treat a 501
// as a broken bucket, so they answer what the model guarantees:
// anonymous access does not exist, so no bucket is public, and ACLs are
// disabled, so ownership is bucket-owner enforced. The writes stay
// NotImplemented like every other bucket-configuration write.

func (g *Gateway) getBucketPolicyStatus(c *opCtx) error {
	return s3xml.Write(c.w, &s3xml.PolicyStatus{XMLNS: s3xml.Namespace, IsPublic: false})
}

func (g *Gateway) getBucketOwnershipControls(c *opCtx) error {
	return s3xml.Write(c.w, &s3xml.OwnershipControls{
		XMLNS: s3xml.Namespace,
		Rules: []s3xml.OwnershipControlsRule{{ObjectOwnership: "BucketOwnerEnforced"}},
	})
}

func (g *Gateway) getPublicAccessBlock(c *opCtx) error {
	return s3xml.Write(c.w, &s3xml.PublicAccessBlockConfiguration{
		XMLNS:                 s3xml.Namespace,
		BlockPublicAcls:       true,
		IgnorePublicAcls:      true,
		BlockPublicPolicy:     true,
		RestrictPublicBuckets: true,
	})
}

// getBucketEncryption is proxied like GetBucketVersioning: the default
// encryption is backend bucket configuration, and the KMS key id it names
// is the same opaque name SSE-KMS requests carry.
func (g *Gateway) getBucketEncryption(c *opCtx) error {
	w, r, rt := c.w, c.r, c.rt
	out, err := rt.client.GetBucketEncryption(r.Context(), &s3.GetBucketEncryptionInput{
		Bucket: aws.String(rt.cfg.Backend.Bucket),
	})
	if err != nil {
		return s3err.FromSDKError(err, r.URL.Path)
	}
	result := &s3xml.ServerSideEncryptionConfiguration{XMLNS: s3xml.Namespace}
	if out.ServerSideEncryptionConfiguration != nil {
		for _, rule := range out.ServerSideEncryptionConfiguration.Rules {
			x := s3xml.ServerSideEncryptionRule{BucketKeyEnabled: rule.BucketKeyEnabled}
			if d := rule.ApplyServerSideEncryptionByDefault; d != nil {
				x.ApplyServerSideEncryptionByDefault = &s3xml.ServerSideEncryptionByDefault{
					SSEAlgorithm:   string(d.SSEAlgorithm),
					KMSMasterKeyID: aws.ToString(d.KMSMasterKeyID),
				}
			}
			result.Rules = append(result.Rules, x)
		}
	}
	return s3xml.Write(w, result)
}
