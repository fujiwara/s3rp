package harness

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/fujiwara/s3rp/s3err"
	"github.com/fujiwara/s3rp/sigv4"
	"github.com/fujiwara/s3rp/store"
)

// backendBuckets is the slice of the S3 client the interceptor needs;
// an interface so tests can stub the backend.
type backendBuckets interface {
	CreateBucket(ctx context.Context, in *s3.CreateBucketInput, opts ...func(*s3.Options)) (*s3.CreateBucketOutput, error)
	DeleteBucket(ctx context.Context, in *s3.DeleteBucketInput, opts ...func(*s3.Options)) (*s3.DeleteBucketOutput, error)
}

// bucketInterceptor handles CreateBucket and DeleteBucket in front of the
// gateway, emulating the control plane the s3-tests suite expects to reach
// through the S3 API. Everything else passes through untouched.
//
// Its two operations answer AWS-compatibly (404 NoSuchBucket for a missing
// bucket) rather than with the gateway's anti-probing 403 — they *are* the
// control plane here, and the suite's create/delete fixtures depend on the
// AWS answers. The gateway's own operations keep their 403.
type bucketInterceptor struct {
	verifier *sigv4.Verifier
	store    *memStore
	backend  backendBuckets
	next     http.Handler
	logger   *jsonLogger
}

func (i *bucketInterceptor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	bucket, ok := i.interceptedBucket(r)
	if !ok {
		i.next.ServeHTTP(w, r)
		return
	}

	requestID := newRequestID()
	w.Header().Set("x-amz-request-id", requestID)

	// Verify the signature and learn the key behind it. Verify reads only
	// headers/query, so on a fall-back path the request would still be
	// intact; the payload hash of the (ignored) CreateBucketConfiguration
	// body is deliberately not re-verified here.
	var key *store.Key
	_, s3e := i.verifier.Verify(r, func(ctx context.Context, accessKeyID, sessionToken string) (sigv4.Credential, error) {
		k, err := i.store.GetKey(ctx, accessKeyID, sessionToken)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return sigv4.Credential{}, fmt.Errorf("%w: %w", sigv4.ErrUnknownKey, err)
			}
			return sigv4.Credential{}, err
		}
		key = k
		return sigv4.Credential{SecretAccessKey: k.SecretAccessKey.String(), SessionToken: k.SessionToken}, nil
	})
	if s3e != nil {
		i.finish(w, r, bucket, "", requestID, s3e)
		return
	}

	switch r.Method {
	case http.MethodPut:
		i.createBucket(w, r, bucket, key, requestID)
	case http.MethodDelete:
		i.deleteBucket(w, r, bucket, key, requestID)
	}
}

// interceptedBucket reports whether the request is a CreateBucket or
// DeleteBucket the harness must handle: PUT or DELETE on a bare bucket
// path whose query contains nothing but the exemptions the gateway's own
// paramSet.check grants (the "x-id" SDK hint and "x-amz-*" auth params).
// A subresource query key (?acl, ?policy, ?cors, ...) falls through so the
// suite sees the gateway's genuine responses for those operations.
func (i *bucketInterceptor) interceptedBucket(r *http.Request) (string, bool) {
	if r.Method != http.MethodPut && r.Method != http.MethodDelete {
		return "", false
	}
	p := strings.TrimPrefix(r.URL.EscapedPath(), "/")
	rawBucket, rawKey, _ := strings.Cut(p, "/")
	if rawBucket == "" || rawKey != "" {
		return "", false
	}
	bucket, err := url.PathUnescape(rawBucket)
	if err != nil {
		return "", false // the gateway reports the invalid path
	}
	for k := range r.URL.Query() {
		if k == "x-id" || strings.HasPrefix(strings.ToLower(k), "x-amz-") {
			continue
		}
		return "", false
	}
	return bucket, true
}

func (i *bucketInterceptor) createBucket(w http.ResponseWriter, r *http.Request, bucket string, key *store.Key, requestID string) {
	// The CreateBucketConfiguration body (LocationConstraint) is read and
	// discarded: the gateway has exactly one region.
	io.Copy(io.Discard, r.Body)

	if err := store.ValidateBucketName(bucket); err != nil {
		i.finish(w, r, bucket, key.Tenant, requestID,
			s3err.New(http.StatusBadRequest, "InvalidBucketName", "The specified bucket is not valid.").WithCause(err))
		return
	}
	owner, claimed := i.store.claim(bucket, key.Tenant)
	if !claimed {
		if owner == key.Tenant {
			i.finish(w, r, bucket, key.Tenant, requestID,
				s3err.New(http.StatusConflict, "BucketAlreadyOwnedByYou", "The bucket that you tried to create already exists, and you own it."))
		} else {
			i.finish(w, r, bucket, key.Tenant, requestID,
				s3err.New(http.StatusConflict, "BucketAlreadyExists", "The requested bucket name is not available."))
		}
		return
	}

	in := &s3.CreateBucketInput{Bucket: aws.String(bucket)}
	if strings.EqualFold(r.Header.Get("x-amz-bucket-object-lock-enabled"), "true") {
		in.ObjectLockEnabledForBucket = aws.Bool(true)
	}
	// x-amz-acl / x-amz-grant-* headers are ignored, mimicking an
	// ACL-disabled bucket — the same stance the gateway takes on ACLs.
	if _, err := i.backend.CreateBucket(r.Context(), in); err != nil {
		// a leftover backend bucket from a previous harness run is fine
		if _, ok := errors.AsType[*types.BucketAlreadyOwnedByYou](err); !ok {
			i.store.remove(bucket) // roll back the claim
			i.finish(w, r, bucket, key.Tenant, requestID, s3err.FromSDKError(err, "/"+bucket))
			return
		}
	}
	w.Header().Set("Location", "/"+bucket)
	w.WriteHeader(http.StatusOK)
	i.finish(w, r, bucket, key.Tenant, requestID, nil)
}

func (i *bucketInterceptor) deleteBucket(w http.ResponseWriter, r *http.Request, bucket string, key *store.Key, requestID string) {
	owner, exists := i.store.owner(bucket)
	if !exists {
		i.finish(w, r, bucket, key.Tenant, requestID,
			s3err.New(http.StatusNotFound, "NoSuchBucket", "The specified bucket does not exist."))
		return
	}
	if owner != key.Tenant {
		i.finish(w, r, bucket, key.Tenant, requestID, s3err.AccessDenied())
		return
	}
	if _, err := i.backend.DeleteBucket(r.Context(), &s3.DeleteBucketInput{Bucket: aws.String(bucket)}); err != nil {
		// keep the registration: the bucket still exists on the backend
		// (e.g. BucketNotEmpty)
		i.finish(w, r, bucket, key.Tenant, requestID, s3err.FromSDKError(err, "/"+bucket))
		return
	}
	i.store.remove(bucket)
	w.WriteHeader(http.StatusNoContent)
	i.finish(w, r, bucket, key.Tenant, requestID, nil)
}

// finish writes the error response (if any) and logs the operation in the
// same JSON-lines stream the gateway observer writes to.
// newRequestID mints an x-amz-request-id in the gateway's shape for the
// requests the harness answers itself.
func newRequestID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (i *bucketInterceptor) finish(w http.ResponseWriter, r *http.Request, bucket, tenant, requestID string, s3e *s3err.Error) {
	status := http.StatusOK
	code := ""
	errMsg := ""
	if s3e != nil {
		s3err.Write(w, r, s3e, requestID)
		status = s3e.Status()
		code = s3e.Code
		errMsg = s3e.Error()
	} else if r.Method == http.MethodDelete {
		status = http.StatusNoContent
	}
	if i.logger != nil {
		i.logger.log(map[string]any{
			"time":       time.Now().Format(time.RFC3339Nano),
			"method":     r.Method,
			"path":       r.URL.Path,
			"request_id": requestID,
			"status":     status,
			"code":       code,
			"tenant":     tenant,
			"bucket":     bucket,
			"error":      errMsg,
			"harness":    true,
		})
	}
}
