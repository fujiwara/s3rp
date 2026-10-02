package s3rp

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/fujiwara/s3rp/cors"
	"github.com/fujiwara/s3rp/s3op"

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

// Password and BackendConfig are defined in the store package; the aliases
// keep the config schema in one place with the rest of the config types.
type (
	Password      = store.Password
	BackendConfig = store.Backend
)

type Config struct {
	Listen string `yaml:"listen" json:"listen"`
	// VirtualHostSuffix enables virtual-hosted-style addressing under this
	// host name ("s3.example.com": bucket "photos" is photos.s3.example.com)
	// alongside the path style; empty = path style only.
	VirtualHostSuffix string `yaml:"virtual_host_suffix,omitempty" json:"virtual_host_suffix,omitempty"`
	// CircuitBreaker, when set, guards every backend with a breaker that
	// opens after Failures consecutive failed attempts and probes once per
	// Cooldown after that; requests to an open backend are refused with
	// ServiceUnavailable instead of waiting on it.
	CircuitBreaker *CircuitBreakerConfig `yaml:"circuit_breaker,omitempty" json:"circuit_breaker,omitempty"`
	// Metrics, when set, exports the metrics of docs/metrics.md over OTLP.
	Metrics *MetricsConfig `yaml:"metrics,omitempty" json:"metrics,omitempty"`
	// StrictActions refuses a config whose policies name an action the
	// gateway never authorizes; unset, LoadConfig only warns about them.
	StrictActions bool            `yaml:"strict_actions,omitempty" json:"strict_actions,omitempty"`
	Tenants       []*TenantConfig `yaml:"tenants,omitempty" json:"tenants,omitempty"`
}

// CircuitBreakerConfig sizes the per-backend breaker (see s3gw.NewConsecutiveFailures).
type CircuitBreakerConfig struct {
	Failures int           `yaml:"failures" json:"failures"`
	Cooldown time.Duration `yaml:"cooldown" json:"cooldown"`
}

func (c *CircuitBreakerConfig) validate() error {
	if c.Failures < 1 {
		return fmt.Errorf("circuit_breaker.failures must be at least 1")
	}
	if c.Cooldown <= 0 {
		return fmt.Errorf("circuit_breaker.cooldown must be positive")
	}
	return nil
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
		if c.StrictActions || !errors.Is(err, s3op.ErrUnknownAction) {
			return nil, fmt.Errorf("invalid config %s: %w", path, err)
		}
		// Validate reports unknown actions only once everything else
		// passed, so the config is otherwise sound
		errs := []error{err}
		if j, ok := err.(interface{ Unwrap() []error }); ok {
			errs = j.Unwrap()
		}
		for _, e := range errs {
			slog.Warn("policy statement never takes effect; set strict_actions to refuse it", "config", path, "error", e)
		}
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

// Validate checks the config. Policies naming an action the gateway never
// authorizes (a typo, or an AWS action such as s3:PutObjectVersionAcl) are
// reported last, only once everything else is valid, joined as errors
// wrapping s3op.ErrUnknownAction: a caller seeing
// errors.Is(err, s3op.ErrUnknownAction) knows nothing else is wrong and may
// downgrade them to warnings.
func (c *Config) Validate() error {
	if len(c.Tenants) == 0 {
		return fmt.Errorf("no tenants defined")
	}
	if c.CircuitBreaker != nil {
		if err := c.CircuitBreaker.validate(); err != nil {
			return err
		}
	}
	tenantNames := make(map[string]bool, len(c.Tenants))
	seen := &configNames{
		bucketNames:  make(map[string]bool),
		keyIDs:       make(map[string]bool),
		backendOwner: make(map[string]string),
	}
	for _, t := range c.Tenants {
		if err := store.ValidateTenantName(t.Name); err != nil {
			return err
		}
		if tenantNames[t.Name] {
			return fmt.Errorf("duplicate tenant name %q", t.Name)
		}
		tenantNames[t.Name] = true
		if err := seen.validateTenant(t); err != nil {
			return err
		}
	}
	return errors.Join(seen.unknownActions...)
}

// configNames tracks what must be unique across all tenants while a config
// is validated.
type configNames struct {
	// bucket names and access key ids must be unique across all tenants:
	// path-style URLs carry no tenant discriminator, and a key belongs to
	// exactly one tenant
	bucketNames map[string]bool
	keyIDs      map[string]bool
	// tracks which tenant owns each physical backend target (endpoint + backend
	// bucket): two tenants mapping to the same physical bucket would share data
	backendOwner map[string]string
	// statements naming an action the gateway never authorizes, reported
	// only once everything else is valid
	unknownActions []error
}

// splitUnknownActions separates the action checker's errors (all of
// them, when errors.Is finds ErrUnknownAction: policy validation runs the
// checker only once the policy is otherwise valid) from any other policy
// error, recording each under prefix.
func (n *configNames) splitUnknownActions(err error, prefix string) error {
	if err == nil || !errors.Is(err, s3op.ErrUnknownAction) {
		return err
	}
	errs := []error{err}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		errs = j.Unwrap()
	}
	for _, e := range errs {
		n.unknownActions = append(n.unknownActions, fmt.Errorf("%s%w", prefix, e))
	}
	return nil
}

func (n *configNames) validateTenant(t *TenantConfig) error {
	if len(t.Users) == 0 {
		return fmt.Errorf("tenant %s: at least one user is required", t.Name)
	}
	userNames := make(map[string]bool, len(t.Users))
	for _, u := range t.Users {
		if err := store.ValidateUserName(u.Name); err != nil {
			return fmt.Errorf("tenant %s: %w", t.Name, err)
		}
		if userNames[u.Name] {
			return fmt.Errorf("tenant %s: duplicate user name %q", t.Name, u.Name)
		}
		userNames[u.Name] = true
		if err := n.validateUser(t.Name, u); err != nil {
			return fmt.Errorf("tenant %s: user %s: %w", t.Name, u.Name, err)
		}
	}
	if len(t.Buckets) == 0 {
		return fmt.Errorf("tenant %s: at least one bucket is required", t.Name)
	}
	for _, b := range t.Buckets {
		if err := n.validateBucket(t.Name, b); err != nil {
			return err
		}
	}
	return nil
}

func (n *configNames) validateUser(tenant string, u *UserConfig) error {
	if len(u.Keys) == 0 {
		return fmt.Errorf("at least one key is required")
	}
	for _, k := range u.Keys {
		if k.AccessKeyID == "" || k.SecretAccessKey == "" {
			return fmt.Errorf("key access_key_id and secret_access_key are required")
		}
		if n.keyIDs[k.AccessKeyID] {
			return fmt.Errorf("duplicate access_key_id %q", k.AccessKeyID)
		}
		n.keyIDs[k.AccessKeyID] = true
	}
	if len(u.Policy) == 0 {
		return nil
	}
	up := &policy.UserPolicy{Statements: u.Policy}
	err := policy.ValidateUserPolicy(up, s3op.CheckActionPattern)
	if err := n.splitUnknownActions(err, fmt.Sprintf("tenant %s: user %s: policy ", tenant, u.Name)); err != nil {
		return fmt.Errorf("invalid policy: %w", err)
	}
	// the byte cap applies to the serialized form, which the YAML
	// path does not otherwise produce
	if _, err := policy.MarshalUserPolicy(up); err != nil {
		return fmt.Errorf("invalid policy: %w", err)
	}
	return nil
}

func (n *configNames) validateBucket(tenant string, b *BucketConfig) error {
	if err := store.ValidateBucketName(b.Name); err != nil {
		return err
	}
	if n.bucketNames[b.Name] {
		return fmt.Errorf("duplicate bucket name %q", b.Name)
	}
	n.bucketNames[b.Name] = true

	if b.Backend == nil {
		return fmt.Errorf("bucket %s: backend is required", b.Name)
	}
	if err := b.Backend.Validate(); err != nil {
		return fmt.Errorf("bucket %s: %w", b.Name, err)
	}
	// two tenants must not target the same physical backend bucket, or
	// each could read/overwrite/delete the other's objects. The backend
	// bucket defaults to the front name (see SetDefaults).
	backendBucket := b.Backend.Bucket
	if backendBucket == "" {
		backendBucket = b.Name
	}
	target := b.Backend.Endpoint + "\x00" + backendBucket
	if owner, ok := n.backendOwner[target]; ok && owner != tenant {
		return fmt.Errorf("bucket %s: backend bucket %q on %q is already used by tenant %s (cross-tenant sharing is not allowed)",
			b.Name, backendBucket, b.Backend.Endpoint, owner)
	}
	n.backendOwner[target] = tenant

	if b.Policy != "" {
		_, err := policy.Parse(b.Name, b.Policy, s3op.CheckActionPattern)
		if err := n.splitUnknownActions(err, fmt.Sprintf("tenant %s: bucket %s: policy ", tenant, b.Name)); err != nil {
			return fmt.Errorf("bucket %s: invalid policy: %w", b.Name, err)
		}
	}
	for _, rule := range b.CORS {
		if err := rule.Validate(); err != nil {
			return fmt.Errorf("bucket %s: %w", b.Name, err)
		}
	}
	return nil
}
