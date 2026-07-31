-- Write queries used by the admin tooling / control plane only.
-- The proxy must never import the package generated from this file.

-- name: CreateTenant :one
INSERT INTO tenants (name) VALUES (?) RETURNING id;

-- name: CreateUser :one
INSERT INTO users (tenant_id, name) VALUES (?, ?) RETURNING id;

-- name: CreateAccessKey :exec
INSERT INTO access_keys (access_key_id, user_id, secret_access_key) VALUES (?, ?, ?);

-- name: CreateBucket :exec
INSERT INTO buckets (tenant_id, name, backend, policy, cors) VALUES (?, ?, ?, ?, ?);
