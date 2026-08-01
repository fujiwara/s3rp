package s3rp_test

import (
	"errors"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp"
)

func TestProxyListObjectsV1(t *testing.T) {
	stub := &stubBackend{
		listV1Out: &s3.ListObjectsOutput{
			IsTruncated: aws.Bool(true),
			MaxKeys:     aws.Int32(1),
			NextMarker:  aws.String("dir/a.txt"),
			Contents: []types.Object{
				{
					Key:          aws.String("dir/a.txt"),
					Size:         aws.Int64(10),
					ETag:         aws.String(`"etag-a"`),
					LastModified: aws.Time(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
				},
			},
		},
	}
	client, _ := newTestProxy(t, stub)
	out, err := client.ListObjects(t.Context(), &s3.ListObjectsInput{
		Bucket:  aws.String("testbucket"),
		Prefix:  aws.String("dir/"),
		MaxKeys: aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.Name) != "testbucket" {
		t.Errorf("expect testbucket, got %s", aws.ToString(out.Name))
	}
	if len(out.Contents) != 1 || aws.ToString(out.Contents[0].Key) != "dir/a.txt" {
		t.Errorf("unexpected contents %v", out.Contents)
	}
	if aws.ToString(out.NextMarker) != "dir/a.txt" {
		t.Errorf("unexpected next marker %v", out.NextMarker)
	}
	if aws.ToString(stub.listV1In.Bucket) != "backend-testbucket" {
		t.Errorf("expect backend-testbucket, got %s", aws.ToString(stub.listV1In.Bucket))
	}
}

func TestProxyGetBucketLocation(t *testing.T) {
	// newTestApp uses the default region us-east-1, represented as empty
	client, _ := newTestProxy(t, &stubBackend{})
	out, err := client.GetBucketLocation(t.Context(), &s3.GetBucketLocationInput{
		Bucket: aws.String("testbucket"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.LocationConstraint != "" {
		t.Errorf("expect empty location, got %s", out.LocationConstraint)
	}
}

func TestProxyDeleteObjects(t *testing.T) {
	stub := &stubBackend{
		delObjsOut: &s3.DeleteObjectsOutput{
			Deleted: []types.DeletedObject{
				{Key: aws.String("a.txt")},
				{Key: aws.String("b.txt")},
			},
			Errors: []types.Error{
				{Key: aws.String("c.txt"), Code: aws.String("AccessDenied"), Message: aws.String("Access Denied")},
			},
		},
	}
	client, _ := newTestProxy(t, stub)
	out, err := client.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
		Bucket: aws.String("testbucket"),
		Delete: &types.Delete{
			Objects: []types.ObjectIdentifier{
				{Key: aws.String("a.txt")},
				{Key: aws.String("b.txt")},
				{Key: aws.String("c.txt")},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Deleted) != 2 {
		t.Errorf("expect 2 deleted, got %v", out.Deleted)
	}
	if len(out.Errors) != 1 || aws.ToString(out.Errors[0].Code) != "AccessDenied" {
		t.Errorf("unexpected errors %v", out.Errors)
	}
	in := stub.delObjsIn
	if aws.ToString(in.Bucket) != "backend-testbucket" {
		t.Errorf("expect backend-testbucket, got %s", aws.ToString(in.Bucket))
	}
	if len(in.Delete.Objects) != 3 || aws.ToString(in.Delete.Objects[2].Key) != "c.txt" {
		t.Errorf("unexpected objects %v", in.Delete.Objects)
	}
}

func TestProxyObjectTagging(t *testing.T) {
	stub := &stubBackend{
		getTagOut: &s3.GetObjectTaggingOutput{
			TagSet: []types.Tag{
				{Key: aws.String("env"), Value: aws.String("test")},
				{Key: aws.String("team"), Value: aws.String("sre")},
			},
		},
	}
	client, _ := newTestProxy(t, stub)

	if _, err := client.PutObjectTagging(t.Context(), &s3.PutObjectTaggingInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("dir/tagged.txt"),
		Tagging: &types.Tagging{TagSet: []types.Tag{
			{Key: aws.String("env"), Value: aws.String("test")},
			{Key: aws.String("team"), Value: aws.String("sre")},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(stub.putTagIn.Bucket) != "backend-testbucket" {
		t.Errorf("expect backend-testbucket, got %s", aws.ToString(stub.putTagIn.Bucket))
	}
	if len(stub.putTagIn.Tagging.TagSet) != 2 || aws.ToString(stub.putTagIn.Tagging.TagSet[1].Key) != "team" {
		t.Errorf("unexpected tag set %v", stub.putTagIn.Tagging.TagSet)
	}

	got, err := client.GetObjectTagging(t.Context(), &s3.GetObjectTaggingInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("dir/tagged.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TagSet) != 2 || aws.ToString(got.TagSet[0].Key) != "env" || aws.ToString(got.TagSet[1].Value) != "sre" {
		t.Errorf("unexpected tag set %v", got.TagSet)
	}

	if _, err := client.DeleteObjectTagging(t.Context(), &s3.DeleteObjectTaggingInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("dir/tagged.txt"),
	}); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(stub.delTagIn.Key) != "dir/tagged.txt" {
		t.Errorf("unexpected key %v", stub.delTagIn.Key)
	}
}

func TestProxyPutObjectWithTaggingHeader(t *testing.T) {
	stub := &stubBackend{
		putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
	}
	client, _ := newTestProxy(t, stub)
	if _, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket:  aws.String("testbucket"),
		Key:     aws.String("tagged-on-put.txt"),
		Body:    strings.NewReader("x"),
		Tagging: aws.String("env=test&team=sre"),
	}); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(stub.putIn.Tagging); got != "env=test&team=sre" {
		t.Errorf("unexpected tagging %q", got)
	}
}

// TestProxyTenantIsolation verifies that a key of one tenant cannot access
// another tenant's bucket, and that ListBuckets only shows the own tenant.
func TestProxyTenantIsolation(t *testing.T) {
	cfg := &s3rp.Config{
		Tenants: []*s3rp.TenantConfig{
			{
				Name: "tenant-a",
				Users: []*s3rp.UserConfig{
					{
						Name: "usera",
						Keys: []*s3rp.KeyConfig{
							{AccessKeyID: "TENANTAKEY", SecretAccessKey: "tenantasecret"},
						},
					},
				},
				Buckets: []*s3rp.BucketConfig{
					{
						Name: "bucket-a",
						Backend: &s3rp.BackendConfig{
							Endpoint: "http://backend.invalid", AccessKeyID: "bk", SecretAccessKey: "bs",
						},
					},
				},
			},
			{
				Name: "tenant-b",
				Users: []*s3rp.UserConfig{
					{
						Name: "userb",
						Keys: []*s3rp.KeyConfig{
							{AccessKeyID: "TENANTBKEY", SecretAccessKey: "tenantbsecret"},
						},
					},
				},
				Buckets: []*s3rp.BucketConfig{
					{
						Name: "bucket-b",
						Backend: &s3rp.BackendConfig{
							Endpoint: "http://backend.invalid", AccessKeyID: "bk", SecretAccessKey: "bs",
						},
					},
				},
			},
		},
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := s3rp.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubBackend{
		getOut: &s3.GetObjectOutput{Body: io.NopCloser(strings.NewReader("x"))},
	}
	mustSetBackend(t, app, "bucket-a", stub)
	mustSetBackend(t, app, "bucket-b", stub)
	ts := newTestServerForApp(t, app)
	awscfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider("TENANTAKEY", "tenantasecret", ""),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	clientA := s3.NewFromConfig(awscfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts.URL)
		o.UsePathStyle = true
	})

	// own bucket is accessible
	out, err := clientA.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("bucket-a"), Key: aws.String("k"),
	})
	if err != nil {
		t.Fatal(err)
	}
	out.Body.Close()

	// another tenant's bucket is denied
	_, err = clientA.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("bucket-b"), Key: aws.String("k"),
	})
	if err == nil {
		t.Fatal("expect error accessing another tenant's bucket")
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "AccessDenied" {
		t.Errorf("expect AccessDenied, got %v", err)
	}

	// ListBuckets shows only the own tenant's buckets with the tenant owner
	list, err := clientA.ListBuckets(t.Context(), &s3.ListBucketsInput{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Buckets) != 1 || aws.ToString(list.Buckets[0].Name) != "bucket-a" {
		t.Errorf("unexpected buckets %v", list.Buckets)
	}
	if aws.ToString(list.Owner.ID) != "tenant-a" {
		t.Errorf("expect owner tenant-a, got %s", aws.ToString(list.Owner.ID))
	}
}

func TestProxyBucketVersioning(t *testing.T) {
	stub := &stubBackend{
		getVerOut: &s3.GetBucketVersioningOutput{
			Status: types.BucketVersioningStatusEnabled,
		},
	}
	client, _ := newTestProxy(t, stub)

	if _, err := client.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{
		Bucket: aws.String("testbucket"),
		VersioningConfiguration: &types.VersioningConfiguration{
			Status: types.BucketVersioningStatusEnabled,
		},
	}); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(stub.putVerIn.Bucket) != "backend-testbucket" {
		t.Errorf("expect backend-testbucket, got %s", aws.ToString(stub.putVerIn.Bucket))
	}
	if stub.putVerIn.VersioningConfiguration.Status != types.BucketVersioningStatusEnabled {
		t.Errorf("unexpected status %v", stub.putVerIn.VersioningConfiguration.Status)
	}

	out, err := client.GetBucketVersioning(t.Context(), &s3.GetBucketVersioningInput{
		Bucket: aws.String("testbucket"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Status != types.BucketVersioningStatusEnabled {
		t.Errorf("expect Enabled, got %v", out.Status)
	}
}

func TestProxyListObjectVersions(t *testing.T) {
	stub := &stubBackend{
		listVerOut: &s3.ListObjectVersionsOutput{
			IsTruncated: aws.Bool(false),
			MaxKeys:     aws.Int32(1000),
			Versions: []types.ObjectVersion{
				{
					Key:          aws.String("v.txt"),
					VersionId:    aws.String("ver2"),
					IsLatest:     aws.Bool(true),
					ETag:         aws.String(`"etag-2"`),
					Size:         aws.Int64(10),
					LastModified: aws.Time(time.Date(2026, 5, 2, 0, 0, 0, 0, time.UTC)),
				},
				{
					Key:          aws.String("v.txt"),
					VersionId:    aws.String("ver1"),
					IsLatest:     aws.Bool(false),
					ETag:         aws.String(`"etag-1"`),
					Size:         aws.Int64(5),
					LastModified: aws.Time(time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)),
				},
			},
			DeleteMarkers: []types.DeleteMarkerEntry{
				{
					Key:       aws.String("gone.txt"),
					VersionId: aws.String("dm1"),
					IsLatest:  aws.Bool(true),
				},
			},
		},
	}
	client, _ := newTestProxy(t, stub)
	out, err := client.ListObjectVersions(t.Context(), &s3.ListObjectVersionsInput{
		Bucket: aws.String("testbucket"),
		Prefix: aws.String("v"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.Name) != "testbucket" {
		t.Errorf("expect testbucket, got %s", aws.ToString(out.Name))
	}
	if len(out.Versions) != 2 || aws.ToString(out.Versions[1].VersionId) != "ver1" {
		t.Errorf("unexpected versions %v", out.Versions)
	}
	if !aws.ToBool(out.Versions[0].IsLatest) {
		t.Error("expect first version to be latest")
	}
	if len(out.DeleteMarkers) != 1 || aws.ToString(out.DeleteMarkers[0].VersionId) != "dm1" {
		t.Errorf("unexpected delete markers %v", out.DeleteMarkers)
	}
	if aws.ToString(stub.listVerIn.Prefix) != "v" {
		t.Errorf("unexpected prefix %v", stub.listVerIn.Prefix)
	}
}

func TestProxyVersionID(t *testing.T) {
	stub := &stubBackend{
		getOut: &s3.GetObjectOutput{
			Body:      io.NopCloser(strings.NewReader("old version")),
			VersionId: aws.String("ver1"),
		},
		delOut: &s3.DeleteObjectOutput{},
	}
	client, _ := newTestProxy(t, stub)
	out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket:    aws.String("testbucket"),
		Key:       aws.String("v.txt"),
		VersionId: aws.String("ver1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	out.Body.Close()
	if aws.ToString(stub.getIn.VersionId) != "ver1" {
		t.Errorf("expect versionId ver1, got %v", stub.getIn.VersionId)
	}
	if aws.ToString(out.VersionId) != "ver1" {
		t.Errorf("expect x-amz-version-id ver1, got %v", out.VersionId)
	}
	if _, err := client.DeleteObject(t.Context(), &s3.DeleteObjectInput{
		Bucket:    aws.String("testbucket"),
		Key:       aws.String("v.txt"),
		VersionId: aws.String("ver1"),
	}); err != nil {
		t.Fatal(err)
	}
	if aws.ToString(stub.delIn.VersionId) != "ver1" {
		t.Errorf("expect versionId ver1, got %v", stub.delIn.VersionId)
	}
}

// newCopyTestProxy builds a proxy with one tenant holding two buckets on the
// same backend and one on a different backend.
func newCopyTestProxy(t *testing.T) (*s3.Client, *stubBackend) {
	t.Helper()
	sameBackend := func(name string) *s3rp.BucketConfig {
		return &s3rp.BucketConfig{
			Name: name,
			Backend: &s3rp.BackendConfig{
				Endpoint:        "http://backend.invalid",
				Bucket:          "backend-" + name,
				AccessKeyID:     "backendkey",
				SecretAccessKey: "backendsecret",
			},
		}
	}
	cfg := &s3rp.Config{
		Tenants: []*s3rp.TenantConfig{
			{
				Name: "copytenant",
				Users: []*s3rp.UserConfig{
					{
						Name: "copyuser",
						Keys: []*s3rp.KeyConfig{
							{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey},
						},
					},
				},
				Buckets: []*s3rp.BucketConfig{
					sameBackend("srcbucket"),
					sameBackend("dstbucket"),
					{
						Name: "remotebucket",
						Backend: &s3rp.BackendConfig{
							Endpoint:        "http://other-backend.invalid",
							AccessKeyID:     "otherkey",
							SecretAccessKey: "othersecret",
						},
					},
				},
			},
		},
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := s3rp.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	stub := &stubBackend{
		copyOut: &s3.CopyObjectOutput{
			CopyObjectResult: &types.CopyObjectResult{
				ETag:         aws.String(`"copy-etag"`),
				LastModified: aws.Time(time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)),
			},
		},
		upcOut: &s3.UploadPartCopyOutput{
			CopyPartResult: &types.CopyPartResult{
				ETag: aws.String(`"part-copy-etag"`),
			},
		},
	}
	for _, name := range []string{"srcbucket", "dstbucket", "remotebucket"} {
		mustSetBackend(t, app, name, stub)
	}
	ts := httptest.NewServer(app.Handler())
	t.Cleanup(ts.Close)
	awscfg, err := awsconfig.LoadDefaultConfig(t.Context(),
		awsconfig.WithRegion("us-east-1"),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(testAccessKeyID, testSecretAccessKey, ""),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := s3.NewFromConfig(awscfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(ts.URL)
		o.UsePathStyle = true
	})
	return client, stub
}

func TestProxyCopyObject(t *testing.T) {
	client, stub := newCopyTestProxy(t)
	out, err := client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket:            aws.String("dstbucket"),
		Key:               aws.String("dir/dst.txt"),
		CopySource:        aws.String("srcbucket/dir/src file.txt"),
		Metadata:          map[string]string{"copied": "yes"},
		MetadataDirective: types.MetadataDirectiveReplace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.CopyObjectResult.ETag) != `"copy-etag"` {
		t.Errorf("unexpected etag %v", out.CopyObjectResult.ETag)
	}
	in := stub.copyIn
	if aws.ToString(in.Bucket) != "backend-dstbucket" {
		t.Errorf("expect backend-dstbucket, got %s", aws.ToString(in.Bucket))
	}
	// the copy source must be rewritten to the backend bucket name and escaped
	if got := aws.ToString(in.CopySource); got != "backend-srcbucket/dir/src%20file.txt" {
		t.Errorf("unexpected copy source %s", got)
	}
	if in.MetadataDirective != types.MetadataDirectiveReplace {
		t.Errorf("unexpected metadata directive %v", in.MetadataDirective)
	}
	if in.Metadata["copied"] != "yes" {
		t.Errorf("unexpected metadata %v", in.Metadata)
	}
}

func TestProxyCopyObjectAcrossBackends(t *testing.T) {
	client, _ := newCopyTestProxy(t)
	_, err := client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket:     aws.String("remotebucket"),
		Key:        aws.String("dst.txt"),
		CopySource: aws.String("srcbucket/src.txt"),
	})
	if err == nil {
		t.Fatal("expect error")
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "NotImplemented" {
		t.Errorf("expect NotImplemented, got %v", err)
	}
}

func TestProxyCopyObjectUnauthorizedSource(t *testing.T) {
	client, _ := newCopyTestProxy(t)
	_, err := client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket:     aws.String("dstbucket"),
		Key:        aws.String("dst.txt"),
		CopySource: aws.String("nosuchbucket/src.txt"),
	})
	if err == nil {
		t.Fatal("expect error")
	}
	var apiErr smithy.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode() != "AccessDenied" {
		t.Errorf("expect AccessDenied, got %v", err)
	}
}

func TestProxyUploadPartCopy(t *testing.T) {
	client, stub := newCopyTestProxy(t)
	out, err := client.UploadPartCopy(t.Context(), &s3.UploadPartCopyInput{
		Bucket:          aws.String("dstbucket"),
		Key:             aws.String("dir/dst.bin"),
		UploadId:        aws.String("upc-upload-id"),
		PartNumber:      aws.Int32(3),
		CopySource:      aws.String("srcbucket/dir/src.bin"),
		CopySourceRange: aws.String("bytes=0-1048575"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(out.CopyPartResult.ETag) != `"part-copy-etag"` {
		t.Errorf("unexpected etag %v", out.CopyPartResult.ETag)
	}
	in := stub.upcIn
	if aws.ToString(in.CopySource) != "backend-srcbucket/dir/src.bin" {
		t.Errorf("unexpected copy source %v", in.CopySource)
	}
	if aws.ToString(in.UploadId) != "upc-upload-id" || aws.ToInt32(in.PartNumber) != 3 {
		t.Errorf("unexpected upload id/part %v %v", in.UploadId, in.PartNumber)
	}
	if aws.ToString(in.CopySourceRange) != "bytes=0-1048575" {
		t.Errorf("unexpected range %v", in.CopySourceRange)
	}
}
