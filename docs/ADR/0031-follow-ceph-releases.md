# 0031. The default Ceph backend is the newest GA release; EOL releases are dropped

- Status: Accepted
- Date: 2026-11-18
- Pull request: [#219](https://github.com/fujiwara/s3rp/pull/219)

## Context

The Ceph backend image is pinned in compose.yml and dependabot bumps it
within a release line. Majors are ignored there on purpose: the `x.1`
releases are release candidates the tag cannot tell apart, and a major can
need changes to `docker/ceph-demo.sh`. So nothing announced a new major.
Umbrella (v21.2.0) had been GA for three weeks before anyone looked, and it
did need a bootstrap change. It also behaves differently: eight s3-tests
cases that fail on tentacle for RGW's own multipart checksum and part-ETag
semantics pass on umbrella.

Which release is the default matters beyond the integration matrix: the
`ceph` service is the s3-tests backend, so its release decides what the
triage report calls expected.

## Decision

- The default `ceph` service runs the newest GA release (`x.2.0` and later).
- Every older release upstream still maintains keeps a `ceph-<release>`
  service in the integration matrix.
- A release past its end of life in Ceph's release index is not a test
  target: its service, matrix entry, s3-tests image choice and the
  workarounds that existed only for it are removed.
- A weekly workflow (`ceph-releases`, logic in
  `.github/scripts/ceph-releases.sh`) compares the repository with Ceph's
  `doc/releases/releases.yml` and opens one issue per event.

## Alternatives considered

- **Keep the default on the older release and add the new one beside it.**
  This is what #217 did. The newest release is where behavior changes
  arrive, and a service adopting the gateway deploys a current Ceph, so the
  suite that defines "expected" would describe the past.
- **Let dependabot propose majors.** It would propose the release
  candidates too, and its PR would fail on a bootstrap change with no hint
  of why.
- **Fail the scheduled job instead of opening an issue.** A red weekly job
  repeats every week and says nothing new; an issue is opened once and
  carries the steps.
- **Scrape the release table from docs.ceph.com, or use quay.io tags.**
  The table is rendered from `releases.yml`, which is structured and holds
  the end-of-life dates the tags do not.

## Consequences

- A GA release costs one hand-made PR: the swap, a bootstrap fix if needed,
  and an s3-tests run whose classification changes land with it.
- End-of-life dates in the index are estimates until upstream confirms
  them, and have slipped by months before. The issue says which kind it
  found; dropping a release on an estimate is a judgement, not automatic.
- The check trusts the layout of `releases.yml`. If it changes, the weekly
  job fails loudly on an empty index and does not go quiet.
- The check is a yq and jq script, not Go: it is glue around two YAML
  files. The two tools are pinned in the root `mise.toml`, which is where
  tools needed by workflows and maintenance scripts go.
