# 0021. Proxy-framework features deliberately not adopted

- Status: Accepted (recorded retroactively)
- Date: 2026-08-28
- Pull requests: none (a review; the adopted parts are
  [#126](https://github.com/fujiwara/s3rp/pull/126) and
  [#127](https://github.com/fujiwara/s3rp/pull/127))

## Context

The gateway was compared with other proxy frameworks and pipelines
(kroxylicious, Vitess, kube-apiserver, the OpenStack Swift pipeline,
LiteLLM) to find concepts it lacked. The filter was what fits a public,
multi-tenant cloud service.

## Decision

Adopted: denial reasons for the operator
([0019](0019-denial-reasons-for-the-operator-only.md)) and per-backend fault
isolation ([0020](0020-per-backend-circuit-breaker.md)).

Not adopted:

- **Hooks that change object data or read bodies.** Hooks may admit,
  refuse, meter and pace, but never touch customer data.
- **Flexible routing, or several backends per bucket.** Too complex to
  operate as a service.
- **Per-tenant virtual endpoints.** They break the model of globally unique
  bucket names ([0003](0003-users-are-the-identity.md)).
- **A field per backend fact on `RequestInfo`/`Op`.** A service carries such
  facts in the request context, filled by its own client middleware.

The same review kept SigV4 verification on aws-sdk-go-v2's signer, for
definitional SDK compatibility. That part was later reversed by
[0029](0029-build-sigv4-canonical-request-in-verifier.md).

## Consequences

- The hook surface stays small, and every hook is safe with respect to
  customer data.
- Proposals for these features should start from this record.
