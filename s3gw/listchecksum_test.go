package s3gw_test

import (
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/go-cmp/cmp"
)

// The listings report each entry's checksum algorithm and type as the
// backend does, so a client can tell how an object is protected without a
// HEAD per key; ListParts also carries each part's checksum.
func TestListingsCarryChecksums(t *testing.T) {
	modified := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	algs := []types.ChecksumAlgorithm{types.ChecksumAlgorithmCrc64nvme}
	stub := &stubBackend{
		listOut: &s3.ListObjectsV2Output{Contents: []types.Object{{
			Key: aws.String("v2"), LastModified: aws.Time(modified), ChecksumAlgorithm: algs, ChecksumType: types.ChecksumTypeFullObject,
		}}},
		listV1Out: &s3.ListObjectsOutput{Contents: []types.Object{{
			Key: aws.String("v1"), LastModified: aws.Time(modified), ChecksumAlgorithm: algs, ChecksumType: types.ChecksumTypeFullObject,
		}}},
		listVerOut: &s3.ListObjectVersionsOutput{Versions: []types.ObjectVersion{{
			Key: aws.String("ver"), VersionId: aws.String("v"),
			ChecksumAlgorithm: []types.ChecksumAlgorithm{types.ChecksumAlgorithmSha256}, ChecksumType: types.ChecksumTypeComposite,
		}}},
		listMPUOut: &s3.ListMultipartUploadsOutput{Uploads: []types.MultipartUpload{{
			Key: aws.String("mpu"), UploadId: aws.String("u"),
			ChecksumAlgorithm: types.ChecksumAlgorithmCrc32c, ChecksumType: types.ChecksumTypeFullObject,
		}}},
		listPartsOut: &s3.ListPartsOutput{
			ChecksumAlgorithm: types.ChecksumAlgorithmSha1,
			ChecksumType:      types.ChecksumTypeComposite,
			Parts: []types.Part{{
				PartNumber:        aws.Int32(1),
				ChecksumCRC32:     aws.String("crc32"),
				ChecksumCRC32C:    aws.String("crc32c"),
				ChecksumCRC64NVME: aws.String("crc64nvme"),
				ChecksumSHA1:      aws.String("sha1"),
				ChecksumSHA256:    aws.String("sha256"),
			}},
		},
	}
	client, _ := newTestProxy(t, stub)
	ctx := t.Context()
	bucket := aws.String("testbucket")

	v2, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if o := v2.Contents[0]; !cmp.Equal(o.ChecksumAlgorithm, algs) || o.ChecksumType != types.ChecksumTypeFullObject {
		t.Errorf("ListObjectsV2: %v %q", o.ChecksumAlgorithm, o.ChecksumType)
	}

	v1, err := client.ListObjects(ctx, &s3.ListObjectsInput{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if o := v1.Contents[0]; !cmp.Equal(o.ChecksumAlgorithm, algs) || o.ChecksumType != types.ChecksumTypeFullObject {
		t.Errorf("ListObjects: %v %q", o.ChecksumAlgorithm, o.ChecksumType)
	}

	ver, err := client.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if v := ver.Versions[0]; !cmp.Equal(v.ChecksumAlgorithm, []types.ChecksumAlgorithm{types.ChecksumAlgorithmSha256}) ||
		v.ChecksumType != types.ChecksumTypeComposite {
		t.Errorf("ListObjectVersions: %v %q", v.ChecksumAlgorithm, v.ChecksumType)
	}

	mpu, err := client.ListMultipartUploads(ctx, &s3.ListMultipartUploadsInput{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if u := mpu.Uploads[0]; u.ChecksumAlgorithm != types.ChecksumAlgorithmCrc32c || u.ChecksumType != types.ChecksumTypeFullObject {
		t.Errorf("ListMultipartUploads: %q %q", u.ChecksumAlgorithm, u.ChecksumType)
	}

	parts, err := client.ListParts(ctx, &s3.ListPartsInput{Bucket: bucket, Key: aws.String("mpu"), UploadId: aws.String("u")})
	if err != nil {
		t.Fatal(err)
	}
	if parts.ChecksumAlgorithm != types.ChecksumAlgorithmSha1 || parts.ChecksumType != types.ChecksumTypeComposite {
		t.Errorf("ListParts: %q %q", parts.ChecksumAlgorithm, parts.ChecksumType)
	}
	p := parts.Parts[0]
	got := []string{aws.ToString(p.ChecksumCRC32), aws.ToString(p.ChecksumCRC32C), aws.ToString(p.ChecksumCRC64NVME),
		aws.ToString(p.ChecksumSHA1), aws.ToString(p.ChecksumSHA256)}
	if diff := cmp.Diff([]string{"crc32", "crc32c", "crc64nvme", "sha1", "sha256"}, got); diff != "" {
		t.Errorf("ListParts part checksums (-want +got):\n%s", diff)
	}

	// an entry the backend reports no checksum for carries none
	stub.listOut = &s3.ListObjectsV2Output{Contents: []types.Object{{Key: aws.String("plain"), LastModified: aws.Time(modified)}}}
	plain, err := client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: bucket})
	if err != nil {
		t.Fatal(err)
	}
	if o := plain.Contents[0]; len(o.ChecksumAlgorithm) != 0 || o.ChecksumType != "" {
		t.Errorf("expect no checksum on a plain entry, got %v %q", o.ChecksumAlgorithm, o.ChecksumType)
	}
}
