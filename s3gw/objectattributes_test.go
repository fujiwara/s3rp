package s3gw_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	v4 "github.com/aws/aws-sdk-go-v2/aws/signer/v4"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/google/go-cmp/cmp"
)

func TestGetObjectAttributes(t *testing.T) {
	lm := time.Date(2026, 8, 11, 1, 2, 3, 0, time.UTC)
	stub := &stubBackend{getAttrOut: &s3.GetObjectAttributesOutput{
		ETag:         aws.String(`"etag-value"`),
		LastModified: aws.Time(lm),
		VersionId:    aws.String("v1"),
		ObjectSize:   aws.Int64(1024),
		StorageClass: types.StorageClassStandard,
		Checksum: &types.Checksum{
			ChecksumSHA256: aws.String("sha256sum"),
			ChecksumType:   types.ChecksumTypeComposite,
		},
		ObjectParts: &types.GetObjectAttributesParts{
			IsTruncated:     aws.Bool(false),
			MaxParts:        aws.Int32(1000),
			TotalPartsCount: aws.Int32(2),
			Parts: []types.ObjectPart{
				{PartNumber: aws.Int32(1), Size: aws.Int64(512), ChecksumSHA256: aws.String("p1sum")},
				{PartNumber: aws.Int32(2), Size: aws.Int64(512), ChecksumSHA256: aws.String("p2sum")},
			},
		},
	}}
	client, _ := newTestProxy(t, stub)

	out, err := client.GetObjectAttributes(t.Context(), &s3.GetObjectAttributesInput{
		Bucket: aws.String("testbucket"),
		Key:    aws.String("k"),
		ObjectAttributes: []types.ObjectAttributes{
			types.ObjectAttributesEtag, types.ObjectAttributesChecksum,
			types.ObjectAttributesObjectParts, types.ObjectAttributesStorageClass,
			types.ObjectAttributesObjectSize,
		},
		MaxParts:         aws.Int32(1000),
		PartNumberMarker: aws.String("0"),
		VersionId:        aws.String("v1"),
	})
	if err != nil {
		t.Fatal(err)
	}

	// the request reached the backend with everything forwarded
	in := stub.getAttrIn
	if in == nil {
		t.Fatal("backend not called")
	}
	if aws.ToString(in.Bucket) != "backend-testbucket" {
		t.Errorf("expect the backend bucket name, got %s", aws.ToString(in.Bucket))
	}
	if len(in.ObjectAttributes) != 5 {
		t.Errorf("attributes not forwarded: %v", in.ObjectAttributes)
	}
	if aws.ToInt32(in.MaxParts) != 1000 || aws.ToString(in.PartNumberMarker) != "0" || aws.ToString(in.VersionId) != "v1" {
		t.Errorf("part paging/version not forwarded: %+v", in)
	}

	// this API returns the ETag without quotes
	if got := aws.ToString(out.ETag); got != "etag-value" {
		t.Errorf("expect unquoted etag-value, got %q", got)
	}
	if aws.ToInt64(out.ObjectSize) != 1024 || out.StorageClass != types.StorageClassStandard {
		t.Errorf("size/storage class not returned: %+v", out)
	}
	if out.Checksum == nil || aws.ToString(out.Checksum.ChecksumSHA256) != "sha256sum" {
		t.Errorf("checksum not returned: %+v", out.Checksum)
	}
	if out.Checksum == nil || out.Checksum.ChecksumType != types.ChecksumTypeComposite {
		t.Errorf("checksum type not returned: %+v", out.Checksum)
	}
	if out.ObjectParts == nil || aws.ToInt32(out.ObjectParts.TotalPartsCount) != 2 {
		t.Fatalf("object parts not returned: %+v", out.ObjectParts)
	}
	var partNums []int32
	for _, p := range out.ObjectParts.Parts {
		partNums = append(partNums, aws.ToInt32(p.PartNumber))
	}
	if diff := cmp.Diff([]int32{1, 2}, partNums); diff != "" {
		t.Errorf("parts (-want +got):\n%s", diff)
	}
	if aws.ToString(out.VersionId) != "v1" {
		t.Errorf("version id header not returned: %+v", out.VersionId)
	}
	if got := aws.ToTime(out.LastModified); !got.Equal(lm) {
		t.Errorf("last modified not returned: %v", got)
	}
}

func TestGetObjectAttributesErrors(t *testing.T) {
	t.Run("backend 404 relays the delete marker", func(t *testing.T) {
		stub := &stubBackend{getAttrErr: backendError(http.StatusNotFound, http.Header{
			"X-Amz-Delete-Marker": []string{"true"},
		})}
		client, _ := newTestProxy(t, stub)
		_, err := client.GetObjectAttributes(t.Context(), &s3.GetObjectAttributesInput{
			Bucket:           aws.String("testbucket"),
			Key:              aws.String("k"),
			ObjectAttributes: []types.ObjectAttributes{types.ObjectAttributesEtag},
		})
		var apiErr smithy.APIError
		if !errors.As(err, &apiErr) {
			t.Fatalf("expected an API error, got %v", err)
		}
	})

	t.Run("missing attributes header refused", func(t *testing.T) {
		// the SDK always sends the header, so sign a bare request by hand
		stub := &stubBackend{}
		_, ts := newTestProxy(t, stub)

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, ts.URL+"/testbucket/k?attributes", nil)
		if err != nil {
			t.Fatal(err)
		}
		const emptySHA = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		req.Header.Set("x-amz-content-sha256", emptySHA)
		signer := v4.NewSigner(func(o *v4.SignerOptions) { o.DisableURIPathEscaping = true })
		creds := aws.Credentials{AccessKeyID: testAccessKeyID, SecretAccessKey: testSecretAccessKey}
		if err := signer.SignHTTP(context.Background(), creds, req, emptySHA, "s3", "us-east-1", time.Now()); err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400 without x-amz-object-attributes, got %d: %s", resp.StatusCode, body)
		}
		if stub.getAttrIn != nil {
			t.Error("backend must not be called without attributes")
		}
	})
}
