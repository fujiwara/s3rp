package s3gw

import (
	"context"
	"fmt"

	"github.com/fujiwara/s3rp/store"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// BackendClient is the narrow interface of the S3 client methods s3rp uses,
// for injecting stubs in tests.
type BackendClient interface {
	GetObject(ctx context.Context, in *s3.GetObjectInput, optFns ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	PutObject(ctx context.Context, in *s3.PutObjectInput, optFns ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, optFns ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
	HeadBucket(ctx context.Context, in *s3.HeadBucketInput, optFns ...func(*s3.Options)) (*s3.HeadBucketOutput, error)
	CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error)
	UploadPart(ctx context.Context, in *s3.UploadPartInput, optFns ...func(*s3.Options)) (*s3.UploadPartOutput, error)
	CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error)
	AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, optFns ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error)
	ListParts(ctx context.Context, in *s3.ListPartsInput, optFns ...func(*s3.Options)) (*s3.ListPartsOutput, error)
	ListMultipartUploads(ctx context.Context, in *s3.ListMultipartUploadsInput, optFns ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error)
	ListObjects(ctx context.Context, in *s3.ListObjectsInput, optFns ...func(*s3.Options)) (*s3.ListObjectsOutput, error)
	DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error)
	CopyObject(ctx context.Context, in *s3.CopyObjectInput, optFns ...func(*s3.Options)) (*s3.CopyObjectOutput, error)
	UploadPartCopy(ctx context.Context, in *s3.UploadPartCopyInput, optFns ...func(*s3.Options)) (*s3.UploadPartCopyOutput, error)
	GetBucketVersioning(ctx context.Context, in *s3.GetBucketVersioningInput, optFns ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error)
	PutBucketVersioning(ctx context.Context, in *s3.PutBucketVersioningInput, optFns ...func(*s3.Options)) (*s3.PutBucketVersioningOutput, error)
	ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, optFns ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error)
	GetObjectTagging(ctx context.Context, in *s3.GetObjectTaggingInput, optFns ...func(*s3.Options)) (*s3.GetObjectTaggingOutput, error)
	PutObjectTagging(ctx context.Context, in *s3.PutObjectTaggingInput, optFns ...func(*s3.Options)) (*s3.PutObjectTaggingOutput, error)
	DeleteObjectTagging(ctx context.Context, in *s3.DeleteObjectTaggingInput, optFns ...func(*s3.Options)) (*s3.DeleteObjectTaggingOutput, error)
	GetObjectLockConfiguration(ctx context.Context, in *s3.GetObjectLockConfigurationInput, optFns ...func(*s3.Options)) (*s3.GetObjectLockConfigurationOutput, error)
	PutObjectLockConfiguration(ctx context.Context, in *s3.PutObjectLockConfigurationInput, optFns ...func(*s3.Options)) (*s3.PutObjectLockConfigurationOutput, error)
	GetObjectRetention(ctx context.Context, in *s3.GetObjectRetentionInput, optFns ...func(*s3.Options)) (*s3.GetObjectRetentionOutput, error)
	PutObjectRetention(ctx context.Context, in *s3.PutObjectRetentionInput, optFns ...func(*s3.Options)) (*s3.PutObjectRetentionOutput, error)
	GetObjectLegalHold(ctx context.Context, in *s3.GetObjectLegalHoldInput, optFns ...func(*s3.Options)) (*s3.GetObjectLegalHoldOutput, error)
	PutObjectLegalHold(ctx context.Context, in *s3.PutObjectLegalHoldInput, optFns ...func(*s3.Options)) (*s3.PutObjectLegalHoldOutput, error)
}

func newBackendClient(ctx context.Context, b *store.Backend, clientOptions func(*store.Backend) []func(*s3.Options)) (BackendClient, error) {
	optFns := []func(*awsconfig.LoadOptions) error{
		awsconfig.WithRegion(b.Region),
	}
	if b.AccessKeyID != "" {
		optFns = append(optFns, awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(b.AccessKeyID, b.SecretAccessKey.String(), ""),
		))
	} // otherwise the SDK default credential chain is used
	cfg, err := awsconfig.LoadDefaultConfig(ctx, optFns...)
	if err != nil {
		return nil, fmt.Errorf("failed to load aws config: %w", err)
	}
	var extra []func(*s3.Options)
	if clientOptions != nil {
		extra = clientOptions(b)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		if b.Endpoint != "" {
			o.BaseEndpoint = aws.String(b.Endpoint)
		} // otherwise the SDK resolves the AWS S3 endpoint from the region
		// nil-safe: a Store is expected to have applied Backend.SetDefaults,
		// but never panic in the request path if one did not
		o.UsePathStyle = b.UsePathStyle != nil && *b.UsePathStyle
		// the request body from the client is not seekable, so the SDK
		// cannot compute a payload hash over plain http endpoints
		o.APIOptions = append(o.APIOptions, v4.SwapComputePayloadSHA256ForUnsignedPayloadMiddleware)
		// default CRC32 trailer checksums re-introduce aws-chunked
		// encoding, which some S3-compatible backends reject
		o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		o.ResponseChecksumValidation = aws.ResponseChecksumValidationWhenRequired
		// the service's options come last, so they can adjust or knowingly
		// override the defaults above
		for _, fn := range extra {
			fn(o)
		}
	})
	return client, nil
}
