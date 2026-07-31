package s3rp

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/store"
	"github.com/goccy/go-yaml"
)

const (
	DefaultListen = ":8080"
	DefaultRegion = "us-east-1"
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
	Tenants []*TenantConfig `yaml:"tenants" json:"tenants"`
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
	Name string       `yaml:"name" json:"name"`
	Keys []*KeyConfig `yaml:"keys" json:"keys"`
}

type BucketConfig struct {
	Name    string         `yaml:"name" json:"name"`
	Backend *BackendConfig `yaml:"backend" json:"backend"`
	Policy  string         `yaml:"policy,omitempty" json:"policy,omitempty"` // bucket policy JSON text
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
			if b.Backend.Region == "" {
				b.Backend.Region = DefaultRegion
			}
			if b.Backend.Bucket == "" {
				b.Backend.Bucket = b.Name
			}
			if b.Backend.UsePathStyle == nil {
				// S3-compatible servers conventionally need path-style,
				// while AWS S3 (no endpoint) prefers virtual-hosted style
				usePathStyle := b.Backend.Endpoint != ""
				b.Backend.UsePathStyle = &usePathStyle
			}
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
				}
			}
		}
	}
	return nil
}
