package localbackend_test

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/fujiwara/s3rp/s3gw/localbackend"
)

func openDir(t *testing.T, dir string) *localbackend.Backend {
	t.Helper()
	be, err := localbackend.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { be.Close() })
	return be
}

func blobFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func read(t *testing.T, be *localbackend.Backend, bucket, key string, versionID *string) string {
	t.Helper()
	out, err := be.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key), VersionId: versionID})
	if err != nil {
		t.Fatal(err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Everything the API can show of a bucket is there again after the
// Backend is closed and its directory opened anew.
func TestReopen(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	be := openDir(t, dir)
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	if _, err := be.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("b"), ObjectLockEnabledForBucket: aws.Bool(true)}); err != nil {
		t.Fatal(err)
	}
	if _, err := be.PutBucketEncryption(ctx, &s3.PutBucketEncryptionInput{
		Bucket: aws.String("b"),
		ServerSideEncryptionConfiguration: &types.ServerSideEncryptionConfiguration{Rules: []types.ServerSideEncryptionRule{{
			ApplyServerSideEncryptionByDefault: &types.ServerSideEncryptionByDefault{SSEAlgorithm: types.ServerSideEncryptionAes256},
		}}},
	}); err != nil {
		t.Fatal(err)
	}
	v1, err := be.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("b"), Key: aws.String("k"), Body: strings.NewReader("one"),
		ContentType: aws.String("text/plain"), Metadata: map[string]string{"m": "v"},
		Tagging: aws.String("t=1"), ChecksumAlgorithm: types.ChecksumAlgorithmSha256,
		ObjectLockMode: types.ObjectLockModeGovernance, ObjectLockRetainUntilDate: aws.Time(until),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("b"), Key: aws.String("k"), Body: strings.NewReader("two")}); err != nil {
		t.Fatal(err)
	}
	if _, err := be.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("b"), Key: aws.String("gone")}); err != nil {
		t.Fatal(err)
	}
	// a multipart object, a copy sharing its bytes, and an upload under way
	const partSize = 5 * 1024 * 1024
	big := bytes.Repeat([]byte("p"), partSize)
	done := multipart(t, be, "b", "big", big, []byte("tail"))
	if _, err := be.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("b"), Key: aws.String("copy"), CopySource: aws.String("b/big")}); err != nil {
		t.Fatal(err)
	}
	open, err := be.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String("b"), Key: aws.String("later"), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32})
	if err != nil {
		t.Fatal(err)
	}
	part, err := be.UploadPart(ctx, &s3.UploadPartInput{
		Bucket: aws.String("b"), Key: aws.String("later"), UploadId: open.UploadId, PartNumber: aws.Int32(1), Body: strings.NewReader("part one"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := be.Close(); err != nil {
		t.Fatal(err)
	}

	be = openDir(t, dir)
	if got := read(t, be, "b", "k", nil); got != "two" {
		t.Errorf("current version = %q", got)
	}
	if got := read(t, be, "b", "k", v1.VersionId); got != "one" {
		t.Errorf("first version = %q", got)
	}
	head, err := be.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String("b"), Key: aws.String("k"), VersionId: v1.VersionId, ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(head.ContentType) != "text/plain" || head.Metadata["m"] != "v" || aws.ToInt32(head.TagCount) != 1 ||
		aws.ToString(head.ETag) != aws.ToString(v1.ETag) || aws.ToString(head.ChecksumSHA256) != aws.ToString(v1.ChecksumSHA256) ||
		head.ServerSideEncryption != types.ServerSideEncryptionAes256 ||
		head.ObjectLockMode != types.ObjectLockModeGovernance || !head.ObjectLockRetainUntilDate.Equal(until) {
		t.Errorf("first version after reopening: %+v", head)
	}
	// the retention is still enforced
	_, err = be.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("b"), Key: aws.String("k"), VersionId: v1.VersionId})
	wantError(t, err, http.StatusForbidden, "AccessDenied")
	versions, err := be.ListObjectVersions(ctx, &s3.ListObjectVersionsInput{Bucket: aws.String("b"), Prefix: aws.String("gone")})
	if err != nil {
		t.Fatal(err)
	}
	if len(versions.DeleteMarkers) != 1 {
		t.Errorf("delete markers of gone = %+v", versions.DeleteMarkers)
	}
	for _, key := range []string{"big", "copy"} {
		if got := read(t, be, "b", key, nil); got != string(big)+"tail" {
			t.Errorf("%s: %d bytes after reopening", key, len(got))
		}
	}
	bigHead, err := be.HeadObject(ctx, &s3.HeadObjectInput{Bucket: aws.String("b"), Key: aws.String("big"), PartNumber: aws.Int32(2)})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(bigHead.ETag) != aws.ToString(done.ETag) || aws.ToInt32(bigHead.PartsCount) != 2 || aws.ToInt64(bigHead.ContentLength) != 4 {
		t.Errorf("multipart object after reopening: %+v", bigHead)
	}
	// the upload goes on where it was
	parts, err := be.ListParts(ctx, &s3.ListPartsInput{Bucket: aws.String("b"), Key: aws.String("later"), UploadId: open.UploadId})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts.Parts) != 1 || aws.ToString(parts.Parts[0].ETag) != aws.ToString(part.ETag) || parts.ChecksumAlgorithm != types.ChecksumAlgorithmCrc32 {
		t.Errorf("parts after reopening: %+v", parts)
	}
	if _, err := be.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("b"), Key: aws.String("later"), UploadId: open.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{{PartNumber: aws.Int32(1), ETag: part.ETag}}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := read(t, be, "b", "later", nil); got != "part one" {
		t.Errorf("upload completed after reopening = %q", got)
	}
}

func multipart(t *testing.T, be *localbackend.Backend, bucket, key string, bodies ...[]byte) *s3.CompleteMultipartUploadOutput {
	t.Helper()
	ctx := t.Context()
	create, err := be.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{Bucket: aws.String(bucket), Key: aws.String(key)})
	if err != nil {
		t.Fatal(err)
	}
	var parts []types.CompletedPart
	for i, body := range bodies {
		n := aws.Int32(int32(i + 1))
		out, err := be.UploadPart(ctx, &s3.UploadPartInput{
			Bucket: aws.String(bucket), Key: aws.String(key), UploadId: create.UploadId, PartNumber: n, Body: bytes.NewReader(body),
		})
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, types.CompletedPart{PartNumber: n, ETag: out.ETag})
	}
	done, err := be.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
		Bucket: aws.String(bucket), Key: aws.String(key), UploadId: create.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	if err != nil {
		t.Fatal(err)
	}
	return done
}

// A blob file goes when nothing refers to it any more, and not before the
// last response reading it is done.
func TestBlobFilesAreCollected(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	be := openDir(t, dir)
	if _, err := be.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("b")}); err != nil {
		t.Fatal(err)
	}
	put := func(key, body string) {
		t.Helper()
		if _, err := be.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("b"), Key: aws.String(key), Body: strings.NewReader(body)}); err != nil {
			t.Fatal(err)
		}
	}
	sync := func(want int, when string) {
		t.Helper()
		if err := be.Sync(); err != nil {
			t.Fatal(err)
		}
		if got := blobFiles(t, dir); len(got) != want {
			t.Errorf("%s: %d blob files %v, want %d", when, len(got), got, want)
		}
	}
	put("a", "first")
	put("shared", "shared bytes")
	sync(2, "two objects")

	put("a", "second") // an overwrite leaves the first body unreferenced
	sync(2, "after an overwrite")

	// a copy shares its source's blob, which then outlives the source
	if _, err := be.CopyObject(ctx, &s3.CopyObjectInput{Bucket: aws.String("b"), Key: aws.String("copy"), CopySource: aws.String("b/shared")}); err != nil {
		t.Fatal(err)
	}
	if _, err := be.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("b"), Key: aws.String("shared")}); err != nil {
		t.Fatal(err)
	}
	sync(2, "source of a copy deleted")
	if got := read(t, be, "b", "copy", nil); got != "shared bytes" {
		t.Errorf("copy after its source was deleted = %q", got)
	}

	// a response body opened before the delete reads to its end
	out, err := be.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String("b"), Key: aws.String("copy")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := be.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: aws.String("b"), Key: aws.String("copy")}); err != nil {
		t.Fatal(err)
	}
	sync(2, "deleted while being read")
	if b, err := io.ReadAll(out.Body); err != nil || string(b) != "shared bytes" {
		t.Errorf("body read across its delete = %q, %v", b, err)
	}
	out.Body.Close()
	sync(1, "after the reader closed")

	// a write that is refused leaves no file
	_, err = be.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String("b"), Key: aws.String("bad"), Body: strings.NewReader("data"), ChecksumCRC32: aws.String("AAAAAA=="),
	})
	wantError(t, err, http.StatusBadRequest, "BadDigest")
	_, err = be.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("missing"), Key: aws.String("k"), Body: strings.NewReader("data")})
	wantError(t, err, http.StatusNotFound, "NoSuchBucket")
	if got := blobFiles(t, dir); len(got) != 1 {
		t.Errorf("after refused writes: blob files %v", got)
	}
}

// Changes reach the directory on their own, a moment after the request.
func TestChangesAreWrittenWithoutSync(t *testing.T) {
	dir := t.TempDir()
	be := openDir(t, dir)
	if _, err := be.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("b")}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if data, err := os.ReadFile(filepath.Join(dir, "meta.json")); err == nil && bytes.Contains(data, []byte(`"b"`)) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the bucket never reached meta.json")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// What a process that died left behind is cleared; a directory this
// package could not have left is refused.
func TestOpenClearsAndChecks(t *testing.T) {
	dir := t.TempDir()
	ctx := t.Context()
	be := openDir(t, dir)
	if _, err := be.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: aws.String("b")}); err != nil {
		t.Fatal(err)
	}
	if _, err := be.PutObject(ctx, &s3.PutObjectInput{Bucket: aws.String("b"), Key: aws.String("k"), Body: strings.NewReader("kept")}); err != nil {
		t.Fatal(err)
	}
	if err := be.Close(); err != nil {
		t.Fatal(err)
	}
	kept := blobFiles(t, dir)
	if len(kept) != 1 {
		t.Fatalf("blob files = %v", kept)
	}
	for _, name := range []string{"tmp-half-written", "ORPHAN"} {
		if err := os.WriteFile(filepath.Join(dir, "blobs", name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	be = openDir(t, dir)
	if got := blobFiles(t, dir); len(got) != 1 || got[0] != kept[0] {
		t.Errorf("blob files after Open = %v, want only %v", got, kept)
	}
	if got := read(t, be, "b", "k", nil); got != "kept" {
		t.Errorf("object = %q", got)
	}
	be.Close()

	t.Run("missing blob", func(t *testing.T) {
		if err := os.Remove(filepath.Join(dir, "blobs", kept[0])); err != nil {
			t.Fatal(err)
		}
		if _, err := localbackend.Open(dir); err == nil || !strings.Contains(err.Error(), "missing") {
			t.Errorf("Open with a blob gone: %v", err)
		}
	})
	t.Run("unknown format version", func(t *testing.T) {
		other := t.TempDir()
		if err := os.WriteFile(filepath.Join(other, "meta.json"), []byte(`{"version":99}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := localbackend.Open(other); err == nil || !strings.Contains(err.Error(), "format version") {
			t.Errorf("Open of another format: %v", err)
		}
	})
}

func TestSyncAndCloseInMemory(t *testing.T) {
	be := localbackend.New()
	if err := be.Sync(); err != nil {
		t.Error(err)
	}
	if err := be.Close(); err != nil {
		t.Error(err)
	}
	if _, err := be.CreateBucket(t.Context(), &s3.CreateBucketInput{Bucket: aws.String("b")}); err != nil {
		t.Errorf("a Backend in memory after Close: %v", err)
	}
}
