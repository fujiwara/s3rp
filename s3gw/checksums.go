package s3gw

import (
	"net/http"

	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp/checksum"
	"github.com/fujiwara/s3rp/s3err"
)

// The per-algorithm conversions (checksumsFrom*, setChecksums*, the s3xml
// Checksums) are generated for every algorithm in checksum.Algorithms;
// which algorithms a request may use is decided here, from each one's
// Support, and nowhere else.
//go:generate go run ./internal/checksumgen/cmd/checksumgen

func init() {
	// a Refused algorithm's value header stays unknown, so it meets the 501
	// of checkKnownAmzHeaders like any header the gateway does not handle
	for _, a := range checksum.Algorithms() {
		if a.Support != checksum.Refused {
			knownAmzHeaders[a.Header()] = true
		}
	}
}

// checksumAlgorithm reads x-amz-checksum-algorithm, the name a client asks
// the backend to compute (CreateMultipartUpload, CopyObject). A Refused
// algorithm is refused here rather than passed on, the same as its value
// header would be.
func (c *opCtx) checksumAlgorithm() (types.ChecksumAlgorithm, *s3err.Error) {
	v := c.signed("x-amz-checksum-algorithm")
	if v == "" {
		return "", nil
	}
	a, ok := checksum.Lookup(v)
	if !ok {
		return "", s3err.New(http.StatusBadRequest, "InvalidRequest",
			"Value for x-amz-checksum-algorithm header is invalid.")
	}
	if a.Support == checksum.Refused {
		return "", s3err.NotImplemented("checksum algorithm " + a.Name)
	}
	return types.ChecksumAlgorithm(a.Name), nil
}

// checkChecksums refuses values of a Refused algorithm that arrive outside
// a header (the parts of a CompleteMultipartUpload body), which
// checkKnownAmzHeaders never sees.
func checkChecksums(v checksum.Values) *s3err.Error {
	for name := range v {
		if a, _ := checksum.Lookup(name); a.Support == checksum.Refused {
			return s3err.NotImplemented("checksum algorithm " + a.Name)
		}
	}
	return nil
}
