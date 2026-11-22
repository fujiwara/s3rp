# AWS fidelity tests

Runs the S3 tests of
[aws/aws-emulator-evaluation-suite](https://github.com/aws/aws-emulator-evaluation-suite)
against s3rp, proxying to the Ceph RGW backend from `compose.yml`. Each of
those tests is a short boto3 program asserting how Amazon S3 itself
behaves — the result, and for a refusal the exact error code and message —
and was verified against real AWS accounts. How to read a run, and why
each expected-failure category is expected, lives in
[docs/s3-fidelity.md](../docs/s3-fidelity.md).

The suite was written to score the AWS Emulator; the emulator itself is
not used here. Its runner only needs an endpoint on loopback and a key
file, which the s3-tests harness provides
([s3tests/README.md](../s3tests/README.md): it answers CreateBucket and
DeleteBucket, which the gateway deliberately does not have).

## Running

```sh
./s3fidelity/run.sh
```

Or in CI: the [`s3-fidelity` workflow](../.github/workflows/s3fidelity.yml),
which runs weekly and can be triggered by hand.

Prerequisite: [mise](https://mise.jdx.dev/) — uv is pinned in
`s3fidelity/mise.toml`, jq in the root `mise.toml`; the suite pins its own
Python and dependencies (`uv.lock`).

`S3RP_TEST_BACKEND_ENDPOINT=memory ./s3fidelity/run.sh` runs the same
tests on the in-memory backend (`s3gw/localbackend`) instead of Ceph.
Its expected failures are `expected-localbackend.tsv`, which the script
picks for these two endpoint forms (`directory:PATH` is the same backend
kept in `PATH`). CI runs it beside the Ceph run.

The script starts the `ceph` compose service, builds and starts the
harness on `127.0.0.1:7482`, checks out the suite at the pinned commit
into `s3fidelity/work/` (gitignored), selects the tests the gateway can
serve, runs each in its own runner process (a few minutes in all), and
writes under `s3fidelity/work/results/`:

- `report.md` — the triage report
- `reports/*.json` — the runner's verdict per test
- `logs/*.log` — the runner's output per test
- `selected.txt` — the tests that ran
- `harness.log` — the gateway's request log (what the client was told and
  the backend's own error behind it, by request id)

It exits 1 when a test failed that `expected.tsv` does not list, or a
listed one passed.

## Which tests run

A test runs when every operation its `manifest.jsonl` row names is one the
operation catalog (`s3op/operations.json`) marks supported, or one of the
harness's two. That is 162 of the suite's 539 S3 tests at the pinned
commit; the rest need bucket-configuration writes, IAM, or other services.
A newly supported operation widens the selection by itself.

## Files

- `run.sh` — the whole run; the suite commit is pinned at the top
- `expected.tsv` — every test expected to fail, with its category
- `triage.py` — compares a run with `expected.tsv`
