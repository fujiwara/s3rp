# 0039. Real S3 clients are run against the gateway

- Status: Accepted
- Date: 2026-11-20
- Pull request: [#238](https://github.com/fujiwara/s3rp/pull/238)

## Context

Clients in the field send requests that differ in small ways, and the
suites in place see few of them. s3-tests and the fidelity tests are
boto3 programs; the SigV4 cross-check replays other signers but stops at
the signature. The `x-amz-te` header of the AWS SDK for Java v2
([ADR 0032](0032-accept-headers-ignorable-by-protocol.md)) was found by a
user, not by a test, and it was not a signature problem: the request
verified and was then refused.

## Decision

Run client programs themselves against the `s3rp` binary and a real
backend (`clients/`), over http and https, weekly and on demand.

- Clients are chosen by lineage, one signer and HTTP stack each, starting
  with implementations that are not an AWS SDK: minio-go (`mc`, restic),
  s3cmd, s3fs-fuse, `object_store`, OpenDAL, DuckDB httpfs, Bun, and
  aws-sdk-go v1 (s5cmd); rclone for its request patterns.
- Scenarios are [runn](https://github.com/k1LoW/runn) runbooks written
  against abstract verbs (`put`, `get`, `list`, ...); a YAML definition
  per client maps each verb to one command line. What is checked and how
  a client does it are read separately, and neither is shell or Go.
- A scenario fails on its own checks or on any 5xx,
  `SignatureDoesNotMatch` or `AccessDenied` in the gateway's request log,
  and results are verified by reading objects back through curl.
- Every expected failure is listed with a reason in `clients/expected.tsv`,
  exact both ways, as in [ADR 0038](0038-run-aws-fidelity-tests.md).
- MinIO's client and client library are run. [ADR 0006](0006-test-backends-without-minio.md)
  keeps MinIO out as a backend; as a client it is what many deployments
  point at any S3 endpoint.

## Alternatives considered

- **A runner of our own interpreting a smaller YAML.** The shortest
  notation, but its meaning would live in a few hundred lines of shell,
  with key quoting — the thing under test — as its most fragile part.
  runn brings loops, value passing, assertions and failure output; what
  remains ours is starting the gateway, slicing its log per scenario and
  the report.
- **One runbook per client, without verbs.** Simpler to start, but the
  scenarios would be copied per client and drift apart, and a new
  scenario would have to be written ten times.
- **Capture each client's requests into the SigV4 cross-check corpus
  only.** Replays in plain `go test`, but covers the signature alone,
  which is not where the last failures were.
- **Add client libraries to go.mod and test in Go.** Only reaches Go
  clients, and pulls their dependency trees into the module.

## Consequences

- Found on the first runs: the gateway's handler sat behind an
  `http.ServeMux`, so every key with `//` or a dot segment was redirected
  ([#236](https://github.com/fujiwara/s3rp/pull/236)), and `s3cmd cp`
  fails by default on the PutObjectAcl it sends after a copy. ACL writes
  stay refused ([ADR 0004](0004-acl-disabled-buckets.md)); s3cmd is run
  with `acl_public = False`, and the setting is documented.
- The binary gained a `tls` option so that it can be run over https
  without a terminator ([#237](https://github.com/fujiwara/s3rp/pull/237)).
- Client versions are pinned in `clients/mise.toml`, which dependabot
  does not read: a new client release is tried only when the pin is
  bumped by hand.
- The run needs FUSE (s3fs) and network access to the clients' package
  registries, so it is not part of the pull-request checks.
