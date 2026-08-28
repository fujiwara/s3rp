package sigv4

import "fmt"

// AuthFailure is the cause behind a refused authentication: what failed and
// the access key id the request presented, when it named one. It is
// attached to the *s3err.Error a Verifier returns (errors.As on the error's
// cause) so an operator's log can tell, for a request that never proved an
// identity, which key was tried and why it was refused — the client is told
// only the S3 error code and message. The key id is an identifier, not a
// secret, and is recorded unverified: it is what the request claimed.
type AuthFailure struct {
	// Reason is one of the Reason* constants.
	Reason string `json:"reason"`
	// AccessKeyID is the id the request presented, empty when the request
	// carried no credential at all.
	AccessKeyID string `json:"access_key_id,omitempty"`
	// Detail is a human-readable elaboration (which headers were unsigned,
	// what the signed-header sets were), never a secret.
	Detail string `json:"detail,omitempty"`

	err error // the lookup's own error, for unknown-key and invalid-token
}

const (
	// ReasonNoAuth: neither an Authorization header nor presigned query
	// parameters.
	ReasonNoAuth = "no-auth"
	// ReasonUnknownKey: the lookup does not know the access key id.
	ReasonUnknownKey = "unknown-key"
	// ReasonInvalidToken: the session token failed validation or did not
	// match the key's, or a token was presented for a long-lived key.
	ReasonInvalidToken = "invalid-token"
	// ReasonSignatureMismatch: the re-signed request differs from what the
	// client signed (wrong secret, or a request altered in transit).
	ReasonSignatureMismatch = "signature-mismatch"
	// ReasonUnsignedHeaders: an x-amz-* header was present but not covered
	// by the signature.
	ReasonUnsignedHeaders = "unsigned-headers"
	// ReasonExpired: a presigned URL past X-Amz-Expires, or a POST policy
	// past its expiration.
	ReasonExpired = "expired"
	// ReasonNotYetValid: a signing time in the future beyond the allowed
	// skew.
	ReasonNotYetValid = "not-yet-valid"
	// ReasonTimeSkewed: the signing time is too far from the server's.
	ReasonTimeSkewed = "time-skewed"
	// ReasonPostPolicy: a POST upload's form did not satisfy its policy
	// (Detail says which condition).
	ReasonPostPolicy = "post-policy"
)

func (f *AuthFailure) Error() string {
	s := f.Reason
	if f.AccessKeyID != "" {
		s += " (access key " + f.AccessKeyID + ")"
	}
	if f.Detail != "" {
		s += ": " + f.Detail
	}
	if f.err != nil {
		s += fmt.Sprintf(": %v", f.err)
	}
	return s
}

// Unwrap exposes the lookup's error (ErrUnknownKey, ErrInvalidToken, and
// whatever the store wrapped them with).
func (f *AuthFailure) Unwrap() error { return f.err }

func authFailure(reason, akid, detail string) *AuthFailure {
	return &AuthFailure{Reason: reason, AccessKeyID: akid, Detail: detail}
}
