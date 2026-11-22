package membackend

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"reflect"
	"strings"

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

func computeChecksum(name string, data []byte) (string, bool) {
	h := checksum.NewHash(name)
	if h == nil {
		return "", false
	}
	h.Write(data)
	return checksum.Base64(h), true
}

// writeChecksums settles the checksums stored with data: each value the
// client supplied is verified where the algorithm can be computed (and kept
// as given where it cannot), and the algorithm a write names without a
// value is computed.
func writeChecksums(data []byte, supplied checksum.Values, algorithm types.ChecksumAlgorithm) (checksum.Values, *failure) {
	var v checksum.Values
	set := func(name, value string) {
		if v == nil {
			v = checksum.Values{}
		}
		v[name] = value
	}
	for name, want := range supplied {
		got, ok := computeChecksum(name, data)
		if ok && got != want {
			return nil, fail(http.StatusBadRequest, "BadDigest",
				fmt.Sprintf("The %s you specified did not match the calculated checksum.", name))
		}
		set(name, want)
	}
	if name := strings.ToUpper(string(algorithm)); name != "" && v[name] == "" {
		if got, ok := computeChecksum(name, data); ok {
			set(name, got)
		}
	}
	return v, nil
}

// checkContentMD5 verifies a Content-MD5 the write carries.
func checkContentMD5(data []byte, contentMD5 *string) *failure {
	if contentMD5 == nil {
		return nil
	}
	want, err := base64.StdEncoding.DecodeString(*contentMD5)
	if err != nil || len(want) != md5.Size {
		return fail(http.StatusBadRequest, "InvalidDigest", "The Content-MD5 you specified was invalid.")
	}
	if got := md5.Sum(data); string(got[:]) != string(want) {
		return fail(http.StatusBadRequest, "BadDigest", "The Content-MD5 you specified did not match what we received.")
	}
	return nil
}

func md5ETag(data []byte) string {
	sum := md5.Sum(data)
	return `"` + hex.EncodeToString(sum[:]) + `"`
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
