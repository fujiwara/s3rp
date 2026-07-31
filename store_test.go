package s3rp_test

import (
	"database/sql"
	"errors"
	"sort"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/fujiwara/s3rp"
	"github.com/fujiwara/s3rp/db"
	"github.com/fujiwara/s3rp/store"
	"github.com/fujiwara/s3rp/store/rdb"
	_ "modernc.org/sqlite"
)

// storeContract asserts the store.Store semantics against the definitions
// of testdata/config.yaml. It runs for every Store implementation.
func storeContract(t *testing.T, st store.Store) {
	ctx := t.Context()

	t.Run("GetKey", func(t *testing.T) {
		key, err := st.GetKey(ctx, "S3RPKEY001")
		if err != nil {
			t.Fatal(err)
		}
		if key.Tenant != "acme" || key.User != "app1" || key.SecretAccessKey.String() != "frontsecret001" {
			t.Errorf("unexpected key %+v", key)
		}
		// a key of another user of the same tenant
		key2, err := st.GetKey(ctx, "S3RPKEY003")
		if err != nil {
			t.Fatal(err)
		}
		if key2.Tenant != "acme" || key2.User != "batch" {
			t.Errorf("unexpected key %+v", key2)
		}
		if _, err := st.GetKey(ctx, "NOSUCHKEY"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expect ErrNotFound, got %v", err)
		}
	})
	t.Run("GetBucket", func(t *testing.T) {
		b, err := st.GetBucket(ctx, "acme", "photos")
		if err != nil {
			t.Fatal(err)
		}
		if b.Backend.Bucket != "photos-prod" {
			t.Errorf("unexpected bucket %+v", b)
		}
		if b.Backend.UsePathStyle == nil || !*b.Backend.UsePathStyle {
			t.Errorf("expect use_path_style true, got %+v", b.Backend)
		}
		// another tenant's bucket is not visible
		if _, err := st.GetBucket(ctx, "acme", "research"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expect ErrNotFound, got %v", err)
		}
		if _, err := st.GetBucket(ctx, "nosuchtenant", "photos"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expect ErrNotFound, got %v", err)
		}
	})
	t.Run("GetBucketByName", func(t *testing.T) {
		b, err := st.GetBucketByName(ctx, "research")
		if err != nil {
			t.Fatal(err)
		}
		if b.Tenant != "umbrella" {
			t.Errorf("unexpected tenant %s", b.Tenant)
		}
		if _, err := st.GetBucketByName(ctx, "nosuchbucket"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expect ErrNotFound, got %v", err)
		}
	})
	t.Run("ListBucketNames", func(t *testing.T) {
		names, err := st.ListBucketNames(ctx, "acme")
		if err != nil {
			t.Fatal(err)
		}
		sort.Strings(names)
		if len(names) != 2 || names[0] != "logs" || names[1] != "photos" {
			t.Errorf("unexpected names %v", names)
		}
		if _, err := st.ListBucketNames(ctx, "nosuchtenant"); !errors.Is(err, store.ErrNotFound) {
			t.Errorf("expect ErrNotFound, got %v", err)
		}
	})
}

func loadTestConfig(t *testing.T) *s3rp.Config {
	t.Helper()
	t.Setenv("TEST_BACKEND_ACCESS_KEY_ID", "backendkey")
	t.Setenv("TEST_BACKEND_SECRET_ACCESS_KEY", "backendsecret")
	cfg, err := s3rp.LoadConfig("testdata/config.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestConfigStore(t *testing.T) {
	storeContract(t, s3rp.NewConfigStore(loadTestConfig(t)))
}

// buildTestDB migrates a fresh sqlite database and imports the config.
func buildTestDB(t *testing.T, cfg *s3rp.Config) (dsn string) {
	t.Helper()
	dsn = t.TempDir() + "/s3rp.db"
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	if err := db.Migrate(t.Context(), sqldb); err != nil {
		t.Fatal(err)
	}
	if err := db.Import(t.Context(), sqldb, cfg); err != nil {
		t.Fatal(err)
	}
	return dsn
}

func TestRDBStore(t *testing.T) {
	dsn := buildTestDB(t, loadTestConfig(t))
	st, err := rdb.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	storeContract(t, st)
}

func TestRDBStoreReadOnly(t *testing.T) {
	if _, err := rdb.Open("s3rp.db?mode=rwc"); err == nil {
		t.Error("expect error for a dsn specifying mode=")
	}
	// migration is idempotent
	dsn := buildTestDB(t, loadTestConfig(t))
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer sqldb.Close()
	if err := db.Migrate(t.Context(), sqldb); err != nil {
		t.Errorf("migrate must be idempotent: %v", err)
	}
}

// TestRDBStorePolicyAndCORS verifies that policy and CORS definitions
// survive the DB roundtrip.
func TestRDBStorePolicyAndCORS(t *testing.T) {
	cfg := loadTestConfig(t)
	cfg.Tenants[0].Buckets[0].Policy = batchReadOnlyPolicyFor("photos")
	cfg.Tenants[0].Buckets[0].CORS = []*store.CORSRule{
		{
			AllowedOrigins: []string{"https://app.example.com"},
			AllowedMethods: []string{"GET"},
			MaxAgeSeconds:  60,
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	dsn := buildTestDB(t, cfg)
	st, err := rdb.Open(dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	b, err := st.GetBucketByName(t.Context(), "photos")
	if err != nil {
		t.Fatal(err)
	}
	if b.Policy == nil || len(b.Policy.Statement) != 1 {
		t.Errorf("expect parsed policy, got %+v", b.Policy)
	}
	if b.PolicyText == "" {
		t.Error("expect raw policy text")
	}
	if len(b.CORS) != 1 || b.CORS[0].AllowedOrigins[0] != "https://app.example.com" {
		t.Errorf("unexpected cors %+v", b.CORS)
	}
	// the backend secret must survive (Password masks on ordinary marshal)
	if b.Backend.SecretAccessKey.String() != "backendsecret" {
		t.Errorf("backend secret lost: %q", b.Backend.SecretAccessKey.String())
	}
}

// TestRDBStoreProxyE2E runs the proxy with the sqlite-backed store and a
// real SDK client end to end.
func TestRDBStoreProxyE2E(t *testing.T) {
	dsn := buildTestDB(t, loadTestConfig(t))
	cfg := &s3rp.Config{
		Store: &s3rp.StoreConfig{Driver: "sqlite", DSN: dsn},
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	app, err := s3rp.New(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	stub := newPolicyStub()
	app.SetBackend("photos", stub)
	ts := newTestServerForApp(t, app)
	client := newS3Client(t, ts.URL, "S3RPKEY001", "frontsecret001")

	out, err := client.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("photos"),
		Key:    aws.String("e2e.txt"),
	})
	if err != nil {
		t.Fatal(err)
	}
	out.Body.Close()
	// the backend receives the renamed bucket from the DB definition
	if aws.ToString(stub.getIn.Bucket) != "photos-prod" {
		t.Errorf("expect photos-prod, got %s", aws.ToString(stub.getIn.Bucket))
	}
	// a key of another tenant cannot access this bucket
	other := newS3Client(t, ts.URL, "S3RPKEY101", "frontsecret101")
	if _, err := other.GetObject(t.Context(), &s3.GetObjectInput{
		Bucket: aws.String("photos"),
		Key:    aws.String("e2e.txt"),
	}); err == nil {
		t.Error("expect AccessDenied for another tenant's key")
	}
}

func batchReadOnlyPolicyFor(bucket string) string {
	return `{
	  "Statement": [
	    {
	      "Effect": "Deny",
	      "Principal": {"S3RP": ["batch"]},
	      "Action": ["s3:PutObject"],
	      "Resource": ["` + bucket + `/*"]
	    }
	  ]
	}`
}
