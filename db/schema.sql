-- s3rp schema: tenants, users, access keys and buckets.
-- The single source of truth for both sqlc code generation and migration.
CREATE TABLE IF NOT EXISTS tenants (
  id   INTEGER PRIMARY KEY,
  name TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS users (
  id        INTEGER PRIMARY KEY,
  tenant_id INTEGER NOT NULL REFERENCES tenants (id),
  name      TEXT NOT NULL,
  policy    TEXT NOT NULL DEFAULT '', -- user identity policy as JSON ('' = allow all)
  UNIQUE (tenant_id, name)
);

CREATE TABLE IF NOT EXISTS access_keys (
  access_key_id     TEXT PRIMARY KEY, -- globally unique
  user_id           INTEGER NOT NULL REFERENCES users (id),
  secret_access_key TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS buckets (
  id        INTEGER PRIMARY KEY,
  tenant_id INTEGER NOT NULL REFERENCES tenants (id),
  name      TEXT NOT NULL UNIQUE, -- globally unique
  backend   TEXT NOT NULL,        -- store.Backend as JSON
  policy    TEXT NOT NULL DEFAULT '', -- bucket policy JSON text ('' = none)
  cors      TEXT NOT NULL DEFAULT ''  -- []store.CORSRule as JSON ('' = none)
);

-- ListBucketNames filters by tenant_id. buckets.name already has an implicit
-- unique index for GetBucketByName, but tenant_id needs its own.
CREATE INDEX IF NOT EXISTS idx_buckets_tenant_id ON buckets (tenant_id);
