// Package store defines the read-only contract for tenant, key and bucket
// definitions used by s3rp. The bundled implementation reads the static
// YAML config (see the parent package); a service built on the gateway
// implements this interface against its own definition storage.
package store

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/fujiwara/s3rp/cors"
	"github.com/fujiwara/s3rp/policy"
)

// ErrNotFound is returned by Store implementations when the requested
// entity does not exist.
var ErrNotFound = errors.New("not found")

// ErrInvalidToken is returned by GetKey when the presented session token
// fails the store's own validation — a self-validating token whose
// authentication failed, or one the store knows to be revoked. The client
// sees InvalidToken instead of the InvalidAccessKeyId an ErrNotFound
// produces.
var ErrInvalidToken = errors.New("invalid session token")

// Store provides read-only access to tenant, key and bucket definitions.
// Methods must be safe for concurrent use.
type Store interface {
	// GetKey returns the access key by its id. sessionToken is the session
	// token the request presented (empty when none), handed over before the
	// signature is verified — untrusted input. A store that persists
	// temporary keys may ignore it: the gateway still requires the
	// presented token to match Key.SessionToken exactly, so revocation
	// stays a row delete. A store that issues self-contained tokens instead
	// derives the Key from the token — after authenticating it, e.g. by its
	// MAC — and returns the presented token as Key.SessionToken, which
	// passes the gateway's exact-match trivially.
	// Returns an error wrapping ErrNotFound when the key does not exist,
	// and one wrapping ErrInvalidToken when the token itself fails the
	// store's validation.
	GetKey(ctx context.Context, accessKeyID, sessionToken string) (*Key, error)
	// GetBucket returns the named bucket of the tenant.
	// Returns an error wrapping ErrNotFound when the tenant does not own
	// such a bucket.
	GetBucket(ctx context.Context, tenant, bucket string) (*Bucket, error)
	// GetBucketByName returns the bucket by its (globally unique) name,
	// regardless of the tenant. Used for unauthenticated CORS preflight
	// requests, which carry no tenant identity, and to resolve cross-tenant
	// requests — a bucket whose policy names another tenant's user.
	GetBucketByName(ctx context.Context, bucket string) (*Bucket, error)
	// ListBuckets returns the tenant's buckets as light entries — just what
	// the ListBuckets response exposes — so an implementation does not have
	// to materialize full definitions (backend credentials included) for a
	// listing.
	ListBuckets(ctx context.Context, tenant string) ([]BucketEntry, error)
}

// BucketEntry is one bucket of a ListBuckets result.
type BucketEntry struct {
	Name string
	// CreatedAt is when the bucket was created. Zero = unknown; the
	// gateway reports the Unix epoch then.
	CreatedAt time.Time
}

// Key is a front-side access key. It belongs to exactly one user of a
// tenant. The user name is the stable identity; keys rotate under it.
type Key struct {
	AccessKeyID     string
	SecretAccessKey Password
	Tenant          string
	User            string
	// SessionToken marks a temporary credential (empty = long-lived key):
	// a request signed with this key must present exactly this token.
	// Expiry is the Store's business — an expired key is simply not
	// returned by GetKey — and issuance is the control plane's; for a
	// persisted temporary key the issuance response must not be returned
	// before the key is visible to the store the gateways read, or the
	// first use races the write. A store validating self-contained tokens
	// sets the presented token it authenticated (see Store.GetKey).
	SessionToken string
	// Policy is the user's identity policy (nil = allow all operations).
	Policy *policy.UserPolicy

	// Metadata is an opaque value a Store implementation may attach when it
	// builds the key — data it already had in hand, such as a suspension
	// flag or a rate-limit tier loaded with the same query. The gateway
	// never interprets it; it is handed back on Op for the Authorizer and
	// Interceptors installed by the same service. A Store that shares keys
	// across requests must make the value safe for concurrent reads —
	// immutable, or synchronized internally.
	Metadata any
}

// Bucket is a front-side bucket and its backend definition.
type Bucket struct {
	Tenant  string
	Name    string
	Backend *Backend

	// CreatedAt is when the bucket was created (zero = unknown). ListBuckets
	// reports it via BucketEntry; here it rides along for operations that may
	// want it.
	CreatedAt time.Time

	// PolicyText is the bucket policy JSON as the tenant wrote it (empty
	// if none); it is what GetBucketPolicy returns, verbatim. Policy is
	// the parsed form the gateway evaluates. The two are deliberately
	// independent — nothing requires Policy to be the parse of
	// PolicyText — so a store whose tenants write a different policy
	// dialect can keep the original text here and derive Policy from a
	// translation.
	PolicyText string
	Policy     *policy.Policy

	// CORS is the CORS configuration of the bucket (nil if none).
	CORS []*cors.Rule

	// Metadata is an opaque value a Store implementation may attach when it
	// builds the bucket — data it already had in hand, such as quota or
	// plan information loaded with the same query. The gateway never
	// interprets it; it is handed back on Op for the Authorizer and
	// Interceptors installed by the same service. A Store that shares
	// buckets across requests must make the value safe for concurrent
	// reads — immutable, or synchronized internally.
	Metadata any
}

// DefaultRegion is used when a backend definition omits the region.
const DefaultRegion = "us-east-1"

// Backend is the definition of an S3-compatible backend for a bucket.
// An empty Endpoint means Amazon S3 (resolved by the SDK from the region);
// empty credentials mean the SDK default credential chain.
type Backend struct {
	Endpoint        string   `yaml:"endpoint" json:"endpoint"`
	Region          string   `yaml:"region" json:"region"`
	Bucket          string   `yaml:"bucket,omitempty" json:"bucket,omitempty"`
	AccessKeyID     string   `yaml:"access_key_id" json:"access_key_id"`
	SecretAccessKey Password `yaml:"secret_access_key" json:"secret_access_key"`
	UsePathStyle    *bool    `yaml:"use_path_style,omitempty" json:"use_path_style,omitempty"`
}

// SetDefaults fills in the optional fields of a backend definition, given the
// front-side bucket name it belongs to. Every Store implementation must apply
// it so callers always receive a fully resolved backend: the config store
// defaults at load time, and the DB store defaults after unmarshaling a row,
// whose optional columns may legitimately be absent.
func (b *Backend) SetDefaults(bucketName string) {
	if b.Region == "" {
		b.Region = DefaultRegion
	}
	if b.Bucket == "" {
		b.Bucket = bucketName
	}
	if b.UsePathStyle == nil {
		// S3-compatible servers conventionally need path-style, while
		// Amazon S3 (no endpoint) prefers virtual-hosted style
		usePathStyle := b.Endpoint != ""
		b.UsePathStyle = &usePathStyle
	}
}

// IsHTTPS reports whether the backend is reached over TLS. Without an
// endpoint the SDK resolves the Amazon S3 endpoint, which is https.
func (b *Backend) IsHTTPS() bool {
	if b.Endpoint == "" {
		return true
	}
	return strings.HasPrefix(strings.ToLower(b.Endpoint), "https://")
}

// Password is a string that is masked when marshaled to JSON or YAML.
type Password string

func (p Password) String() string {
	return string(p)
}

func (p Password) MarshalJSON() ([]byte, error) {
	if p == "" {
		return []byte(`""`), nil
	}
	return []byte(`"********"`), nil
}

func (p Password) MarshalYAML() ([]byte, error) {
	if p == "" {
		return []byte(`""`), nil
	}
	return []byte(`"********"`), nil
}
