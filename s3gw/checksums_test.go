package s3gw_test

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp/checksum"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/s3gw/internal/checksumgen"
	"github.com/google/go-cmp/cmp"
)

// The generated conversions must match the checksum table; run go generate
// in s3gw after changing the algorithm list.
func TestChecksumsGenerated(t *testing.T) {
	files, err := checksumgen.Files()
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range files {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if diff := cmp.Diff(string(want), string(got)); diff != "" {
			t.Errorf("%s is stale; run go generate ./s3gw (-want +got):\n%s", path, diff)
		}
	}
}

// Every algorithm the SDK knows must have a row in the table, and the
// table nothing the SDK does not know: an algorithm S3 adds must get a
// Support decision, not a default.
func TestChecksumTableCoversSDK(t *testing.T) {
	var sdk, table []string
	for _, a := range types.ChecksumAlgorithm("").Values() {
		sdk = append(sdk, string(a))
	}
	for _, a := range checksum.Algorithms() {
		table = append(table, a.Name)
	}
	slices.Sort(sdk)
	slices.Sort(table)
	if diff := cmp.Diff(sdk, table); diff != "" {
		t.Errorf("checksum table differs from the SDK's algorithms (-sdk +table):\n%s", diff)
	}
}

// The default support table in docs/s3-api.md is written by hand; it must
// say what the code does.
func TestChecksumTableDocumented(t *testing.T) {
	doc, err := os.ReadFile("../docs/s3-api.md")
	if err != nil {
		t.Fatal(err)
	}
	row := regexp.MustCompile("(?m)^\\| `([A-Z0-9]+)` \\| (verified|forwarded|refused) \\|$")
	var documented, want []string
	for _, m := range row.FindAllStringSubmatch(string(doc), -1) {
		documented = append(documented, m[1]+" "+m[2])
	}
	def := s3gw.DefaultChecksumSupport()
	for _, a := range checksum.Algorithms() {
		want = append(want, a.Name+" "+def[a.Name].String())
	}
	if diff := cmp.Diff(want, documented); diff != "" {
		t.Errorf("docs/s3-api.md checksum table differs from DefaultChecksumSupport (-code +docs):\n%s", diff)
	}
}

// refusedAlgorithm returns an algorithm the default support refuses,
// skipping the test when none is.
func refusedAlgorithm(t *testing.T) checksum.Algorithm {
	t.Helper()
	def := s3gw.DefaultChecksumSupport()
	for _, a := range checksum.Algorithms() {
		if def[a.Name] == s3gw.ChecksumRefused {
			return a
		}
	}
	t.Skip("no algorithm is refused")
	return checksum.Algorithm{}
}

// newChecksumProxy serves stub through a gateway with the given checksum
// support, set before the server starts.
func newChecksumProxy(t *testing.T, stub *stubBackend, support map[string]s3gw.ChecksumSupport) *s3.Client {
	t.Helper()
	gw := newTestGateway(t)
	if err := gw.SetChecksumSupport(support); err != nil {
		t.Fatal(err)
	}
	if err := gw.SetBackend("testbucket", stub); err != nil {
		t.Fatal(err)
	}
	client, _, _ := newSDKClientFor(t, gw)
	return client
}

func expectAPIError(t *testing.T, err error, code string) {
	t.Helper()
	var ae smithy.APIError
	if !errors.As(err, &ae) || ae.ErrorCode() != code {
		t.Fatalf("expect %s, got %v", code, err)
	}
}

// A refused algorithm is refused in every form a request can name it.
func TestRefusedChecksumAlgorithm(t *testing.T) {
	refused := refusedAlgorithm(t)
	stub := &stubBackend{
		createMPUOut:   &s3.CreateMultipartUploadOutput{UploadId: aws.String("u")},
		completeMPUOut: &s3.CompleteMultipartUploadOutput{ETag: aws.String(`"e"`)},
		copyOut:        &s3.CopyObjectOutput{CopyObjectResult: &types.CopyObjectResult{ETag: aws.String(`"e"`)}},
		putOut:         &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
	}
	client, _ := newTestProxy(t, stub)
	ctx := t.Context()
	bucket := aws.String("testbucket")
	alg := types.ChecksumAlgorithm(refused.Name)

	t.Run("CreateMultipartUpload", func(t *testing.T) {
		_, err := client.CreateMultipartUpload(ctx, &s3.CreateMultipartUploadInput{
			Bucket: bucket, Key: aws.String("k"), ChecksumAlgorithm: alg,
		})
		expectAPIError(t, err, "NotImplemented")
		if stub.createMPUIn != nil {
			t.Error("the refused request reached the backend")
		}
	})
	t.Run("CopyObject", func(t *testing.T) {
		_, err := client.CopyObject(ctx, &s3.CopyObjectInput{
			Bucket: bucket, Key: aws.String("dst"), CopySource: aws.String("testbucket/src"), ChecksumAlgorithm: alg,
		})
		expectAPIError(t, err, "NotImplemented")
		if stub.copyIn != nil {
			t.Error("the refused request reached the backend")
		}
	})
	t.Run("value header", func(t *testing.T) {
		req := &s3.PutObjectInput{Bucket: bucket, Key: aws.String("k"), Body: strings.NewReader("x")}
		setValue(req, refused.Name, "AAAA")
		_, err := client.PutObject(ctx, req)
		expectAPIError(t, err, "NotImplemented")
		if stub.putIn != nil {
			t.Error("the refused request reached the backend")
		}
	})
	t.Run("CompleteMultipartUpload part", func(t *testing.T) {
		part := types.CompletedPart{PartNumber: aws.Int32(1), ETag: aws.String(`"p"`)}
		setPartValue(&part, refused.Name, "AAAA")
		_, err := client.CompleteMultipartUpload(ctx, &s3.CompleteMultipartUploadInput{
			Bucket: bucket, Key: aws.String("k"), UploadId: aws.String("u"),
			MultipartUpload: &types.CompletedMultipartUpload{Parts: []types.CompletedPart{part}},
		})
		expectAPIError(t, err, "NotImplemented")
		if stub.completeMPUIn != nil {
			t.Error("the refused request reached the backend")
		}
	})
}

func TestUnknownChecksumAlgorithm(t *testing.T) {
	stub := &stubBackend{}
	client, _ := newTestProxy(t, stub)
	_, err := client.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String("testbucket"), Key: aws.String("k"), ChecksumAlgorithm: "CRC16",
	})
	expectAPIError(t, err, "InvalidRequest")
	if stub.createMPUIn != nil {
		t.Error("the refused request reached the backend")
	}
}

// A checksum the backend reports reaches the client exactly when the
// service offers its algorithm: clients see the same set whatever backend
// serves the bucket.
func TestChecksumsReportedByPolicy(t *testing.T) {
	for _, a := range checksum.Algorithms() {
		t.Run(a.Name, func(t *testing.T) {
			for _, offered := range []bool{true, false} {
				support := s3gw.DefaultChecksumSupport()
				support[a.Name] = s3gw.ChecksumRefused
				if offered {
					support[a.Name] = s3gw.ChecksumForwarded
				}
				out := &s3.HeadObjectOutput{ContentLength: aws.Int64(0)}
				setHeadValue(out, a.Name, "dmFsdWU=")
				client := newChecksumProxy(t, &stubBackend{headOut: out}, support)
				got, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{
					Bucket: aws.String("testbucket"), Key: aws.String("k"), ChecksumMode: types.ChecksumModeEnabled,
				})
				if err != nil {
					t.Fatal(err)
				}
				want := ""
				if offered {
					want = "dmFsdWU="
				}
				if v := headValue(got, a.Name); v != want {
					t.Errorf("offered=%v: expect %q, got %q", offered, want, v)
				}
			}
		})
	}
}

func TestChecksumsFilteredInListings(t *testing.T) {
	stub := &stubBackend{
		listOut: &s3.ListObjectsV2Output{Contents: []types.Object{{
			Key: aws.String("k"), LastModified: aws.Time(time.Unix(0, 0)),
			ChecksumAlgorithm: []types.ChecksumAlgorithm{types.ChecksumAlgorithmXxhash3, types.ChecksumAlgorithmCrc32},
		}}},
		listPartsOut: &s3.ListPartsOutput{ChecksumAlgorithm: types.ChecksumAlgorithmXxhash3},
	}
	support := s3gw.DefaultChecksumSupport()
	support["XXHASH3"] = s3gw.ChecksumRefused
	client := newChecksumProxy(t, stub, support)
	out, err := client.ListObjectsV2(t.Context(), &s3.ListObjectsV2Input{Bucket: aws.String("testbucket")})
	if err != nil {
		t.Fatal(err)
	}
	if diff := cmp.Diff([]types.ChecksumAlgorithm{types.ChecksumAlgorithmCrc32}, out.Contents[0].ChecksumAlgorithm); diff != "" {
		t.Errorf("ListObjectsV2 algorithms (-want +got):\n%s", diff)
	}
	parts, err := client.ListParts(t.Context(), &s3.ListPartsInput{
		Bucket: aws.String("testbucket"), Key: aws.String("k"), UploadId: aws.String("u"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if parts.ChecksumAlgorithm != "" {
		t.Errorf("expect a refused algorithm hidden, got %q", parts.ChecksumAlgorithm)
	}
}

func TestSetChecksumSupport(t *testing.T) {
	gw := newTestGateway(t)
	for _, tc := range []struct {
		name    string
		support map[string]s3gw.ChecksumSupport
		ok      bool
	}{
		{"default", s3gw.DefaultChecksumSupport(), true},
		{"empty refuses everything", map[string]s3gw.ChecksumSupport{}, true},
		{"canonical name", map[string]s3gw.ChecksumSupport{"SHA512": s3gw.ChecksumVerified}, true},
		{"other spelling", map[string]s3gw.ChecksumSupport{"sha512": s3gw.ChecksumVerified}, false},
		{"forwarded without an implementation", map[string]s3gw.ChecksumSupport{"XXHASH3": s3gw.ChecksumForwarded}, true},
		{"unknown algorithm", map[string]s3gw.ChecksumSupport{"CRC16": s3gw.ChecksumForwarded}, false},
		{"verified without an implementation", map[string]s3gw.ChecksumSupport{"XXHASH3": s3gw.ChecksumVerified}, false},
		{"invalid support", map[string]s3gw.ChecksumSupport{"CRC32": 7}, false},
		{"one algorithm twice", map[string]s3gw.ChecksumSupport{"sha512": s3gw.ChecksumVerified, "SHA512": s3gw.ChecksumRefused}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := gw.SetChecksumSupport(tc.support); (err == nil) != tc.ok {
				t.Errorf("expect ok=%v, got %v", tc.ok, err)
			}
		})
	}
}

// SetChecksumSupport replaces the whole policy: an algorithm left out is
// refused, even one the default offers.
func TestSetChecksumSupportReplaces(t *testing.T) {
	stub := &stubBackend{putOut: &s3.PutObjectOutput{ETag: aws.String(`"e"`)}}
	client := newChecksumProxy(t, stub, map[string]s3gw.ChecksumSupport{"SHA256": s3gw.ChecksumVerified})
	_, err := client.PutObject(t.Context(), &s3.PutObjectInput{
		Bucket: aws.String("testbucket"), Key: aws.String("k"), Body: strings.NewReader("123456789"),
		ChecksumCRC32: aws.String("y/Q5Jg=="),
	})
	expectAPIError(t, err, "NotImplemented")
	if stub.putIn != nil {
		t.Error("the refused request reached the backend")
	}
}

// An algorithm the service offers is forwarded: the value header and the
// name reach the backend.
func TestForwardedChecksumAlgorithm(t *testing.T) {
	stub := &stubBackend{
		putOut:       &s3.PutObjectOutput{ETag: aws.String(`"e"`)},
		createMPUOut: &s3.CreateMultipartUploadOutput{UploadId: aws.String("u")},
	}
	support := s3gw.DefaultChecksumSupport()
	support["XXHASH3"] = s3gw.ChecksumForwarded
	client := newChecksumProxy(t, stub, support)
	req := &s3.PutObjectInput{Bucket: aws.String("testbucket"), Key: aws.String("k"), Body: strings.NewReader("x")}
	setValue(req, "XXHASH3", "AAAAAAAAAAA=")
	if _, err := client.PutObject(t.Context(), req); err != nil {
		t.Fatal(err)
	}
	if got := aws.ToString(stub.putIn.ChecksumXXHASH3); got != "AAAAAAAAAAA=" {
		t.Errorf("expect the value forwarded, got %q", got)
	}
	if stub.putIn.ChecksumAlgorithm != types.ChecksumAlgorithmXxhash3 {
		t.Errorf("expect the algorithm named alongside, got %q", stub.putIn.ChecksumAlgorithm)
	}
	if _, err := client.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String("testbucket"), Key: aws.String("m"), ChecksumAlgorithm: types.ChecksumAlgorithmXxhash3,
	}); err != nil {
		t.Fatal(err)
	}
	if stub.createMPUIn.ChecksumAlgorithm != types.ChecksumAlgorithmXxhash3 {
		t.Errorf("expect the name forwarded, got %q", stub.createMPUIn.ChecksumAlgorithm)
	}
}

func TestChecksumAlgorithmCaseInsensitive(t *testing.T) {
	stub := &stubBackend{createMPUOut: &s3.CreateMultipartUploadOutput{UploadId: aws.String("u")}}
	client, _ := newTestProxy(t, stub)
	if _, err := client.CreateMultipartUpload(t.Context(), &s3.CreateMultipartUploadInput{
		Bucket: aws.String("testbucket"), Key: aws.String("k"), ChecksumAlgorithm: "crc32c",
	}); err != nil {
		t.Fatal(err)
	}
	// the backend is sent the table's spelling
	if got := stub.createMPUIn.ChecksumAlgorithm; got != types.ChecksumAlgorithmCrc32c {
		t.Errorf("unexpected algorithm %q", got)
	}
}

// The helpers below spell each algorithm's SDK field by hand on purpose: a
// test built on the generated conversions could not catch a mistake in them.

func setValue(in *s3.PutObjectInput, name, v string) {
	p := aws.String(v)
	switch name {
	case "CRC32":
		in.ChecksumCRC32 = p
	case "CRC32C":
		in.ChecksumCRC32C = p
	case "CRC64NVME":
		in.ChecksumCRC64NVME = p
	case "SHA1":
		in.ChecksumSHA1 = p
	case "SHA256":
		in.ChecksumSHA256 = p
	case "SHA512":
		in.ChecksumSHA512 = p
	case "MD5":
		in.ChecksumMD5 = p
	case "XXHASH64":
		in.ChecksumXXHASH64 = p
	case "XXHASH3":
		in.ChecksumXXHASH3 = p
	case "XXHASH128":
		in.ChecksumXXHASH128 = p
	default:
		panic("unhandled algorithm " + name)
	}
}

func setPartValue(p *types.CompletedPart, name, v string) {
	s := aws.String(v)
	switch name {
	case "CRC32":
		p.ChecksumCRC32 = s
	case "CRC32C":
		p.ChecksumCRC32C = s
	case "CRC64NVME":
		p.ChecksumCRC64NVME = s
	case "SHA1":
		p.ChecksumSHA1 = s
	case "SHA256":
		p.ChecksumSHA256 = s
	case "SHA512":
		p.ChecksumSHA512 = s
	case "MD5":
		p.ChecksumMD5 = s
	case "XXHASH64":
		p.ChecksumXXHASH64 = s
	case "XXHASH3":
		p.ChecksumXXHASH3 = s
	case "XXHASH128":
		p.ChecksumXXHASH128 = s
	default:
		panic("unhandled algorithm " + name)
	}
}

func setHeadValue(o *s3.HeadObjectOutput, name, v string) {
	s := aws.String(v)
	switch name {
	case "CRC32":
		o.ChecksumCRC32 = s
	case "CRC32C":
		o.ChecksumCRC32C = s
	case "CRC64NVME":
		o.ChecksumCRC64NVME = s
	case "SHA1":
		o.ChecksumSHA1 = s
	case "SHA256":
		o.ChecksumSHA256 = s
	case "SHA512":
		o.ChecksumSHA512 = s
	case "MD5":
		o.ChecksumMD5 = s
	case "XXHASH64":
		o.ChecksumXXHASH64 = s
	case "XXHASH3":
		o.ChecksumXXHASH3 = s
	case "XXHASH128":
		o.ChecksumXXHASH128 = s
	default:
		panic("unhandled algorithm " + name)
	}
}

func headValue(o *s3.HeadObjectOutput, name string) string {
	var p *string
	switch name {
	case "CRC32":
		p = o.ChecksumCRC32
	case "CRC32C":
		p = o.ChecksumCRC32C
	case "CRC64NVME":
		p = o.ChecksumCRC64NVME
	case "SHA1":
		p = o.ChecksumSHA1
	case "SHA256":
		p = o.ChecksumSHA256
	case "SHA512":
		p = o.ChecksumSHA512
	case "MD5":
		p = o.ChecksumMD5
	case "XXHASH64":
		p = o.ChecksumXXHASH64
	case "XXHASH3":
		p = o.ChecksumXXHASH3
	case "XXHASH128":
		p = o.ChecksumXXHASH128
	default:
		panic("unhandled algorithm " + name)
	}
	return aws.ToString(p)
}

// Responses show checksums only through the policy's filter
// (setChecksumHeaders / reportChecksums in checksums.go); a direct
// checksum.SetHeaders or xmlChecksums elsewhere would bypass it.
func TestChecksumsReportedThroughPolicy(t *testing.T) {
	direct := regexp.MustCompile(`checksum\.SetHeaders\(|[^.]xmlChecksums\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || strings.HasSuffix(f, "_gen.go") || f == "checksums.go" {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if loc := direct.FindIndex(src); loc != nil {
			t.Errorf("%s reports checksums without the policy filter: %q", f, src[loc[0]:loc[1]])
		}
	}
}
