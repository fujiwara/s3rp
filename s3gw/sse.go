package s3gw

import (
	"net/http"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp/s3err"
)

// SSE-S3 and SSE-KMS pass through to the backend, which performs the actual
// encryption; the KMS key id is an opaque name resolved by the backend's KMS
// (e.g. Ceph RGW handing it to a Vault-compatible service), so the gateway
// forwards it untouched and exposes it on Op for the service to authorize —
// whether a tenant may use a key id is the service's decision, like a quota.
// A KMSKeyMapper (intercept.go) lets the service keep the backend's key
// namespace out of the API altogether, mapping ids in both directions.
//
// SSE-C is refused loudly instead of being silently dropped: an ignored
// customer key would store the object without the encryption the client
// believes it requested, and later serve it back without the key.

const (
	hdrSSE          = "x-amz-server-side-encryption"
	hdrSSEKMSKeyID  = "x-amz-server-side-encryption-aws-kms-key-id"
	hdrSSECAlgo     = "x-amz-server-side-encryption-customer-algorithm"
	hdrCopySSECAlgo = "x-amz-copy-source-server-side-encryption-customer-algorithm"
)

// checkSSEC rejects SSE-C requests. The algorithm header is what activates
// SSE-C on AWS; the key headers without it are inert there too. Attribute,
// not Signed, and inside this function rather than chosen by the caller:
// a refusal must see the value signed or not — treating an unsigned SSE-C
// header as absent would be the silent drop this refusal exists to prevent.
func checkSSEC(hdr signedHeader) *s3err.Error {
	if hdr.Attribute(hdrSSECAlgo) != "" || hdr.Attribute(hdrCopySSECAlgo) != "" {
		return s3err.NotImplemented("SSE-C")
	}
	return nil
}

// checkSSE validates the SSE request fields without mapping them. It runs
// before the hooks, so Op.SSE only ever carries a supported value. Signed,
// because what is validated is what applySSE will honor. POST uploads run
// the same check over their form fields via postFieldHeader.
func checkSSE(hdr signedHeader) *s3err.Error {
	v := hdr.Signed(hdrSSE)
	switch v {
	case "", "AES256", "aws:kms":
	default:
		return s3err.New(http.StatusBadRequest, "InvalidArgument",
			"The encryption method specified is not supported")
	}
	if hdr.Signed(hdrSSEKMSKeyID) != "" && v != "aws:kms" {
		return s3err.New(http.StatusBadRequest, "InvalidArgument",
			hdrSSEKMSKeyID+" can only be used with aws:kms")
	}
	return nil
}

// applySSE maps the SSE request fields onto an upload operation's input,
// validating them for the paths that do not pass through dispatch. The KMS
// key id goes through the KMSKeyMapper when one is installed: for an
// aws:kms write the mapper's answer replaces the client's id entirely.
func (c *opCtx) applySSE(hdr signedHeader, enc *types.ServerSideEncryption, kmsKeyID **string) *s3err.Error {
	if s3e := checkSSE(hdr); s3e != nil {
		return s3e
	}
	v := hdr.Signed(hdrSSE)
	if v != "" {
		*enc = types.ServerSideEncryption(v)
	}
	id := hdr.Signed(hdrSSEKMSKeyID)
	if v == "aws:kms" && c.g.kmsKey != nil {
		id = c.g.kmsKey.ToBackend(c.op, id)
	}
	if id != "" {
		*kmsKeyID = aws.String(id)
	}
	return nil
}

// clientKMSKeyID is what the client is shown for a key id the backend
// reported: nil when the backend named none, the mapper's answer (nil for
// "") when one is installed, the backend's id otherwise.
func (c *opCtx) clientKMSKeyID(keyID *string) *string {
	if keyID == nil || c.g.kmsKey == nil {
		return keyID
	}
	if v := c.g.kmsKey.ToClient(c.op, *keyID); v != "" {
		return aws.String(v)
	}
	return nil
}

// setSSEHeaders reports the backend's encryption result to the client. The
// key id is the same opaque name the client sent, not a backend secret.
func setSSEHeaders(h http.Header, enc types.ServerSideEncryption, kmsKeyID *string) {
	if enc != "" {
		h.Set(hdrSSE, string(enc))
	}
	if kmsKeyID != nil {
		h.Set(hdrSSEKMSKeyID, *kmsKeyID)
	}
}
