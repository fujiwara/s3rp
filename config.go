package s3rp

import (
	"fmt"
	"net/url"
	"os"
	"regexp"

	"github.com/goccy/go-yaml"
)

const (
	DefaultListen = ":8080"
	DefaultRegion = "us-east-1"
)

var bucketNameRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

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

type Config struct {
	Listen  string          `yaml:"listen" json:"listen"`
	Buckets []*BucketConfig `yaml:"buckets" json:"buckets"`
}

type BucketConfig struct {
	Name    string         `yaml:"name" json:"name"`
	Backend *BackendConfig `yaml:"backend" json:"backend"`
	Keys    []*KeyConfig   `yaml:"keys" json:"keys"`
}

type BackendConfig struct {
	Endpoint        string   `yaml:"endpoint" json:"endpoint"`
	Region          string   `yaml:"region" json:"region"`
	Bucket          string   `yaml:"bucket,omitempty" json:"bucket,omitempty"`
	AccessKeyID     string   `yaml:"access_key_id" json:"access_key_id"`
	SecretAccessKey Password `yaml:"secret_access_key" json:"secret_access_key"`
	UsePathStyle    *bool    `yaml:"use_path_style,omitempty" json:"use_path_style,omitempty"`
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
	for _, b := range c.Buckets {
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

func (c *Config) Validate() error {
	if len(c.Buckets) == 0 {
		return fmt.Errorf("no buckets defined")
	}
	names := make(map[string]bool, len(c.Buckets))
	keySecrets := make(map[string]Password)
	for _, b := range c.Buckets {
		if b.Name == "" {
			return fmt.Errorf("bucket name is required")
		}
		if !bucketNameRegexp.MatchString(b.Name) {
			return fmt.Errorf("invalid bucket name %q", b.Name)
		}
		if names[b.Name] {
			return fmt.Errorf("duplicate bucket name %q", b.Name)
		}
		names[b.Name] = true

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

		if len(b.Keys) == 0 {
			return fmt.Errorf("bucket %s: at least one key is required", b.Name)
		}
		inBucket := make(map[string]bool, len(b.Keys))
		for _, k := range b.Keys {
			if k.AccessKeyID == "" || k.SecretAccessKey == "" {
				return fmt.Errorf("bucket %s: key access_key_id and secret_access_key are required", b.Name)
			}
			if inBucket[k.AccessKeyID] {
				return fmt.Errorf("bucket %s: duplicate key access_key_id %q", b.Name, k.AccessKeyID)
			}
			inBucket[k.AccessKeyID] = true
			// verification looks up the secret by access key id alone,
			// so the same id must not have different secrets across buckets
			if secret, ok := keySecrets[k.AccessKeyID]; ok {
				if secret != k.SecretAccessKey {
					return fmt.Errorf("access_key_id %q has different secrets across buckets", k.AccessKeyID)
				}
			} else {
				keySecrets[k.AccessKeyID] = k.SecretAccessKey
			}
		}
	}
	return nil
}
