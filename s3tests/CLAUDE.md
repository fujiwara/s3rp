# CLAUDE.md — analyzing s3-tests results

How to read a suite run and, in particular, how to find operations that
are *implementable but not implemented* — as opposed to deliberately
refused. README.md here covers running the suite; docs/s3-tests.md holds
the classification rationale and the dated history, **not** current
numbers (those live in the latest run's report). Update it only when a
classification decision changes — same commit as the `triage.py` rule
change — never just because a re-run moved the counts.

## Where results are

- Local run: `s3tests/work/results/` — `report.md` (mechanical triage),
  `results.xml` (junit), `pytest.out`, `harness.log` (JSON lines,
  correlate by bucket name / `x-amz-request-id`).
- CI run: `gh run download <run-id> -n s3tests-results`. The job summary
  shows the backend Ceph version — always note it; the failure mix
  changes with the backend (see the 19.2.0 vs 20.2.1 sections of
  docs/s3-tests.md).

## triage.py is a first pass, not the answer

Its rules encode the *hand-verified* findings of docs/s3-tests.md, so on
the verified baseline every failure lands in a named category and the
`UNMATCHED` bucket is the actual to-triage list — anything appearing
there after a backend or suite update is either a new incompatibility or
a pattern the rules don't know yet. The name-based rules are heuristics:
when the failure mix shifts, spot-check that reclassified tests really
belong where they landed (a *newly failing* test matching an old pattern
is exactly what to distrust). The `NotImplemented` group is keyed by the
**boto3 operation name in the ClientError** — which conflates three
different causes. Split them by the **501 message text**, not the
operation name:

| message | cause |
|---|---|
| `query parameter X is not implemented` | proxy: genuinely unimplemented subresource/parameter (the loud-501 rule — reads included) |
| `X is not implemented` (SSE-C, an operation name, ...) | proxy: implemented operation refusing an unimplemented *feature* — e.g. PutObject×N that are all SSE-C tests |
| `A header that you provided implies functionality that is not implemented.` | **backend**: RGW's own 501 — s3err replaces backend messages with S3's canonical wording, and that sentence is the canonical text for code `NotImplemented`. Not a proxy gap (e.g. RGW refusing copy of an SSE-KMS object) |

Proxy-raised 501s always come from `s3err.NotImplemented(what)` and read
"`<what>` is not implemented"; anything else worded came from the
backend. When unsure, probe the backend directly (bypassing the proxy)
with the same request — `curl --aws-sigv4` against the backend endpoint,
or the suite's venv boto — and compare. That direct-probe step is also
how every "backend limitation" claim in docs/s3-tests.md was verified;
don't classify a failure as backend-caused without it.

## Judging: implementable vs deliberate

Deliberate refusals — never propose implementing these (rationale in the
root CLAUDE.md and README; several are "do not regress" decisions):

- **Bucket-configuration writes**: PutBucketVersioning / Policy / Cors /
  Encryption / Lifecycle / Logging / OwnershipControls /
  PublicAccessBlock / PutObjectLockConfiguration. Written by the control
  plane, not through a data-plane key.
- **ACL writes and real ACL reads** (fixed-stub by design), **SSE-C**
  (silent drop would fail open), **anonymous access**, **SigV2** (the
  `test_post_object_*` family signs with V2).
- **RGW extensions**: `allow-unordered`, `usage`, `x-rgw-*` headers,
  `x-amz-tagging-count` on HEAD — not in the S3 API model; matching RGW
  here is a non-goal.

A 501 is an *implementation candidate* when all of these hold:

1. It is in the S3 API model (the aws-sdk-go-v2 `s3` package has the
   operation), not an RGW extension.
2. It is read-only, or an object-level write — not bucket configuration.
3. The response exposes no backend identity that would need masking: no
   backend bucket name, region, or operator account as Owner/Initiator
   (compare how existing operations substitute `rt.cfg.Name` and
   `tenantOwner`).
4. It maps to an existing or obvious `s3:*` action for `authorize` (check
   the AWS docs for the required permission — e.g. GetObjectAttributes
   authorizes as `s3:GetObject`).
5. Enforcement or storage semantics stay the backend's job (pass-through
   is enough), or the gateway can implement them fully — no silent
   partial support. If only part of a feature can be honored, refusing
   loudly stays correct.

GetObjectAttributes was the candidate the 20.2.1 baseline surfaced and
has since been implemented (note that API returns the ETag *without*
quotes). As of that baseline, everything else in the 501 bucket is
deliberate or backend-caused.

## Comparing two runs

Diff the junit XMLs, not the reports — pass/fail transitions are the
signal (this found the conditional-write and SSE-KMS shifts):

```python
import xml.etree.ElementTree as ET
def outcomes(p):
    o = {}
    for c in ET.parse(p).getroot().iter('testcase'):
        o[c.get('name')] = ('skip' if c.find('skipped') is not None else
            'fail' if (c.find('failure') is not None or c.find('error') is not None) else 'pass')
    return o
old, new = outcomes('old/results.xml'), outcomes('new/results.xml')
print('fixed:', sorted(n for n in new if new[n]=='pass' and old.get(n)=='fail'))
print('broke:', sorted(n for n in new if new[n]=='fail' and old.get(n)=='pass'))
```

Anything in the `RUN CONTAMINATED` category means the backend or proxy
died mid-run — find the first casualty in `harness.log`, fix or deselect
it, and rerun before analyzing anything else.

## Rerun gotchas

- `PYTEST_ARGS` is word-split by run.sh: quoted expressions with spaces
  (`-k "a or b"`) do not survive — use space-free `-k` patterns or edit
  the deselect/marker variables. Positional test-id args are *unioned*
  with the default file list, selecting everything.
- A single test rerun still needs the harness: `run.sh` manages it; when
  probing by hand, `go run ./cmd/s3tests-harness` and mind that the
  in-memory store starts empty (leftover backend buckets are adopted on
  CreateBucket, not listed).
- The local aws CLI may crash with `argument of type 'NoneType'` on some
  error responses — use `curl --aws-sigv4` or the venv's boto for error
  probes.
- MicroCeph's RGW daemon is `client.radosgw.gateway`; `ceph config set
  client.rgw ...` silently does not reach it.
