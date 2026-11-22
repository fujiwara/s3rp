# Client compatibility

How S3 clients are run against s3rp, and how to read a run.
[clients/README.md](../clients/README.md) describes the harness and the
local run (`./clients/run.sh`); CI runs the same script weekly, and on
demand, via the [`clients` workflow](../.github/workflows/clients.yml). A
failed weekly run means the gateway moved a result since the last one —
the clients and the backend are pinned — so triage it as below.

## What it adds

A client program is run as its users run it, against the `s3rp` binary
and a real backend, with https on both sides as in production
([ADR 0041](ADR/0041-run-the-aws-sdks-as-clients.md)). That finds what the other suites
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
| aws-cli (`aws-cli-crt`) | the AWS CRT (`aws-c-s3`), the transfer client of `aws s3` | the most used client; a signer, HTTP stack and part scheduling apart from botocore's |
| aws-cli (`aws-cli-classic`) | botocore and s3transfer, as aws-cli v1 and `preferred_transfer_client = classic` | boto3's lineage under the `aws s3` command layer: `sync`, and the headers `cp` adds to a copy |
| `mc` | minio-go | the most used non-AWS client library; signer and streaming signatures of its own |
| restic | minio-go | the same library driven as a backup tool: its own repository scenario |
| kopia | minio-go | a second backup tool on it: ranged reads of pack blobs, and repository maintenance |
| s3cmd | Python, own implementation | long-lived in operations; sends ACL and location requests others do not |
| s3fs-fuse | C++ on libcurl, own signer | file operations become HEAD, ranged GET, multipart and copy requests |
| obstore | Rust `object_store` | what Polars, delta-rs and DataFusion use |
| OpenDAL | Rust, `reqsign` signer | a second Rust lineage |
| DuckDB httpfs | C++, own implementation | analytics: globbed listings and ranged reads of Parquet |
| Bun `S3Client` | Zig, own implementation | recent, and built into a runtime |
| s5cmd | aws-sdk-go v1 | a signer no other suite exercises (WAL-G, the Docker registry) |
| rclone | aws-sdk-go-v2 | the signer is covered elsewhere; its request patterns are not |
| Cyberduck (`duck`) | Java, own implementation | a desktop client on Windows and macOS, and the engine of Mountain Duck; probes folders and bucket configuration around every transfer |
| WinSCP | C++, a fork of libs3 | the common Windows file-transfer client; run under Wine |

The AWS SDKs are run as well, one per language
([ADR 0041](ADR/0041-run-the-aws-sdks-as-clients.md)). They share a
specification, not an implementation: each has its own signer, HTTP stack
and transfer manager, and they differ in the headers they add, in how
they send a checksum (a header, or an `aws-chunked` trailer) and in how
they encode `x-amz-copy-source`. Each is used the way its documentation
leads a user to — the default configuration plus endpoint, path style and
credentials, uploads through the SDK's transfer manager where it has one.

| client | SDK | uploads through |
|---|---|---|
| `sdk-python` | boto3 | `upload_file` (s3transfer) |
| `sdk-js` | AWS SDK for JavaScript v3, on Node.js | `@aws-sdk/lib-storage` |
| `sdk-go` | AWS SDK for Go v2 | `feature/s3/manager` |
| `sdk-java` | AWS SDK for Java v2, asynchronous client on Netty | `S3TransferManager`, multipart enabled |
| `sdk-dotnet` | AWS SDK for .NET | `TransferUtility` |
| `sdk-ruby` | AWS SDK for Ruby | `TransferManager` |
| `sdk-php` | AWS SDK for PHP | `ObjectUploader` |
| `sdk-rust` | AWS SDK for Rust | none in the SDK: parts sent by the script, as its documentation shows |

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

The backend is Ceph RGW, reached over https
([ADR 0041](ADR/0041-run-the-aws-sdks-as-clients.md)). A client's result
depends on the backend where the backend departs from S3, so the run uses
the one that departs least: versitygw's posix backend treats `dir//x` and
`dir/x` as one object, which let `double-slash-key` pass for clients that
drop the empty segment themselves.

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
- **backend** — RGW answers differently from S3 and the gateway relays
  the answer. Run the client against RGW directly to tell: it fails there
  too.

Below the table the report lists the operations each client sent. It is
not checked; it is there to show what a client really uses — `mc` asks
for the Object Lock configuration before every upload, s3cmd for the
bucket location before every request.

## Expected failures

Why each entry of `clients/expected.tsv` is expected.

- **obstore, `double-slash-key`** (client). `object_store` validates paths
  and refuses one with an empty segment before sending anything.
- **duck, OpenDAL, rclone, s3fs, `double-slash-key`** (client). Each
  removes the empty segment and stores `dir/x.txt`. On S3, as on RGW,
  that is another key (AWS Emulator 1.0.533), so the object is not where
  it was put.
- **sdk-java, sdk-ruby, `multipart`** (backend). The upload succeeds. The
  transfer manager then downloads the object part by part (`GetObject`
  with `partNumber`) and requires `Content-Range` on each response; RGW
  sends none, where S3 does (AWS Emulator 1.0.552; reported as
  [Ceph #83398](https://tracker.ceph.com/issues/83398)). The gateway
  cannot supply it: the part's offset is not in RGW's answer.
- **s3cmd, `presigned-get`** (gateway, deliberate). `s3cmd signurl` signs
  with Signature Version 2. The gateway verifies SigV4 only; SigV2 is
  HMAC-SHA1 over a canonical form of its own and Amazon S3 itself no
  longer accepts it in regions opened since 2014.

## Client settings the gateway requires

What a user of a client has to set, and what does not work, is in
[Using S3 clients with the gateway](clients.md), which is written from
this run's findings: a finding that changes what a user must do changes
that page in the same commit. The run itself uses those settings —
`acl_public = False` for s3cmd, `signature_version="s3v4"` for boto3.
