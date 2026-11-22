package membackend_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"testing/iotest"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/s3gw/membackend"
)

// The methods a control plane calls, as it would call them on the SDK
// client: failures are the SDK's modeled errors where it has them.
func TestBucketAdministration(t *testing.T) {
	be := membackend.New()
	ctx := t.Context()
	create := func(name string, lock bool) error {
		_, err := be.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String(name), ObjectLockEnabledForBucket: aws.Bool(lock)})
		return err
	}
	if err := create("b", false); err != nil {
		t.Fatal(err)
	}
	if _, ok := errors.AsType[*types.BucketAlreadyOwnedByYou](create("b", false)); !ok {
		t.Error("creating a bucket twice must be BucketAlreadyOwnedByYou")
	}
	if _, err := be.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("missing")}); err == nil {
		t.Error("HeadBucket of a missing bucket succeeded")
	} else if _, ok := errors.AsType[*types.NotFound](err); !ok {
		t.Errorf("HeadBucket of a missing bucket: %v", err)
	}

	if _, err := be.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("b"), Key: aws.String("k"), Body: strings.NewReader("x")}); err != nil {
		t.Fatal(err)
	}
	_, err := be.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("b")})
	wantError(t, err, http.StatusConflict, "BucketNotEmpty")
	if _, err := be.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("b"), Key: aws.String("k")}); err != nil {
		t.Fatal(err)
	}
	if _, err := be.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("b")}); err != nil {
		t.Errorf("deleting an emptied bucket: %v", err)
	}
	_, err = be.DeleteBucket(ctx, &s3.DeleteBucketInput{Bucket: aws.String("b")})
	if _, ok := errors.AsType[*types.NoSuchBucket](err); !ok {
		t.Errorf("deleting a missing bucket: %v", err)
	}

	// a bucket with Object Lock is versioned and stays so
	if err := create("locked", true); err != nil {
		t.Fatal(err)
	}
	if v, err := be.GetBucketVersioning(ctx, &s3.GetBucketVersioningInput{Bucket: aws.String("locked")}); err != nil || v.Status != types.BucketVersioningStatusEnabled {
		t.Errorf("versioning of an Object Lock bucket = %v, %v", v, err)
	}
	_, err = be.PutBucketVersioning(ctx, &s3.PutBucketVersioningInput{
		Bucket:                  aws.String("locked"),
		VersioningConfiguration: &types.VersioningConfiguration{Status: types.BucketVersioningStatusSuspended},
	})
	wantError(t, err, http.StatusConflict, "InvalidBucketState")
}

func TestDefaultEncryption(t *testing.T) {
	f := newFixture(t)
	f.createBucket("docs", false)
	_, err := f.client.GetBucketEncryption(t.Context(), &s3.GetBucketEncryptionInput{Bucket: aws.String("docs")})
	wantError(t, err, http.StatusNotFound, "ServerSideEncryptionConfigurationNotFoundError")
	if got := f.put("docs", "plain", "x").ServerSideEncryption; got != "" {
		t.Errorf("without a default, a write reported encryption %q", got)
	}
	if _, err := f.backend.PutBucketEncryption(t.Context(), &s3.PutBucketEncryptionInput{
		Bucket: aws.String(backendName("docs")),
		ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
			ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.put("docs", "encrypted", "x").ServerSideEncryption; got != types.ServerSideEncryptionAes256 {
		t.Errorf("under a default of AES256, a write reported %q", got)
	}
	// a mode the write names itself wins over the default
	out, err := f.client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("kms"), Body: strings.NewReader("x"),
		ServerSideEncryption: types.ServerSideEncryptionAwsKms, SSEKMSKeyId: aws.String("key-1"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.ServerSideEncryption != types.ServerSideEncryptionAwsKms || aws.ToString(out.SSEKMSKeyId) != "key-1" {
		t.Errorf("SSE-KMS write reported %q key %q", out.ServerSideEncryption, aws.ToString(out.SSEKMSKeyId))
	}
}

// The gateway's body readers fail with the S3 error the client is owed, and
// finds it in what the backend returns: the write must fail with that error
// in the chain and store nothing.
func TestBodyReadErrorFailsTheWrite(t *testing.T) {
	be := membackend.New()
	ctx := t.Context()
	if _, err := be.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("b")}); err != nil {
		t.Fatal(err)
	}
	refusal := s3err.ContentSHA256Mismatch()
	body := io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(refusal))
	_, err := be.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("b"), Key: aws.String("k"), Body: body})
	if got, ok := errors.AsType[*s3err.Error](err); !ok || got != refusal {
		t.Fatalf("expect the read error in the chain, got %v", err)
	}
	if _, err := be.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("b"), Key: aws.String("k")}); err == nil {
		t.Error("a write whose body failed left an object")
	}
	// a body shorter than the length the write declares
	_, err = be.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("b"), Key: aws.String("k"), Body: strings.NewReader("short"), ContentLength: aws.Int64(10),
	})
	wantError(t, err, http.StatusBadRequest, "IncompleteBody")
}

func TestChecksumMismatchRefused(t *testing.T) {
	be := membackend.New()
	ctx := t.Context()
	if _, err := be.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("b")}); err != nil {
		t.Fatal(err)
	}
	_, err := be.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("b"), Key: aws.String("k"), Body: strings.NewReader("data"),
		ChecksumCRC32: aws.String("AAAAAA=="),
	})
	wantError(t, err, http.StatusBadRequest, "BadDigest")
	_, err = be.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("b"), Key: aws.String("k"), Body: strings.NewReader("data"),
		ContentMD5: aws.String("AAAAAAAAAAAAAAAAAAAAAA=="),
	})
	wantError(t, err, http.StatusBadRequest, "BadDigest")
}

func TestCanceledContext(t *testing.T) {
	be := membackend.New()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := be.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: aws.String("b")}); !errors.Is(err, context.Canceled) {
		t.Errorf("a canceled request: %v", err)
	}
}

// One Backend serves every request of a gateway at once.
func TestConcurrentUse(t *testing.T) {
	be := membackend.New()
	ctx := t.Context()
	if _, err := be.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("b")}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			key := aws.String("k" + strconv.Itoa(i%2)) // writers share keys
			for range 50 {
				if _, err := be.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("b"), Key: key, Body: strings.NewReader("v")}); err != nil {
					t.Error(err)
					return
				}
				out, err := be.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("b"), Key: key})
				if err != nil {
					t.Error(err)
					return
				}
				io.Copy(io.Discard, out.Body)
				if _, err := be.ListObjectsV2(ctx, &s3.ListObjectsV2Input{Bucket: aws.String("b")}); err != nil {
					t.Error(err)
					return
				}
				if _, err := be.PutObjectTagging(ctx, &s3.PutObjectTaggingInput{
					Bucket: aws.String("b"), Key: key, Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("a"), Value: aws.String("b")}}},
				}); err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	wg.Wait()
}
