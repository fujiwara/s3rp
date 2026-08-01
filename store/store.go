// Package store defines the read-only contract for tenant, key and bucket
// definitions used by s3rp. The default implementation reads the static
// YAML config (see the parent package); a future implementation may read
// from an RDBMS.
package store

import (
	"context"
	"errors"

	"github.com/fujiwara/s3rp/cors"
	"github.com/fujiwara/s3rp/policy"
)

// ErrNotFound is returned by Store implementations when the requested
// entity does not exist.
var ErrNotFound = errors.New("not found")

// Store provides read-only access to tenant, key and bucket definitions.
// Methods must be safe for concurrent use.
type Store interface {
	// GetKey returns the access key by its id.
	// Returns an error wrapping ErrNotFound when the key does not exist.
	GetKey(ctx context.Context, accessKeyID string) (*Key, error)
	// GetBucket returns the named bucket of the tenant.
	// Returns an error wrapping ErrNotFound when the tenant does not own
	// such a bucket.
	GetBucket(ctx context.Context, tenant, bucket string) (*Bucket, error)
	// GetBucketByName returns the bucket by its (globally unique) name,
	// regardless of the tenant. Used for unauthenticated CORS preflight
	// requests, which carry no tenant identity.
	GetBucketByName(ctx context.Context, bucket string) (*Bucket, error)
	// ListBucketNames returns the bucket names owned by the tenant.
	ListBucketNames(ctx context.Context, tenant string) ([]string, error)
}

// Key is a front-side access key. It belongs to exactly one user of a
// tenant. The user name is the stable identity; keys rotate under it.
type Key struct {
	AccessKeyID     string
	SecretAccessKey Password
	Tenant          string
	User            string
	// Policy is the user's identity policy (nil = allow all operations).
	Policy *policy.UserPolicy
}

// Bucket is a front-side bucket and its backend definition.
type Bucket struct {
	Tenant  string
	Name    string
	Backend *Backend

	// PolicyText is the raw bucket policy JSON (empty if none); Policy is
	// its parsed form. Implementations store the text and parse it with
	// policy.Parse.
	PolicyText string
	Policy     *policy.Policy

	// CORS is the CORS configuration of the bucket (nil if none).
	CORS []*cors.Rule
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
