package s3rp

import "github.com/fujiwara/s3rp/s3err"

// The S3 error representation and the aws-sdk error mapping live in s3err so
// that they can be reused independently of this proxy. These aliases keep the
// operation layer's call sites unqualified: that layer is the part a real
// service would restructure anyway, so there is nothing to gain from churning
// it here.

// S3Error is an S3 API error response.
type S3Error = s3err.Error

var (
	newS3Error   = s3err.New
	fromSDKError = s3err.FromSDKError
	writeS3Error = s3err.Write
	newRequestID = s3err.NewRequestID

	errNotImplemented        = s3err.NotImplemented
	errAccessDenied          = s3err.AccessDenied
	errInvalidAccessKeyID    = s3err.InvalidAccessKeyID
	errSignatureDoesNotMatch = s3err.SignatureDoesNotMatch
	errContentSHA256Mismatch = s3err.ContentSHA256Mismatch
)
