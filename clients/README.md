# Client compatibility

Runs S3 clients that real deployments use against s3rp and checks that
what they send is served. The other suites drive the gateway with boto3
(s3-tests, s3fidelity) or replay signatures (sigv4 crosscheck); this one
runs the client programs themselves, so a header, query parameter, body
framing or request sequence only one client sends is found here. How to
read a run, and why each expected failure is expected, is in
[docs/client-compat.md](../docs/client-compat.md).

```console
$ ./clients/run.sh                 # every client in clients/defs
$ ./clients/run.sh rclone mc       # the named ones
$ SCHEMES=http ./clients/run.sh    # default: https
$ JOBS=1 ./clients/run.sh          # default: 4 clients at a time
```

It needs docker (the compose Ceph RGW backend, `ceph-tls`), mise, Go, curl,
openssl, FUSE for s3fs, and Wine (32-bit, with xvfb) for WinSCP; the
AWS SDK for PHP runs in a container. For
every client and scheme it starts an s3rp serving a bucket of its own
(removed afterwards) and runs every scenario against it. Clients run
`JOBS` at a time, each against a gateway of its own on its own port; only
the backend is shared, since a scenario's verdict reads the log of the
gateway that served it. The gateway is
served over https and reaches its backend over https, as a deployment
does: clients frame and sign bodies differently without TLS, and so does
the gateway toward its backend. Both certificates are issued by a CA made
for the run, which the clients and the gateway are made to trust. The
CA and the backend container are kept between runs (`docker compose down
ceph-tls` and removing `work/tls` start over); a client's bucket is removed
when it ends, so only an interrupted run leaves one behind. A
client that cannot be made to trust it names `schemes: [http]` in its
definition and is run over http. The report is `clients/work/results/report.md`: a client ×
scenario table, the expected failures, and the operations each client
sent.

## Layout

- `scenarios/*.yml` — what is checked, as [runn](https://github.com/k1LoW/runn)
  runbooks. They are client-independent: a step runs a *verb*
  (`{{ vars.client.verbs.put }}`) with its arguments in shell variables.
- `defs/<client>.yml` — how one client does each verb, one command line
  per verb, and what points it at the gateway (`env`, and a `setup` /
  `teardown` script for a client that needs a config file or a mount). A
  scenario using a verb the client lacks is skipped and reported as such.
- `own/<client>/*.yml` — scenarios for one client only, for what verbs do
  not express (a restic or kopia repository, DuckDB reading Parquet).
- `lib/` — the verbs of clients that are libraries, as small scripts;
  `lib/sdk/` holds one per AWS SDK (`defs/sdk-*.yml`), with the SDK's
  version pinned beside it in the form its language uses (a lock file, or
  the dependency line of a single-file script).
- `bin/` — checks that do not rest on the client under test (`same-object`,
  `no-object` read through curl).
- `expected.tsv` — every scenario expected to fail (client, scheme,
  scenario), with a category and the reason. A run fails on an unlisted
  failure and on a listed scenario that passes.
- `mise.toml` — runn, the clients and the language runtimes of the SDKs,
  pinned. A client mise has no package for (`duck`, WinSCP) is downloaded
  by its definition's `setup` through `bin/fetch`, pinned there by URL and
  SHA-256.
- `bump.sh` — bumps every pin (see Versions).

## Versions

Everything is pinned to an exact version and bumped all at once, followed
by a run. What a pin is bumped to:

| | pinned to | |
|---|---|---|
| a client, an AWS SDK | the latest release | |
| Go, Python, Ruby, Rust, PHP, Bun | the latest stable release | the language has one supported line, or its users follow the newest |
| Java, .NET | the latest release of the newest LTS line | the lines between two LTS are short-lived and deployments stay on LTS |
| Node.js | the latest release of the newest even-numbered line | an odd-numbered line is supported for months and never becomes LTS |

`./clients/bump.sh` does that to every pin: `mise.toml`, an SDK's version
beside its script (and its lock file), the Python line in a script's
header, the Java line of `lib/sdk/java/Client.java`, the `composer` image
of `lib/sdk/php.sh` (which brings PHP), and the downloads in
`defs/duck.yml` and `defs/winscp.yml`. The weekly `clients-bump` workflow
runs it and opens a pull request. The checks of a pull request a workflow
opened wait for a person: approve them there, and merge once the
`clients` run has passed.

What is not AWS's own is held back a week (`minimum_release_age` in
`mise.toml`, which the script applies to the pins outside it too): a
release that is withdrawn or tampered with is usually found within days.
That covers the clients, the runtimes and the third-party libraries a
script names; an AWS SDK is bumped the day it is released, and what an SDK
depends on comes with it, as its lock file resolves. The script leaves two
pins alone: `slf4j-nop`, which stays on the 1.x line the SDK for Java
binds to, and the Go of `lib/sdk/go/go.mod`, which follows the
repository's.

## What fails a scenario

- a step's `test:`, or
- any request the gateway answered with a 5xx (501 for something it does
  not handle included), `SignatureDoesNotMatch` or `AccessDenied`, whatever
  the client's exit code says.

Each scenario leaves, under `work/results/<scheme>/<client>/`, the
requests the gateway logged for it (`<scenario>.requests.jsonl`) and every
command with its output (`<scenario>.log`).

## Verbs

| verb | arguments | |
|---|---|---|
| `put` | `FILE`, `KEY` | |
| `get` | `KEY`, `OUT` | |
| `list` | `PREFIX` | prints one line per object |
| `delete` | `KEY` | |
| `copy` | `SRC`, `DST` | server-side |
| `presign` | `KEY` | prints a URL for GET |
| `put_tree` | `DIR`, `PREFIX` | |
| `lifecycle` | | prints the bucket's lifecycle rules |
| `replication` | | prints the bucket's replication rules |

The arguments are shell variables, not environment: a verb that runs a
script passes them on its command line. `BUCKET`, `ENDPOINT`, `ACCESS_KEY`
and `SECRET_KEY` are in the environment, and a definition's `env` and
`setup` may also use `SCHEME`, `PORT`, `CA_FILE`, `LIB`, `TOOLS` (where `setup` keeps a
downloaded client) and `STATE_DIR` (a directory for the client's own
configuration).

## Adding a client

Pin it in `mise.toml`, write `defs/<client>.yml`, run it
(`./clients/run.sh <client>`), and look at the failures: one that is the
client's own doing goes in `expected.tsv`; one that is the gateway's is a
bug to fix or a decision to record. Add the client to the table in
docs/client-compat.md.

## Writing a scenario

Things runn does that are easy to trip on:

- A runbook-level `loop:` has no index; loop each step (`loop: N`, index
  `i`) instead.
- `vars.client` is the client definition, so a verb name must be an
  identifier (`put_tree`, not `put-tree`).
- A scenario that would pass without doing anything proves nothing: check
  the result through `same-object` / `no-object` or the client's output,
  not the exit code alone.
