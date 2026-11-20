# Client compatibility

How S3 clients are run against s3rp, and how to read a run.
[clients/README.md](../clients/README.md) describes the harness and the
local run (`./clients/run.sh`); CI runs the same script weekly, and on
demand, via the [`clients` workflow](../.github/workflows/clients.yml). A
failed weekly run means the gateway moved a result since the last one —
the clients and the backend are pinned — so triage it as below.

## What it adds

A client program is run as its users run it, against the `s3rp` binary
and a real backend, over http and https. That finds what the other suites
cannot:

- [ceph/s3-tests](s3-tests.md) and the [fidelity tests](s3-fidelity.md)
  are boto3 programs: one client, sending what botocore sends.
- The [SigV4 cross-check](sigv4-canonicalization.md) replays signatures of
  other implementations, and stops at the signature: a header the gateway
  refuses, a request sequence it cannot serve, or a response a client's
  own parser rejects is past it.

The clients are chosen by lineage — one signer and HTTP stack each — not
by popularity alone: two tools on the same library mostly repeat each
other.

| client | lineage | why it is here |
|---|---|---|
| `mc` | minio-go | the most used non-AWS client library; signer and streaming signatures of its own |
| restic | minio-go | the same library driven as a backup tool: its own repository scenario |
| s3cmd | Python, own implementation | long-lived in operations; sends ACL and location requests others do not |
| s3fs-fuse | C++ on libcurl, own signer | file operations become HEAD, ranged GET, multipart and copy requests |
| obstore | Rust `object_store` | what Polars, delta-rs and DataFusion use |
| OpenDAL | Rust, `reqsign` signer | a second Rust lineage |
| DuckDB httpfs | C++, own implementation | analytics: globbed listings and ranged reads of Parquet |
| Bun `S3Client` | Zig, own implementation | recent, and built into a runtime |
| s5cmd | aws-sdk-go v1 | a signer no other suite exercises (WAL-G, the Docker registry) |
| rclone | aws-sdk-go-v2 | the signer is covered elsewhere; its request patterns are not |

MinIO is not used as a backend ([ADR 0006](ADR/0006-test-backends-without-minio.md));
its client and client library are here because deployments use them
against any S3 endpoint.

## What a scenario checks

Each scenario runs a client's commands and checks the outcome in two
ways that do not depend on the client: the object is read back through
curl and compared byte for byte, and the gateway's own request log for
the scenario must hold no 5xx (a 501 for something unhandled included),
`SignatureDoesNotMatch` or `AccessDenied`. The second is what catches a
client that swallows a refusal; the first is what catches a wrong result
the gateway never logged.

The backend is versitygw. What is under test is the gateway's front, and
a backend's own behavior belongs to the integration matrix. One
consequence: versitygw's posix backend treats `dir//x` and `dir/x` as one
object, so `double-slash-key` shows that such a key is served, not that
the two are kept apart.

## Reading a run

The report (`clients/work/results/report.md`, in CI the job summary) is a
client × scenario table: `pass`, `xfail` (failed as `clients/expected.tsv`
says), `FAIL`, or `skip` (the client has no such verb). The run fails on a
`FAIL`, and on an `expected.tsv` entry that passed.

To triage a `FAIL`, read `<scenario>.log` for the client's own error and
`<scenario>.requests.jsonl` for what the gateway answered, then decide
whose it is:

- **client** — the client does something by itself that the scenario
  cannot expect to work. List it in `expected.tsv` with the reason.
- **gateway** — the gateway refused or mishandled what the client sent.
  Either fix it, or, when the refusal is deliberate, list it with the
  reason and say here why it stays.

Below the table the report lists the operations each client sent. It is
not checked; it is there to show what a client really uses — `mc` asks
for the Object Lock configuration before every upload, s3cmd for the
bucket location before every request.

## Expected failures

Why each entry of `clients/expected.tsv` is expected.

- **obstore, `double-slash-key`** (client). `object_store` validates paths
  and refuses one with an empty segment before sending anything.
- **s3cmd, `presigned-get`** (gateway, deliberate). `s3cmd signurl` signs
  with Signature Version 2. The gateway verifies SigV4 only; SigV2 is
  HMAC-SHA1 over a canonical form of its own and Amazon S3 itself no
  longer accepts it in regions opened since 2014.

## Client settings the gateway requires

- **s3cmd: `acl_public = False`.** Left unset, `s3cmd cp` reads the
  source's ACL and writes it to the copy with PutObjectAcl, tolerating
  only a 501 in answer. The gateway refuses every ACL write with
  `AccessControlListNotSupported`
  ([ADR 0004](ADR/0004-acl-disabled-buckets.md)), so the copy is made and
  the command still fails. With the setting s3cmd sends neither request;
  the run uses it.
