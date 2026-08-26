package s3gw_test

import (
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// stubBackend implements s3gw.BackendClient recording inputs and returning
// canned outputs.
type stubBackend struct {
	getIn  *s3.GetObjectInput
	getOut *s3.GetObjectOutput
	getErr error

	getAttrIn  *s3.GetObjectAttributesInput
	getAttrOut *s3.GetObjectAttributesOutput
	getAttrErr error
	putIn      *s3.PutObjectInput
	putBody    []byte
	putOut     *s3.PutObjectOutput
	headIn     *s3.HeadObjectInput
	headOut    *s3.HeadObjectOutput
	headErr    error
	delIn      *s3.DeleteObjectInput
	delOut     *s3.DeleteObjectOutput
	listIn     *s3.ListObjectsV2Input
	listOut    *s3.ListObjectsV2Output
	hbIn       *s3.HeadBucketInput
	hbOut      *s3.HeadBucketOutput

	createMPUIn    *s3.CreateMultipartUploadInput
	createMPUOut   *s3.CreateMultipartUploadOutput
	uploadPartIns  []*s3.UploadPartInput
	uploadPartData [][]byte
	completeMPUIn  *s3.CompleteMultipartUploadInput
	completeMPUOut *s3.CompleteMultipartUploadOutput
	abortMPUIn     *s3.AbortMultipartUploadInput
	listPartsIn    *s3.ListPartsInput
	listPartsOut   *s3.ListPartsOutput
	listMPUIn      *s3.ListMultipartUploadsInput
	listMPUOut     *s3.ListMultipartUploadsOutput

	listV1In   *s3.ListObjectsInput
	listV1Out  *s3.ListObjectsOutput
	getTagIn   *s3.GetObjectTaggingInput
	getTagOut  *s3.GetObjectTaggingOutput
	putTagIn   *s3.PutObjectTaggingInput
	delTagIn   *s3.DeleteObjectTaggingInput
	getVerIn   *s3.GetBucketVersioningInput
	getVerOut  *s3.GetBucketVersioningOutput
	getEncIn   *s3.GetBucketEncryptionInput
	getEncOut  *s3.GetBucketEncryptionOutput
	getEncErr  error
	listVerIn  *s3.ListObjectVersionsInput
	listVerOut *s3.ListObjectVersionsOutput

	getLockCfgIn    *s3.GetObjectLockConfigurationInput
	getLockCfgOut   *s3.GetObjectLockConfigurationOutput
	getRetentionIn  *s3.GetObjectRetentionInput
	getRetentionOut *s3.GetObjectRetentionOutput
	putRetentionIn  *s3.PutObjectRetentionInput
	getLegalHoldIn  *s3.GetObjectLegalHoldInput
	getLegalHoldOut *s3.GetObjectLegalHoldOutput
	putLegalHoldIn  *s3.PutObjectLegalHoldInput
	delObjsIn       *s3.DeleteObjectsInput
	delObjsOut      *s3.DeleteObjectsOutput
	copyIn          *s3.CopyObjectInput
	copyOut         *s3.CopyObjectOutput
	upcIn           *s3.UploadPartCopyInput
	upcOut          *s3.UploadPartCopyOutput
}

func (b *stubBackend) GetObject(ctx context.Context, in *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	b.getIn = in
	if b.getErr != nil {
		return nil, b.getErr
	}
	return b.getOut, nil
}

func (b *stubBackend) GetObjectAttributes(ctx context.Context, in *s3.GetObjectAttributesInput, _ ...func(*s3.Options)) (*s3.GetObjectAttributesOutput, error) {
	b.getAttrIn = in
	if b.getAttrErr != nil {
		return nil, b.getAttrErr
	}
	return b.getAttrOut, nil
}

func (b *stubBackend) PutObject(ctx context.Context, in *s3.PutObjectInput, _ ...func(*s3.Options)) (*s3.PutObjectOutput, error) {
	b.putIn = in
	body, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	b.putBody = body
	return b.putOut, nil
}

func (b *stubBackend) HeadObject(ctx context.Context, in *s3.HeadObjectInput, _ ...func(*s3.Options)) (*s3.HeadObjectOutput, error) {
	b.headIn = in
	if b.headErr != nil {
		return nil, b.headErr
	}
	return b.headOut, nil
}

func (b *stubBackend) DeleteObject(ctx context.Context, in *s3.DeleteObjectInput, _ ...func(*s3.Options)) (*s3.DeleteObjectOutput, error) {
	b.delIn = in
	return b.delOut, nil
}

func (b *stubBackend) ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	b.listIn = in
	return b.listOut, nil
}

func (b *stubBackend) HeadBucket(ctx context.Context, in *s3.HeadBucketInput, _ ...func(*s3.Options)) (*s3.HeadBucketOutput, error) {
	b.hbIn = in
	return b.hbOut, nil
}

func (b *stubBackend) CreateMultipartUpload(ctx context.Context, in *s3.CreateMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CreateMultipartUploadOutput, error) {
	b.createMPUIn = in
	return b.createMPUOut, nil
}

func (b *stubBackend) UploadPart(ctx context.Context, in *s3.UploadPartInput, _ ...func(*s3.Options)) (*s3.UploadPartOutput, error) {
	b.uploadPartIns = append(b.uploadPartIns, in)
	data, err := io.ReadAll(in.Body)
	if err != nil {
		return nil, err
	}
	b.uploadPartData = append(b.uploadPartData, data)
	return &s3.UploadPartOutput{ETag: aws.String(fmt.Sprintf(`"part-etag-%d"`, len(b.uploadPartIns)))}, nil
}

func (b *stubBackend) CompleteMultipartUpload(ctx context.Context, in *s3.CompleteMultipartUploadInput, _ ...func(*s3.Options)) (*s3.CompleteMultipartUploadOutput, error) {
	b.completeMPUIn = in
	return b.completeMPUOut, nil
}

func (b *stubBackend) AbortMultipartUpload(ctx context.Context, in *s3.AbortMultipartUploadInput, _ ...func(*s3.Options)) (*s3.AbortMultipartUploadOutput, error) {
	b.abortMPUIn = in
	return &s3.AbortMultipartUploadOutput{}, nil
}

func (b *stubBackend) ListParts(ctx context.Context, in *s3.ListPartsInput, _ ...func(*s3.Options)) (*s3.ListPartsOutput, error) {
	b.listPartsIn = in
	return b.listPartsOut, nil
}

func (b *stubBackend) ListMultipartUploads(ctx context.Context, in *s3.ListMultipartUploadsInput, _ ...func(*s3.Options)) (*s3.ListMultipartUploadsOutput, error) {
	b.listMPUIn = in
	return b.listMPUOut, nil
}

func (b *stubBackend) ListObjects(ctx context.Context, in *s3.ListObjectsInput, _ ...func(*s3.Options)) (*s3.ListObjectsOutput, error) {
	b.listV1In = in
	return b.listV1Out, nil
}

func (b *stubBackend) DeleteObjects(ctx context.Context, in *s3.DeleteObjectsInput, _ ...func(*s3.Options)) (*s3.DeleteObjectsOutput, error) {
	b.delObjsIn = in
	return b.delObjsOut, nil
}

func (b *stubBackend) CopyObject(ctx context.Context, in *s3.CopyObjectInput, _ ...func(*s3.Options)) (*s3.CopyObjectOutput, error) {
	b.copyIn = in
	return b.copyOut, nil
}

func (b *stubBackend) UploadPartCopy(ctx context.Context, in *s3.UploadPartCopyInput, _ ...func(*s3.Options)) (*s3.UploadPartCopyOutput, error) {
	b.upcIn = in
	return b.upcOut, nil
}

func (b *stubBackend) GetBucketVersioning(ctx context.Context, in *s3.GetBucketVersioningInput, _ ...func(*s3.Options)) (*s3.GetBucketVersioningOutput, error) {
	b.getVerIn = in
	return b.getVerOut, nil
}

func (b *stubBackend) GetBucketEncryption(ctx context.Context, in *s3.GetBucketEncryptionInput, _ ...func(*s3.Options)) (*s3.GetBucketEncryptionOutput, error) {
	b.getEncIn = in
	return b.getEncOut, b.getEncErr
}

func (b *stubBackend) ListObjectVersions(ctx context.Context, in *s3.ListObjectVersionsInput, _ ...func(*s3.Options)) (*s3.ListObjectVersionsOutput, error) {
	b.listVerIn = in
	return b.listVerOut, nil
}

func (b *stubBackend) GetObjectTagging(ctx context.Context, in *s3.GetObjectTaggingInput, _ ...func(*s3.Options)) (*s3.GetObjectTaggingOutput, error) {
	b.getTagIn = in
	return b.getTagOut, nil
}

func (b *stubBackend) PutObjectTagging(ctx context.Context, in *s3.PutObjectTaggingInput, _ ...func(*s3.Options)) (*s3.PutObjectTaggingOutput, error) {
	b.putTagIn = in
	return &s3.PutObjectTaggingOutput{}, nil
}

func (b *stubBackend) DeleteObjectTagging(ctx context.Context, in *s3.DeleteObjectTaggingInput, _ ...func(*s3.Options)) (*s3.DeleteObjectTaggingOutput, error) {
	b.delTagIn = in
	return &s3.DeleteObjectTaggingOutput{}, nil
}

func (b *stubBackend) GetObjectLockConfiguration(ctx context.Context, in *s3.GetObjectLockConfigurationInput, _ ...func(*s3.Options)) (*s3.GetObjectLockConfigurationOutput, error) {
	b.getLockCfgIn = in
	return b.getLockCfgOut, nil
}

func (b *stubBackend) GetObjectRetention(ctx context.Context, in *s3.GetObjectRetentionInput, _ ...func(*s3.Options)) (*s3.GetObjectRetentionOutput, error) {
	b.getRetentionIn = in
	return b.getRetentionOut, nil
}

func (b *stubBackend) PutObjectRetention(ctx context.Context, in *s3.PutObjectRetentionInput, _ ...func(*s3.Options)) (*s3.PutObjectRetentionOutput, error) {
	b.putRetentionIn = in
	return &s3.PutObjectRetentionOutput{}, nil
}

func (b *stubBackend) GetObjectLegalHold(ctx context.Context, in *s3.GetObjectLegalHoldInput, _ ...func(*s3.Options)) (*s3.GetObjectLegalHoldOutput, error) {
	b.getLegalHoldIn = in
	return b.getLegalHoldOut, nil
}

func (b *stubBackend) PutObjectLegalHold(ctx context.Context, in *s3.PutObjectLegalHoldInput, _ ...func(*s3.Options)) (*s3.PutObjectLegalHoldOutput, error) {
	b.putLegalHoldIn = in
	return &s3.PutObjectLegalHoldOutput{}, nil
}
