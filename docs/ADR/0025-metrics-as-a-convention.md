# 0025. Metrics are a convention defined in a Weaver registry

- Status: Accepted (recorded retroactively)
- Date: 2026-09-28
- Pull requests: [#144](https://github.com/fujiwara/s3rp/pull/144),
  [#150](https://github.com/fujiwara/s3rp/pull/150)

## Context

The gateway emits no metrics ([0009](0009-observe-not-log.md)); every
service derives its own from `RequestInfo` and the cache stats. Without a
shared convention, names, units and attributes would differ between
services, and dashboards would not carry over. Some attributes (object
keys, users, access key ids, request ids) would explode metric cardinality.

## Decision

- The convention is an OpenTelemetry Weaver registry in `s3gw/semconv/model`,
  depending on the upstream semantic conventions for `http.*`, `error.type`,
  `aws.s3.bucket` and `network.io.direction`. `make semconv` checks it and
  generates `s3gw/semconv/semconv_gen.go` and docs/metrics.md, which are
  never edited by hand.
- Rego policies enforce the cardinality rule: the object key, the user, the
  access key id and request ids never go on a metric; tenant and bucket are
  opt-in.
- `s3gw/otelmetric` is the reference implementation and depends on the
  OpenTelemetry metrics **API** only. The SDK and exporters are the
  service's choice.
- The s3rp binary exports the metrics over OTLP when configured (#150), and
  CI's versitygw run checks the emitted metrics against the registry with
  `weaver registry live-check`.
- One repository and one module stay. A separate exporter repository was
  rejected.

## Alternatives considered

- **Emit metrics from the gateway.** Rejected: it would impose a metrics
  stack on every service.
- **A separate exporter repository.** Rejected.

## Consequences

- Drift between the registry, the code and the docs fails CI.
- The weaver version and the pinned upstream semconv release need manual
  attention on weaver upgrades.
