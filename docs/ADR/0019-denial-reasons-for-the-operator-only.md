# 0019. Denial reasons go to the operator, never to the client

- Status: Accepted (recorded retroactively)
- Date: 2026-08-28
- Pull request: [#126](https://github.com/fujiwara/s3rp/pull/126)

## Context

Every refusal looked the same in the log. A policy `AccessDenied` could not
be told from an authentication failure, and a failed authentication lost
the access key id it claimed. Operators answering "why was I denied?" had
nothing to go on. The client must learn nothing more, though: a
cross-tenant requester must not learn the shape of another tenant's
policy, and Amazon S3 returns nothing either.

## Decision

- Every policy refusal carries a `*DenyReason` as its error cause: the
  layer (user policy, bucket policy, cross-tenant baseline, visibility gate,
  copy source), the statement by index and Sid, the principal, the action
  and the resource.
- Every verifier refusal carries a `*sigv4.AuthFailure`: a reason constant,
  the claimed access key id, and a non-secret detail. The claimed id is
  recorded on `RequestInfo.AccessKeyID`, unverified.
- Both reach the observer through the existing cause channel
  ([0009](0009-observe-not-log.md)). None of it is rendered into the XML.
- `Sid`s are validated unique per policy, so a reason can name a statement.
- DeleteObjects folds its per-key denials into `Op.Denials`, deduplicated by
  statement, so the summary stays bounded.

## Alternatives considered

- **Explain the denial in the response.** Rejected: it would disclose policy
  structure to the requester.

## Consequences

- Operators can answer denial questions from the log.
- The allow path builds nothing; only refusals pay for the explanation.
