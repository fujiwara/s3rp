package membackend

import (
	"reflect"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/fujiwara/s3rp/checksum"
)

// The checksum conversions find the SDK's fields by name, so a field the
// SDK spells differently would be skipped without a word. Every type the
// package reads checksums from or writes them to must have one *string
// field per algorithm.
func TestChecksumFieldsByName(t *testing.T) {
	carriers := []any{
		s3.PutObjectInput{}, s3.PutObjectOutput{}, s3.GetObjectOutput{}, s3.HeadObjectOutput{},
		s3.UploadPartInput{}, s3.UploadPartOutput{},
		s3.CompleteMultipartUploadInput{}, s3.CompleteMultipartUploadOutput{},
		types.CompletedPart{}, types.Part{}, types.ObjectPart{}, types.Checksum{},
		types.CopyObjectResult{}, types.CopyPartResult{},
	}
	for _, c := range carriers {
		rt := reflect.TypeOf(c)
		for _, a := range checksum.Algorithms() {
			f, ok := rt.FieldByName("Checksum" + a.Name)
			if !ok || f.Type != reflect.TypeFor[*string]() {
				t.Errorf("%s has no *string field Checksum%s", rt, a.Name)
			}
		}
	}
}
