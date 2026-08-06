package s3gw_test

import (
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp/cors"
	"github.com/fujiwara/s3rp/policy"
	"github.com/fujiwara/s3rp/s3gw"
	"github.com/fujiwara/s3rp/store"
)

// Compact specs for the tests that need more than the single tenant, user
// and bucket newTestGateway provides — several users under one tenant, or
// buckets carrying policies or CORS rules. They stand in for what the YAML
// config store builds, without depending on it.

type userSpec struct {
	name, keyID, secret string
	policy              []policy.ActionStatement
}

type bucketSpec struct {
	name       string
	policyText string
	cors       []*cors.Rule
	// endpoint overrides the shared backend, for the copy tests: copying
	// works only between buckets on one backend, so they need a bucket
	// that is deliberately elsewhere.
	endpoint string
}

// buildStore assembles a memStore for one tenant. Bucket policies are parsed
// here exactly as a store implementation must, and every backend gets
// SetDefaults applied, which the gateway relies on.
func buildStore(t *testing.T, tenant string, users []userSpec, buckets []bucketSpec) memStore {
	t.Helper()
	m := memStore{keys: map[string]*store.Key{}, buckets: map[string]*store.Bucket{}}
	m.addTenant(t, tenant, users, buckets)
	return m
}

// addTenant adds another tenant's definitions, for the tests that check one
// tenant cannot reach another's bucket.
func (m memStore) addTenant(t *testing.T, tenant string, users []userSpec, buckets []bucketSpec) memStore {
	t.Helper()
	for _, u := range users {
		k := &store.Key{
			AccessKeyID:     u.keyID,
			SecretAccessKey: store.Password(u.secret),
			Tenant:          tenant,
			User:            u.name,
		}
		if len(u.policy) > 0 {
			k.Policy = &policy.UserPolicy{Statements: u.policy}
		}
		m.keys[u.keyID] = k
	}
	for _, b := range buckets {
		endpoint := b.endpoint
		if endpoint == "" {
			endpoint = "http://backend.invalid"
		}
		backend := &store.Backend{
			Endpoint:        endpoint,
			Bucket:          "backend-" + b.name,
			AccessKeyID:     "bk",
			SecretAccessKey: "bs",
		}
		backend.SetDefaults(b.name)
		bucket := &store.Bucket{
			Tenant: tenant, Name: b.name, Backend: backend,
			PolicyText: b.policyText, CORS: b.cors,
		}
		if b.policyText != "" {
			p, err := policy.Parse(b.name, b.policyText)
			if err != nil {
				t.Fatalf("bucket %s: %v", b.name, err)
			}
			bucket.Policy = p
		}
		m.buckets[b.name] = bucket
	}
	return m
}

// gatewayFor serves the given definitions with stub as every bucket's
// backend.
func gatewayFor(t *testing.T, m memStore, stub s3gw.BackendClient) *s3gw.Gateway {
	t.Helper()
	gw := s3gw.New(m)
	for name := range m.buckets {
		if err := gw.SetBackend(name, stub); err != nil {
			t.Fatal(err)
		}
	}
	return gw
}

// clientsFor serves gw and returns one SDK client per user, keyed by user
// name, so a test can act as each identity in turn.
func clientsFor(t *testing.T, gw *s3gw.Gateway, users []userSpec) map[string]*s3.Client {
	t.Helper()
	ts := newTestServer(t, gw)
	clients := make(map[string]*s3.Client, len(users))
	for _, u := range users {
		cfg, err := awsconfig.LoadDefaultConfig(t.Context(),
			awsconfig.WithRegion("us-east-1"),
			awsconfig.WithCredentialsProvider(
				credentials.NewStaticCredentialsProvider(u.keyID, u.secret, ""),
			),
		)
		if err != nil {
			t.Fatal(err)
		}
		clients[u.name] = s3.NewFromConfig(cfg, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(ts)
			o.UsePathStyle = true
			o.RequestChecksumCalculation = aws.RequestChecksumCalculationWhenRequired
		})
	}
	return clients
}
