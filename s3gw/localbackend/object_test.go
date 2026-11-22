package localbackend_test

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/google/go-cmp/cmp"
)

func (f *fixture) put(bucket, key, body string) *s3.PutObjectOutput {
	f.t.Helper()
	out, err := f.client.PutObject(f.t.Context(), &s3.PutObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), Body: strings.NewReader(body),
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return out
}

func (f *fixture) get(bucket, key string, versionID *string) string {
	f.t.Helper()
	out, err := f.client.GetObject(f.t.Context(), &s3.GetObjectInput{
		Bucket: aws.String(bucket), Key: aws.String(key), VersionId: versionID,
	})
	if err != nil {
		f.t.Fatal(err)
	}
	defer out.Body.Close()
	b, err := io.ReadAll(out.Body)
	if err != nil {
		f.t.Fatal(err)
	}
	return string(b)
}

func TestObjectRoundTrip(t *testing.T) {
	f := newFixture(t)
	f.createBucket("docs", false)
	const body = "hello, in-process backend"
	sum := md5.Sum([]byte(body))
	wantETag := `"` + hex.EncodeToString(sum[:]) + `"`

	put, err := f.client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("a/hello.txt"),
		Body:         strings.NewReader(body),
		ContentType:  aws.String("text/plain"),
		CacheControl: aws.String("max-age=60"),
		Metadata:     map[string]string{"origin": "test"},
		StorageClass: types.StorageClassStandardIa,
		Tagging:      aws.String("team=storage&env=test"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(put.ETag); got != wantETag {
		t.Errorf("ETag = %s, want the MD5 %s", got, wantETag)
	}
	if put.VersionId != nil {
		t.Errorf("an unversioned bucket reported version id %q", *put.VersionId)
	}

	head, err := f.client.HeadObject(t.Context(), &s3.HeadObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("a/hello.txt"),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(head.ContentType) != "text/plain" || aws.ToString(head.CacheControl) != "max-age=60" ||
		aws.ToInt64(head.ContentLength) != int64(len(body)) || aws.ToString(head.ETag) != wantETag {
		t.Errorf("unexpected head: %+v", head)
	}
	if diff := cmp.Diff(map[string]string{"origin": "test"}, head.Metadata); diff != "" {
		t.Errorf("metadata (-want +got):\n%s", diff)
	}
	if head.StorageClass != types.StorageClassStandardIa {
		t.Errorf("storage class = %q", head.StorageClass)
	}
	if aws.ToInt32(head.TagCount) != 2 {
		t.Errorf("tag count = %d, want 2", aws.ToInt32(head.TagCount))
	}
	// over TLS the SDK sends its checksum as a trailer, which the gateway
	// verifies and the backend is asked to compute for itself
	if aws.ToString(head.ChecksumCRC32) == "" || head.ChecksumType != types.ChecksumTypeFullObject {
		t.Errorf("expect the trailer's CRC32 to be stored, got %q type %q",
			aws.ToString(head.ChecksumCRC32), head.ChecksumType)
	}

	if got := f.get("docs", "a/hello.txt", nil); got != body {
		t.Errorf("body = %q", got)
	}

	if _, err := f.client.DeleteObject(t.Context(), &s3.DeleteObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("a/hello.txt"),
	}); err != nil {
		t.Fatal(err)
	}
	_, err = f.client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("a/hello.txt"),
	})
	wantError(t, err, http.StatusNotFound, "NoSuchKey")
}

func TestRange(t *testing.T) {
	f := newFixture(t)
	f.createBucket("docs", false)
	f.put("docs", "digits", "0123456789")
	tests := []struct {
		name, header string
		want         string
		contentRange string
	}{
		{"first-last", "bytes=2-5", "2345", "bytes 2-5/10"},
		{"open end", "bytes=7-", "789", "bytes 7-9/10"},
		{"suffix", "bytes=-3", "789", "bytes 7-9/10"},
		{"last past the end", "bytes=8-100", "89", "bytes 8-9/10"},
		{"last at the largest offset", "bytes=8-9223372036854775807", "89", "bytes 8-9/10"},
		{"suffix longer than the object", "bytes=-100", "0123456789", "bytes 0-9/10"},
		{"ignored: another unit", "items=1-2", "0123456789", ""},
		{"ignored: several ranges", "bytes=0-1,3-4", "0123456789", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			out, err := f.client.GetObject(t.Context(), &s3.GetObjectInput{
				Bucket: aws.String("docs"), Key: aws.String("digits"), Range: aws.String(tt.header),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer out.Body.Close()
			b, _ := io.ReadAll(out.Body)
			if string(b) != tt.want || aws.ToString(out.ContentRange) != tt.contentRange {
				t.Errorf("got %q (Content-Range %q), want %q (%q)", b, aws.ToString(out.ContentRange), tt.want, tt.contentRange)
			}
		})
	}
	_, err := f.client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("digits"), Range: aws.String("bytes=10-"),
	})
	wantError(t, err, http.StatusRequestedRangeNotSatisfiable, "InvalidRange")
}

func TestConditionalRequests(t *testing.T) {
	f := newFixture(t)
	f.createBucket("docs", false)
	etag := f.put("docs", "k", "v1").ETag
	past := aws.Time(time.Now().Add(-time.Hour))
	future := aws.Time(time.Now().Add(time.Hour))

	get := func(in *s3.GetObjectInput) error {
		in.Bucket, in.Key = aws.String("docs"), aws.String("k")
		out, err := f.client.GetObject(t.Context(), in)
		if err == nil {
			out.Body.Close()
		}
		return err
	}
	if err := get(&s3.GetObjectInput{IfMatch: etag}); err != nil {
		t.Errorf("If-Match with the current ETag: %v", err)
	}
	wantError(t, get(&s3.GetObjectInput{IfMatch: aws.String(`"other"`)}), http.StatusPreconditionFailed, "PreconditionFailed")
	wantError(t, get(&s3.GetObjectInput{IfNoneMatch: etag}), http.StatusNotModified, "NotModified")
	wantError(t, get(&s3.GetObjectInput{IfModifiedSince: future}), http.StatusNotModified, "NotModified")
	wantError(t, get(&s3.GetObjectInput{IfUnmodifiedSince: past}), http.StatusPreconditionFailed, "PreconditionFailed")
	// If-Match outranks If-Unmodified-Since, as in HTTP
	if err := get(&s3.GetObjectInput{IfMatch: etag, IfUnmodifiedSince: past}); err != nil {
		t.Errorf("If-Match must outrank If-Unmodified-Since: %v", err)
	}

	// conditional writes
	_, err := f.client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("k"), Body: strings.NewReader("v2"), IfNoneMatch: aws.String("*"),
	})
	wantError(t, err, http.StatusPreconditionFailed, "PreconditionFailed")
	if _, err := f.client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("new"), Body: strings.NewReader("v"), IfNoneMatch: aws.String("*"),
	}); err != nil {
		t.Errorf("If-None-Match: * on a new key: %v", err)
	}
	_, err = f.client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("k"), Body: strings.NewReader("v2"), IfMatch: aws.String(`"other"`),
	})
	wantError(t, err, http.StatusPreconditionFailed, "PreconditionFailed")
	if got := f.get("docs", "k", nil); got != "v1" {
		t.Errorf("a refused conditional write changed the object to %q", got)
	}
}

func TestListObjects(t *testing.T) {
	f := newFixture(t)
	f.createBucket("docs", false)
	for _, k := range []string{"a.txt", "dir/1", "dir/2", "dir/sub/3", "e f.txt", "z.txt"} {
		f.put("docs", k, "x")
	}

	t.Run("delimiter pages", func(t *testing.T) {
		// every page of two, with a common prefix counting as one entry
		// and not repeated on the page after it
		var keys, prefixes []string
		var token *string
		for range 10 {
			out, err := f.client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
				Bucket: aws.String("docs"), Delimiter: aws.String("/"), MaxKeys: aws.Int32(2), ContinuationToken: token,
			})
			if err != nil {
				t.Fatal(err)
			}
			if n := len(out.Contents) + len(out.CommonPrefixes); n > 2 || int(aws.ToInt32(out.KeyCount)) != n {
				t.Fatalf("page of %d entries, KeyCount %d", n, aws.ToInt32(out.KeyCount))
			}
			for _, o := range out.Contents {
				keys = append(keys, *o.Key)
			}
			for _, p := range out.CommonPrefixes {
				prefixes = append(prefixes, *p.Prefix)
			}
			if !aws.ToBool(out.IsTruncated) {
				break
			}
			token = out.NextContinuationToken
		}
		if diff := cmp.Diff([]string{"a.txt", "e f.txt", "z.txt"}, keys); diff != "" {
			t.Errorf("keys (-want +got):\n%s", diff)
		}
		if diff := cmp.Diff([]string{"dir/"}, prefixes); diff != "" {
			t.Errorf("common prefixes (-want +got):\n%s", diff)
		}
	})

	t.Run("prefix and start-after", func(t *testing.T) {
		out, err := f.client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
			Bucket: aws.String("docs"), Prefix: aws.String("dir/"), StartAfter: aws.String("dir/1"),
		})
		if err != nil {
			t.Fatal(err)
		}
		var keys []string
		for _, o := range out.Contents {
			keys = append(keys, *o.Key)
		}
		if diff := cmp.Diff([]string{"dir/2", "dir/sub/3"}, keys); diff != "" {
			t.Errorf("keys (-want +got):\n%s", diff)
		}
	})

	t.Run("marker inside a common prefix", func(t *testing.T) {
		// the prefix the marker lies in is behind the marker, with the
		// keys it rolls up
		out, err := f.client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
			Bucket: aws.String("docs"), Delimiter: aws.String("/"), StartAfter: aws.String("dir/1"),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.CommonPrefixes) != 0 || len(out.Contents) != 2 || *out.Contents[0].Key != "e f.txt" {
			t.Errorf("after dir/1: prefixes %+v, contents %+v", out.CommonPrefixes, out.Contents)
		}
	})

	t.Run("v1 marker", func(t *testing.T) {
		out, err := f.client.ListObjects(t.Context(), &s3.ListObjectsInput{
			Bucket: aws.String("docs"), Delimiter: aws.String("/"), MaxKeys: aws.Int32(2),
		})
		if err != nil {
			t.Fatal(err)
		}
		if !aws.ToBool(out.IsTruncated) || aws.ToString(out.NextMarker) != "dir/" {
			t.Fatalf("truncated %v, NextMarker %q", aws.ToBool(out.IsTruncated), aws.ToString(out.NextMarker))
		}
		next, err := f.client.ListObjects(t.Context(), &s3.ListObjectsInput{
			Bucket: aws.String("docs"), Delimiter: aws.String("/"), Marker: out.NextMarker,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(next.CommonPrefixes) != 0 || len(next.Contents) != 2 || *next.Contents[0].Key != "e f.txt" {
			t.Errorf("after the marker dir/: %d prefixes, contents %+v", len(next.CommonPrefixes), next.Contents)
		}
	})

	t.Run("encoding-type url", func(t *testing.T) {
		out, err := f.client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
			Bucket: aws.String("docs"), Prefix: aws.String("e"), EncodingType: types.EncodingTypeUrl,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Contents) != 1 || *out.Contents[0].Key != "e+f.txt" {
			t.Errorf("contents = %+v, want the key form-encoded", out.Contents)
		}
	})

	t.Run("max-keys zero", func(t *testing.T) {
		out, err := f.client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{
			Bucket: aws.String("docs"), MaxKeys: aws.Int32(0),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.Contents) != 0 || aws.ToBool(out.IsTruncated) {
			t.Errorf("max-keys=0: %d keys, truncated %v", len(out.Contents), aws.ToBool(out.IsTruncated))
		}
	})
}

func TestVersioning(t *testing.T) {
	f := newFixture(t)
	f.createBucket("docs", false)
	f.put("docs", "k", "before versioning")
	setVersioning := func(status types.BucketVersioningStatus) {
		t.Helper()
		if _, err := f.backend.PutBucketVersioning(t.Context(), &s3.PutBucketVersioningInput{
			Bucket:                  aws.String(backendName("docs")),
			VersioningConfiguration: &types.VersioningConfiguration{Status: status},
		}); err != nil {
			t.Fatal(err)
		}
	}
	setVersioning(types.BucketVersioningStatusEnabled)
	if out, err := f.client.GetBucketVersioning(t.Context(), &s3.GetBucketVersioningInput{Bucket: aws.String("docs")}); err != nil || out.Status != types.BucketVersioningStatusEnabled {
		t.Fatalf("GetBucketVersioning = %v, %v", out, err)
	}

	v1 := f.put("docs", "k", "one").VersionId
	v2 := f.put("docs", "k", "two").VersionId
	if aws.ToString(v1) == "" || aws.ToString(v1) == aws.ToString(v2) {
		t.Fatalf("version ids %q and %q", aws.ToString(v1), aws.ToString(v2))
	}
	if got := f.get("docs", "k", v1); got != "one" {
		t.Errorf("version 1 = %q", got)
	}
	if got := f.get("docs", "k", aws.String("null")); got != "before versioning" {
		t.Errorf("the null version = %q", got)
	}

	del, err := f.client.DeleteObject(t.Context(), &s3.DeleteObjectInput{Bucket: aws.String("docs"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	if !aws.ToBool(del.DeleteMarker) || aws.ToString(del.VersionId) == "" {
		t.Fatalf("a delete in a versioned bucket must report its delete marker, got %+v", del)
	}
	_, err = f.client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("docs"), Key: aws.String("k")})
	wantError(t, err, http.StatusNotFound, "NoSuchKey")
	_, err = f.client.GetObject(t.Context(), &s3.GetObjectInput{Bucket: aws.String("docs"), Key: aws.String("k"), VersionId: del.VersionId})
	wantError(t, err, http.StatusMethodNotAllowed, "MethodNotAllowed")
	if list, err := f.client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String("docs")}); err != nil || len(list.Contents) != 0 {
		t.Errorf("a key under a delete marker is listed: %+v, %v", list.Contents, err)
	}

	versions, err := f.client.ListObjectVersions(t.Context(), &s3.ListObjectVersionsInput{Bucket: aws.String("docs")})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, v := range versions.Versions {
		got = append(got, aws.ToString(v.VersionId))
		if aws.ToBool(v.IsLatest) {
			t.Errorf("version %s is latest under a delete marker", aws.ToString(v.VersionId))
		}
	}
	if diff := cmp.Diff([]string{*v2, *v1, "null"}, got); diff != "" {
		t.Errorf("versions, newest first (-want +got):\n%s", diff)
	}
	if len(versions.DeleteMarkers) != 1 || !aws.ToBool(versions.DeleteMarkers[0].IsLatest) {
		t.Errorf("delete markers = %+v", versions.DeleteMarkers)
	}

	// paging through versions one at a time meets each exactly once
	var paged []string
	var keyMarker, versionMarker *string
	for range 10 {
		page, err := f.client.ListObjectVersions(t.Context(), &s3.ListObjectVersionsInput{
			Bucket: aws.String("docs"), MaxKeys: aws.Int32(1), KeyMarker: keyMarker, VersionIdMarker: versionMarker,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, v := range page.Versions {
			paged = append(paged, aws.ToString(v.VersionId))
		}
		for _, d := range page.DeleteMarkers {
			paged = append(paged, aws.ToString(d.VersionId))
		}
		if !aws.ToBool(page.IsTruncated) {
			break
		}
		keyMarker, versionMarker = page.NextKeyMarker, page.NextVersionIdMarker
	}
	if diff := cmp.Diff([]string{*del.VersionId, *v2, *v1, "null"}, paged); diff != "" {
		t.Errorf("paged versions (-want +got):\n%s", diff)
	}

	// removing the delete marker brings the object back
	if _, err := f.client.DeleteObject(t.Context(), &s3.DeleteObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("k"), VersionId: del.VersionId,
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.get("docs", "k", nil); got != "two" {
		t.Errorf("after removing the delete marker: %q", got)
	}

	// suspended: writes replace the null version and keep the others
	setVersioning(types.BucketVersioningStatusSuspended)
	if id := aws.ToString(f.put("docs", "k", "suspended").VersionId); id != "null" {
		t.Errorf("a write under suspended versioning got version id %q", id)
	}
	if got := f.get("docs", "k", aws.String("null")); got != "suspended" {
		t.Errorf("the null version = %q", got)
	}
	if got := f.get("docs", "k", v1); got != "one" {
		t.Errorf("version 1 after suspension = %q", got)
	}
}

func TestDeleteObjects(t *testing.T) {
	f := newFixture(t)
	f.createBucket("docs", false)
	f.put("docs", "a", "x")
	f.put("docs", "b", "x")
	out, err := f.client.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
		Bucket: aws.String("docs"),
		Delete: &types.Delete{Objects: []types.ObjectIdentifier{
			{Key: aws.String("a")}, {Key: aws.String("missing")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Deleted) != 2 || len(out.Errors) != 0 {
		t.Errorf("deleted %+v, errors %+v: a missing key deletes fine", out.Deleted, out.Errors)
	}
	quiet, err := f.client.DeleteObjects(t.Context(), &s3.DeleteObjectsInput{
		Bucket: aws.String("docs"),
		Delete: &types.Delete{Quiet: aws.Bool(true), Objects: []types.ObjectIdentifier{{Key: aws.String("b")}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(quiet.Deleted) != 0 {
		t.Errorf("a quiet delete listed %+v", quiet.Deleted)
	}
	if list, err := f.client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String("docs")}); err != nil || len(list.Contents) != 0 {
		t.Errorf("left after the deletes: %+v, %v", list.Contents, err)
	}
}

func TestTagging(t *testing.T) {
	f := newFixture(t)
	f.createBucket("docs", false)
	f.put("docs", "k", "x")
	if _, err := f.client.PutObjectTagging(t.Context(), &s3.PutObjectTaggingInput{
		Bucket: aws.String("docs"), Key: aws.String("k"),
		Tagging: &types.Tagging{TagSet: []types.Tag{{Key: aws.String("a"), Value: aws.String("1")}}},
	}); err != nil {
		t.Fatal(err)
	}
	got, err := f.client.GetObjectTagging(t.Context(), &s3.GetObjectTaggingInput{Bucket: aws.String("docs"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.TagSet) != 1 || *got.TagSet[0].Key != "a" || *got.TagSet[0].Value != "1" {
		t.Errorf("tags = %+v", got.TagSet)
	}
	if _, err := f.client.DeleteObjectTagging(t.Context(), &s3.DeleteObjectTaggingInput{Bucket: aws.String("docs"), Key: aws.String("k")}); err != nil {
		t.Fatal(err)
	}
	if got, err := f.client.GetObjectTagging(t.Context(), &s3.GetObjectTaggingInput{Bucket: aws.String("docs"), Key: aws.String("k")}); err != nil || len(got.TagSet) != 0 {
		t.Errorf("tags after delete = %+v, %v", got.TagSet, err)
	}
	_, err = f.client.GetObjectTagging(t.Context(), &s3.GetObjectTaggingInput{Bucket: aws.String("docs"), Key: aws.String("missing")})
	wantError(t, err, http.StatusNotFound, "NoSuchKey")
}

func TestCopyObject(t *testing.T) {
	f := newFixture(t)
	f.createBucket("docs", false)
	f.createBucket("archive", false)
	if _, err := f.client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("src key"), Body: strings.NewReader("copy me"),
		ContentType: aws.String("text/plain"), Metadata: map[string]string{"k": "v"},
	}); err != nil {
		t.Fatal(err)
	}

	// across buckets, keeping the source's metadata
	if _, err := f.client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket: aws.String("archive"), Key: aws.String("kept"), CopySource: aws.String("docs/src key"),
	}); err != nil {
		t.Fatal(err)
	}
	head, err := f.client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String("archive"), Key: aws.String("kept")})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(head.ContentType) != "text/plain" || head.Metadata["k"] != "v" {
		t.Errorf("the copy lost the source's metadata: %+v", head)
	}
	if got := f.get("archive", "kept", nil); got != "copy me" {
		t.Errorf("copied body = %q", got)
	}

	// replacing them
	if _, err := f.client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket: aws.String("archive"), Key: aws.String("replaced"), CopySource: aws.String("docs/src key"),
		MetadataDirective: types.MetadataDirectiveReplace,
		ContentType:       aws.String("application/json"), Metadata: map[string]string{"new": "1"},
	}); err != nil {
		t.Fatal(err)
	}
	head, err = f.client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String("archive"), Key: aws.String("replaced")})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToString(head.ContentType) != "application/json" || head.Metadata["new"] != "1" || head.Metadata["k"] != "" {
		t.Errorf("REPLACE did not replace the metadata: %+v", head)
	}

	// onto itself with nothing changed is refused, as on S3
	_, err = f.client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("src key"), CopySource: aws.String("docs/src key"),
	})
	wantError(t, err, http.StatusBadRequest, "InvalidRequest")

	_, err = f.client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket: aws.String("archive"), Key: aws.String("x"), CopySource: aws.String("docs/missing"),
	})
	wantError(t, err, http.StatusNotFound, "NoSuchKey")
	_, err = f.client.CopyObject(t.Context(), &s3.CopyObjectInput{
		Bucket: aws.String("archive"), Key: aws.String("x"), CopySource: aws.String("docs/src key"),
		CopySourceIfNoneMatch: head.ETag,
	})
	wantError(t, err, http.StatusPreconditionFailed, "PreconditionFailed")
}

func TestMultipart(t *testing.T) {
	f := newFixture(t)
	f.createBucket("docs", false)
	const partSize = 5 * 1024 * 1024
	part1 := bytes.Repeat([]byte("a"), partSize)
	part2 := []byte("the last part may be small")

	create, err := f.client.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String("docs"), Key: aws.String("big"), ContentType: aws.String("application/x-big"),
		ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
	})
	if err != nil {
		t.Fatal(err)
	}
	if create.ChecksumType != types.ChecksumTypeComposite {
		t.Errorf("CRC32 defaults to a COMPOSITE checksum, got %q", create.ChecksumType)
	}
	upload := func(n int32, data []byte) types.CompletedPart {
		t.Helper()
		out, err := f.client.UploadPart(t.Context(), &s3.UploadPartInput{
			Bucket: aws.String("docs"), Key: aws.String("big"), UploadId: create.UploadId,
			PartNumber: aws.Int32(n), Body: bytes.NewReader(data), ChecksumAlgorithm: types.ChecksumAlgorithmCrc32,
		})
		if err != nil {
			t.Fatal(err)
		}
		return types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.ETag, ChecksumCRC32: out.ChecksumCRC32}
	}
	p1, p2 := upload(1, part1), upload(2, part2)

	uploads, err := f.client.ListMultipartUploads(t.Context(), &s3.ListMultipartUploadsInput{Bucket: aws.String("docs")})
	if err != nil {
		t.Fatal(err)
	}
	if len(uploads.Uploads) != 1 || aws.ToString(uploads.Uploads[0].UploadId) != aws.ToString(create.UploadId) {
		t.Errorf("uploads in progress = %+v", uploads.Uploads)
	}
	parts, err := f.client.ListParts(t.Context(), &s3.ListPartsInput{
		Bucket: aws.String("docs"), Key: aws.String("big"), UploadId: create.UploadId, MaxParts: aws.Int32(1),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts.Parts) != 1 || !aws.ToBool(parts.IsTruncated) || aws.ToString(parts.NextPartNumberMarker) != "1" {
		t.Errorf("first page of parts = %+v (truncated %v, next %q)", parts.Parts, aws.ToBool(parts.IsTruncated), aws.ToString(parts.NextPartNumberMarker))
	}

	complete := func(parts ...types.CompletedPart) (*s3.CompleteMultipartUploadOutput, error) {
		return f.client.CompleteMultipartUpload(t.Context(), &s3.CompleteMultipartUploadInput{
			Bucket: aws.String("docs"), Key: aws.String("big"), UploadId: create.UploadId,
			MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
		})
	}
	// a part that was never uploaded, and one named by another ETag
	_, err = complete(p2, types.CompletedPart{PartNumber: aws.Int32(3), ETag: p2.ETag})
	wantError(t, err, http.StatusBadRequest, "InvalidPart")
	wrong := p1
	wrong.ETag = aws.String(`"0123456789abcdef0123456789abcdef"`)
	_, err = complete(wrong, p2)
	wantError(t, err, http.StatusBadRequest, "InvalidPart")

	done, err := complete(p1, p2)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(aws.ToString(done.ETag), `-2"`) {
		t.Errorf("multipart ETag = %s, want the -2 suffix", aws.ToString(done.ETag))
	}
	if !strings.HasSuffix(aws.ToString(done.ChecksumCRC32), "-2") || done.ChecksumType != types.ChecksumTypeComposite {
		t.Errorf("composite checksum = %q type %q", aws.ToString(done.ChecksumCRC32), done.ChecksumType)
	}

	head, err := f.client.HeadObject(t.Context(), &s3.HeadObjectInput{Bucket: aws.String("docs"), Key: aws.String("big")})
	if err != nil {
		t.Fatal(err)
	}
	if aws.ToInt64(head.ContentLength) != int64(len(part1)+len(part2)) || aws.ToString(head.ContentType) != "application/x-big" {
		t.Errorf("assembled object: %d bytes, type %q", aws.ToInt64(head.ContentLength), aws.ToString(head.ContentType))
	}
	if head.PartsCount != nil {
		t.Errorf("a read of the whole object reported a parts count")
	}
	second, err := f.client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("docs"), Key: aws.String("big"), PartNumber: aws.Int32(2),
		ChecksumMode: types.ChecksumModeEnabled,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer second.Body.Close()
	if b, _ := io.ReadAll(second.Body); !bytes.Equal(b, part2) || aws.ToInt32(second.PartsCount) != 2 {
		t.Errorf("part 2 = %q, parts count %d", b, aws.ToInt32(second.PartsCount))
	}
	// a read of one part carries that part's checksum
	if aws.ToString(second.ChecksumCRC32) != aws.ToString(p2.ChecksumCRC32) {
		t.Errorf("part 2 checksum = %q, want %q", aws.ToString(second.ChecksumCRC32), aws.ToString(p2.ChecksumCRC32))
	}

	attrs, err := f.client.GetObjectAttributes(t.Context(), &s3.GetObjectAttributesInput{
		Bucket: aws.String("docs"), Key: aws.String("big"),
		ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesObjectParts, types.ObjectAttributesChecksum, types.ObjectAttributesObjectSize},
	})
	if err != nil {
		t.Fatal(err)
	}
	if attrs.ObjectParts == nil || aws.ToInt32(attrs.ObjectParts.TotalPartsCount) != 2 || len(attrs.ObjectParts.Parts) != 2 {
		t.Errorf("object parts = %+v", attrs.ObjectParts)
	}
	// this view alone gives the composite checksum without its part count
	if attrs.Checksum == nil || aws.ToString(attrs.Checksum.ChecksumCRC32)+"-2" != aws.ToString(done.ChecksumCRC32) {
		t.Errorf("attributes checksum = %+v, completed %q", attrs.Checksum, aws.ToString(done.ChecksumCRC32))
	}

	// the upload is gone: aborting it again is a no-op, an id that never
	// existed is not
	if _, err := f.client.AbortMultipartUpload(t.Context(), &s3.AbortMultipartUploadInput{
		Bucket: aws.String("docs"), Key: aws.String("big"), UploadId: create.UploadId,
	}); err != nil {
		t.Errorf("aborting a completed upload: %v", err)
	}
	_, err = f.client.AbortMultipartUpload(t.Context(), &s3.AbortMultipartUploadInput{
		Bucket: aws.String("docs"), Key: aws.String("big"), UploadId: aws.String("never-existed"),
	})
	wantError(t, err, http.StatusNotFound, "NoSuchUpload")
	_, err = f.client.ListParts(t.Context(), &s3.ListPartsInput{
		Bucket: aws.String("docs"), Key: aws.String("big"), UploadId: create.UploadId,
	})
	wantError(t, err, http.StatusNotFound, "NoSuchUpload")
}

func TestMultipartPartTooSmall(t *testing.T) {
	f := newFixture(t)
	f.createBucket("docs", false)
	create, err := f.client.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{Bucket: aws.String("docs"), Key: aws.String("k")})
	if err != nil {
		t.Fatal(err)
	}
	var parts []types.CompletedPart
	for n := int32(1); n <= 2; n++ {
		out, err := f.client.UploadPart(t.Context(), &s3.UploadPartInput{
			Bucket: aws.String("docs"), Key: aws.String("k"), UploadId: create.UploadId,
			PartNumber: aws.Int32(n), Body: strings.NewReader("small"),
		})
		if err != nil {
			t.Fatal(err)
		}
		parts = append(parts, types.CompletedPart{PartNumber: aws.Int32(n), ETag: out.ETag})
	}
	_, err = f.client.CompleteMultipartUpload(t.Context(), &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("docs"), Key: aws.String("k"), UploadId: create.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: parts},
	})
	wantError(t, err, http.StatusBadRequest, "EntityTooSmall")
}

func TestUploadPartCopy(t *testing.T) {
	f := newFixture(t)
	f.createBucket("docs", false)
	f.put("docs", "src", "0123456789")
	create, err := f.client.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{Bucket: aws.String("docs"), Key: aws.String("dst")})
	if err != nil {
		t.Fatal(err)
	}
	part, err := f.client.UploadPartCopy(t.Context(), &s3.UploadPartCopyInput{
		Bucket: aws.String("docs"), Key: aws.String("dst"), UploadId: create.UploadId, PartNumber: aws.Int32(1),
		CopySource: aws.String("docs/src"), CopySourceRange: aws.String("bytes=2-5"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.client.CompleteMultipartUpload(t.Context(), &s3.CompleteMultipartUploadInput{
		Bucket: aws.String("docs"), Key: aws.String("dst"), UploadId: create.UploadId,
		MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{
			{PartNumber: aws.Int32(1), ETag: part.CopyPartResult.ETag},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := f.get("docs", "dst", nil); got != "2345" {
		t.Errorf("copied range = %q", got)
	}
	_, err = f.client.UploadPartCopy(t.Context(), &s3.UploadPartCopyInput{
		Bucket: aws.String("docs"), Key: aws.String("dst"), UploadId: create.UploadId, PartNumber: aws.Int32(1),
		CopySource: aws.String("docs/src"),
	})
	wantError(t, err, http.StatusNotFound, "NoSuchUpload")
}
