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
$ SCHEMES=https ./clients/run.sh   # default: http https
```

It needs docker (the compose versitygw backend), mise, Go, curl,
openssl, FUSE for s3fs, and Wine (32-bit, with xvfb) for WinSCP. For
every client and scheme it starts an s3rp serving a bucket of its own
(removed afterwards) and runs every scenario against it. Both schemes
are run because clients frame and sign bodies differently over TLS; the
https gateway uses a self-signed certificate the clients are made to
trust. The report is `clients/work/results/report.md`: a client ×
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
  not express (a restic repository, DuckDB reading Parquet).
- `lib/` — the verbs of clients that are libraries, as small scripts.
- `bin/` — checks that do not rest on the client under test (`same-object`,
  `no-object` read through curl).
- `expected.tsv` — every scenario expected to fail (client, scheme,
  scenario), with a category and the reason. A run fails on an unlisted
  failure and on a listed scenario that passes.
- `mise.toml` — runn and the clients, pinned. Dependabot does not read it:
  bump the versions by hand, and re-run. A client mise has no package for
  (`duck`, WinSCP) is downloaded by its definition's `setup` through
  `bin/fetch`, pinned there by URL and SHA-256.

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
