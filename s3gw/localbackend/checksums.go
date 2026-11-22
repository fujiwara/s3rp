package localbackend

import (
	"fmt"
	"net/http"
	"reflect"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"

	"github.com/fujiwara/s3rp/checksum"
)

// The SDK spells a checksum value as a *string field named "Checksum" plus
// the algorithm's name on every input and output that carries one, so the
// conversions go by field name instead of being written out per type.

// checksumsOf reads the checksum fields of an SDK struct (by pointer).
func checksumsOf(src any) checksum.Values {
	rv := reflect.ValueOf(src).Elem()
	var v checksum.Values
	for _, a := range checksum.Algorithms() {
		f := rv.FieldByName("Checksum" + a.Name)
		if !f.IsValid() || f.IsNil() {
			continue
		}
		if s := f.Elem().String(); s != "" {
			if v == nil {
				v = checksum.Values{}
			}
			v[a.Name] = s
		}
	}
	return v
}

// setChecksums writes values into the checksum fields of an SDK struct (by
// pointer).
func setChecksums(dst any, v checksum.Values) {
	rv := reflect.ValueOf(dst).Elem()
	for name, s := range v {
		if f := rv.FieldByName("Checksum" + name); f.IsValid() && s != "" {
			f.Set(reflect.ValueOf(aws.String(s)))
		}
	}
}

// unsupportedChecksum refuses an algorithm this package cannot compute: a
// value stored unverified, or a named algorithm silently not applied, would
// report an integrity check that never happened.
func unsupportedChecksum(name string) *failure {
	return fail(http.StatusNotImplemented, "NotImplemented",
		fmt.Sprintf("The %s checksum algorithm is not supported.", name))
}

func badChecksum(name string) *failure {
	return fail(http.StatusBadRequest, "BadDigest",
		fmt.Sprintf("The %s you specified did not match the calculated checksum.", name))
}

// checksumAlgorithms is the listing form of an object's checksums.
func checksumAlgorithms(v checksum.Values) []types.ChecksumAlgorithm {
	var out []types.ChecksumAlgorithm
	for _, a := range checksum.Algorithms() {
		if v[a.Name] != "" {
			out = append(out, types.ChecksumAlgorithm(a.Name))
		}
	}
	return out
}
