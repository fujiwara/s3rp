package s3gw_test

import (
	"errors"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/fujiwara/s3rp/checksum"
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

// The support table in docs/s3-api.md is written by hand; it must say what
// the code does.
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
	for _, a := range checksum.Algorithms() {
		want = append(want, a.Name+" "+a.Support.String())
	}
	if diff := cmp.Diff(want, documented); diff != "" {
		t.Errorf("docs/s3-api.md checksum table differs from checksum.Algorithms (-code +docs):\n%s", diff)
	}
}

// refusedAlgorithm returns an algorithm the table refuses, skipping the
// test when none is (every algorithm supported leaves nothing to refuse).
func refusedAlgorithm(t *testing.T) checksum.Algorithm {
	t.Helper()
	for _, a := range checksum.Algorithms() {
		if a.Support == checksum.Refused {
			return a
		}
	}
	t.Skip("no algorithm is refused")
	return checksum.Algorithm{}
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

// Whatever an algorithm's Support, a checksum the backend reports reaches
// the client: the conversions are generated for the whole table.
func TestChecksumsRelayedForEveryAlgorithm(t *testing.T) {
	for _, a := range checksum.Algorithms() {
		t.Run(a.Name, func(t *testing.T) {
			out := &s3.HeadObjectOutput{ContentLength: aws.Int64(0)}
			setHeadValue(out, a.Name, "dmFsdWU=")
			stub := &stubBackend{headOut: out}
			client, _ := newTestProxy(t, stub)
			got, err := client.HeadObject(t.Context(), &s3.HeadObjectInput{
				Bucket: aws.String("testbucket"), Key: aws.String("k"), ChecksumMode: types.ChecksumModeEnabled,
			})
			if err != nil {
				t.Fatal(err)
			}
			if v := headValue(got, a.Name); v != "dmFsdWU=" {
				t.Errorf("expect the backend's %s value, got %q", a.Name, v)
			}
		})
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
