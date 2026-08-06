package s3rp

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/fujiwara/s3rp/cors"

	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/store"
	"github.com/goccy/go-yaml"
)

const (
	DefaultListen = ":8080"
	// DefaultRegion lives with the backend definition it applies to, so both
	// Store implementations resolve it identically.
	DefaultRegion = store.DefaultRegion
)

var (
	bucketNameRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	userNameRegexp   = regexp.MustCompile(`^[a-z][a-z0-9_-]+$`)
)

// Password and BackendConfig are defined in the store package; the aliases
// keep the config schema in one place with the rest of the config types.
type (
	Password      = store.Password
	BackendConfig = store.Backend
)

type Config struct {
	Listen  string          `yaml:"listen" json:"listen"`
	Tenants []*TenantConfig `yaml:"tenants,omitempty" json:"tenants,omitempty"`
}

// TenantConfig defines a tenant: its users and the buckets it owns.
type TenantConfig struct {
	Name    string          `yaml:"name" json:"name"`
	Users   []*UserConfig   `yaml:"users" json:"users"`
	Buckets []*BucketConfig `yaml:"buckets" json:"buckets"`
}

// UserConfig defines a user of a tenant. The user name is the stable
// identity (e.g. for policy principals); access keys rotate under it.
type UserConfig struct {
	Name   string                   `yaml:"name" json:"name"`
	Keys   []*KeyConfig             `yaml:"keys" json:"keys"`
	Policy []policy.ActionStatement `yaml:"policy,omitempty" json:"policy,omitempty"`
}

type BucketConfig struct {
	Name    string         `yaml:"name" json:"name"`
	Backend *BackendConfig `yaml:"backend" json:"backend"`
	Policy  string         `yaml:"policy,omitempty" json:"policy,omitempty"` // bucket policy JSON text
	CORS    []*cors.Rule   `yaml:"cors,omitempty" json:"cors,omitempty"`
	// CreatedAt is reported by ListBuckets (unset = the Unix epoch).
	CreatedAt time.Time `yaml:"created_at,omitzero" json:"created_at,omitzero"`
}

type KeyConfig struct {
	AccessKeyID     string   `yaml:"access_key_id" json:"access_key_id"`
	SecretAccessKey Password `yaml:"secret_access_key" json:"secret_access_key"`
}

// LoadConfig reads a YAML config file, expanding environment variables in the content.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal([]byte(os.ExpandEnv(string(data))), &c); err != nil {
		return nil, fmt.Errorf("failed to parse config file %s: %w", path, err)
	}
	c.SetDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) SetDefaults() {
	if c.Listen == "" {
		c.Listen = DefaultListen
	}
	for _, t := range c.Tenants {
		for _, b := range t.Buckets {
			if b.Backend == nil {
				continue
			}
			b.Backend.SetDefaults(b.Name)
		}
	}
}

func (c *Config) Validate() error {
	if len(c.Tenants) == 0 {
		return fmt.Errorf("no tenants defined")
	}
	tenantNames := make(map[string]bool, len(c.Tenants))
	// bucket names and access key ids must be unique across all tenants:
	// path-style URLs carry no tenant discriminator, and a key belongs to
	// exactly one tenant
	bucketNames := make(map[string]bool)
	keyIDs := make(map[string]bool)
	// tracks which tenant owns each physical backend target (endpoint + backend
	// bucket): two tenants mapping to the same physical bucket would share data
	backendOwner := make(map[string]string)
	for _, t := range c.Tenants {
		if t.Name == "" {
			return fmt.Errorf("tenant name is required")
		}
		if tenantNames[t.Name] {
			return fmt.Errorf("duplicate tenant name %q", t.Name)
		}
		tenantNames[t.Name] = true

		if len(t.Users) == 0 {
			return fmt.Errorf("tenant %s: at least one user is required", t.Name)
		}
		userNames := make(map[string]bool, len(t.Users))
		for _, u := range t.Users {
			if !userNameRegexp.MatchString(u.Name) {
				return fmt.Errorf("tenant %s: invalid user name %q", t.Name, u.Name)
			}
			if userNames[u.Name] {
				return fmt.Errorf("tenant %s: duplicate user name %q", t.Name, u.Name)
			}
			userNames[u.Name] = true
			if len(u.Keys) == 0 {
				return fmt.Errorf("tenant %s: user %s: at least one key is required", t.Name, u.Name)
			}
			for _, k := range u.Keys {
				if k.AccessKeyID == "" || k.SecretAccessKey == "" {
					return fmt.Errorf("tenant %s: user %s: key access_key_id and secret_access_key are required", t.Name, u.Name)
				}
				if keyIDs[k.AccessKeyID] {
					return fmt.Errorf("duplicate access_key_id %q", k.AccessKeyID)
				}
				keyIDs[k.AccessKeyID] = true
			}
			if len(u.Policy) > 0 {
				up := &policy.UserPolicy{Statements: u.Policy}
				if err := policy.ValidateUserPolicy(up); err != nil {
					return fmt.Errorf("tenant %s: user %s: invalid policy: %w", t.Name, u.Name, err)
				}
				// the byte cap applies to the serialized form, which the YAML
				// path does not otherwise produce
				if _, err := policy.MarshalUserPolicy(up); err != nil {
					return fmt.Errorf("tenant %s: user %s: invalid policy: %w", t.Name, u.Name, err)
				}
			}
		}

		if len(t.Buckets) == 0 {
			return fmt.Errorf("tenant %s: at least one bucket is required", t.Name)
		}
		for _, b := range t.Buckets {
			if b.Name == "" {
				return fmt.Errorf("tenant %s: bucket name is required", t.Name)
			}
			if !bucketNameRegexp.MatchString(b.Name) {
				return fmt.Errorf("invalid bucket name %q", b.Name)
			}
			if bucketNames[b.Name] {
				return fmt.Errorf("duplicate bucket name %q", b.Name)
			}
			bucketNames[b.Name] = true

			if b.Backend == nil {
				return fmt.Errorf("bucket %s: backend is required", b.Name)
			}
			// an empty endpoint means AWS S3 (resolved by the SDK from the region)
			if b.Backend.Endpoint != "" {
				if u, err := url.Parse(b.Backend.Endpoint); err != nil {
					return fmt.Errorf("bucket %s: invalid backend endpoint: %w", b.Name, err)
				} else if u.Scheme != "http" && u.Scheme != "https" {
					return fmt.Errorf("bucket %s: backend endpoint must be an http(s) URL: %s", b.Name, b.Backend.Endpoint)
				}
			}
			// empty credentials mean the SDK default credential chain
			if (b.Backend.AccessKeyID == "") != (b.Backend.SecretAccessKey == "") {
				return fmt.Errorf("bucket %s: backend access_key_id and secret_access_key must be set together", b.Name)
			}
			// two tenants must not target the same physical backend bucket, or
			// each could read/overwrite/delete the other's objects. The backend
			// bucket defaults to the front name (see SetDefaults).
			backendBucket := b.Backend.Bucket
			if backendBucket == "" {
				backendBucket = b.Name
			}
			target := b.Backend.Endpoint + "\x00" + backendBucket
			if owner, ok := backendOwner[target]; ok && owner != t.Name {
				return fmt.Errorf("bucket %s: backend bucket %q on %q is already used by tenant %s (cross-tenant sharing is not allowed)",
					b.Name, backendBucket, b.Backend.Endpoint, owner)
			}
			backendOwner[target] = t.Name

			if b.Policy != "" {
				p, err := policy.Parse(b.Policy)
				if err != nil {
					return fmt.Errorf("bucket %s: invalid policy: %w", b.Name, err)
				}
				for _, st := range p.Statement {
					for _, res := range st.Resource {
						if res != b.Name && !strings.HasPrefix(res, b.Name+"/") {
							return fmt.Errorf("bucket %s: policy resource %q does not refer to this bucket", b.Name, res)
						}
					}
					// own-tenant users must be written unqualified: a plain
					// name is what evaluation matches for them, so a
					// self-qualified principal would silently never match
					for _, pr := range []*policy.Principal{st.Principal, st.NotPrincipal} {
						if pr == nil {
							continue
						}
						for _, u := range pr.Users {
							if after, ok := strings.CutPrefix(u, t.Name+"/"); ok {
								return fmt.Errorf("bucket %s: principal %q names its own tenant; write the plain user name %q",
									b.Name, u, after)
							}
						}
					}
				}
			}

			for _, rule := range b.CORS {
				if len(rule.AllowedOrigins) == 0 {
					return fmt.Errorf("bucket %s: cors rule requires at least one allowed origin", b.Name)
				}
				if len(rule.AllowedMethods) == 0 {
					return fmt.Errorf("bucket %s: cors rule requires at least one allowed method", b.Name)
				}
				for _, m := range rule.AllowedMethods {
					switch m {
					case "GET", "PUT", "POST", "DELETE", "HEAD":
					default:
						return fmt.Errorf("bucket %s: unsupported cors method %q", b.Name, m)
					}
				}
				if rule.MaxAgeSeconds < 0 {
					return fmt.Errorf("bucket %s: cors max_age_seconds must not be negative", b.Name)
				}
			}
		}
	}
	return nil
}
