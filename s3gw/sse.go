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
// SSE-C on AWS; the key headers without it are inert there too.
func checkSSEC(r *http.Request) *s3err.Error {
	if r.Header.Get(hdrSSECAlgo) != "" || r.Header.Get(hdrCopySSECAlgo) != "" {
		return s3err.NotImplemented("SSE-C")
	}
	return nil
}

// applySSE maps the SSE request fields onto an upload operation's input.
// get looks a field up by its lower-case header name, so the same mapping
// serves headers (case-insensitive by http.Header) and POST form fields.
func applySSE(get func(string) string, enc *types.ServerSideEncryption, kmsKeyID **string) *s3err.Error {
	v := get(hdrSSE)
	switch v {
	case "":
	case "AES256", "aws:kms":
		*enc = types.ServerSideEncryption(v)
	default:
		return s3err.New(http.StatusBadRequest, "InvalidArgument",
			"The encryption method specified is not supported")
	}
	if id := get(hdrSSEKMSKeyID); id != "" {
		if v != "aws:kms" {
			return s3err.New(http.StatusBadRequest, "InvalidArgument",
				hdrSSEKMSKeyID+" can only be used with aws:kms")
		}
		*kmsKeyID = aws.String(id)
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
