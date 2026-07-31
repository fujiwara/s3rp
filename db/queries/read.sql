-- Read-only queries used by the proxy (store/rdb).
-- Do not add INSERT/UPDATE/DELETE here; writes belong to write.sql,
-- which is generated into a package the proxy never imports.

-- name: GetKey :one
SELECT ak.access_key_id, ak.secret_access_key, u.name AS user_name, t.name AS tenant_name
FROM access_keys ak
JOIN users u ON u.id = ak.user_id
JOIN tenants t ON t.id = u.tenant_id
WHERE ak.access_key_id = ?;

-- name: GetBucket :one
SELECT b.name, b.backend, b.policy, b.cors, t.name AS tenant_name
FROM buckets b
JOIN tenants t ON t.id = b.tenant_id
WHERE t.name = ? AND b.name = ?;

-- name: GetBucketByName :one
SELECT b.name, b.backend, b.policy, b.cors, t.name AS tenant_name
FROM buckets b
JOIN tenants t ON t.id = b.tenant_id
WHERE b.name = ?;

-- name: ListBucketNames :many
SELECT b.name
FROM buckets b
JOIN tenants t ON t.id = b.tenant_id
WHERE t.name = ?;

-- name: TenantExists :one
SELECT EXISTS (SELECT 1 FROM tenants WHERE name = ?);
