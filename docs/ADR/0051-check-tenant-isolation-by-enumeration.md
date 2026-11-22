# 0051. Tenant isolation is checked by enumeration against a specification

- Status: Accepted
- Date: 2026-11-22
- Pull request: [#274](https://github.com/fujiwara/s3rp/pull/274)

## Context

Tenant isolation is the property the gateway exists for, and it is not held
by one function. It is the composition of bucket resolution (a bucket of
another tenant is visible only to a principal its policy names), two
baselines (the owning tenant is allowed unless denied, anyone else denied
unless allowed), the user policy in front of both, the copy source resolved
inside the requester's tenant, and the rule that every operation reaches
its handler through the same authorization
([0015](0015-bucket-policy-model.md),
[0023](0023-s3-permissions-are-authorized-as-actions.md)). Its tests were
scenarios: a grant, a request that should pass, one that should not.

A scenario shows a rule holding where someone looked. What isolation needs
is the rule holding for every operation and every combination, including
the operation added next year.

## Decision

`s3gw/isolationspec_test.go` states the rules as a specification over a
deliberately small policy language — a pattern is exact or a prefix
followed by `*` — so that it needs no policy evaluator and can be checked
by reading. Two tests enumerate worlds in that language and hold the
gateway to it:

- **Every decision.** Every policy of up to two statements, in either
  order, over a small vocabulary (38,221), for every user, action and
  resource:
  bucket resolution, `authorize`, the per-object authorizer and its skip
  must each answer as the specification does.
- **Every operation.** Every operation of the `s3op` catalog — with and
  without a version, with each header that adds an action, copying from
  each bucket — sent through the HTTP handler as every user. A request
  passes authorization exactly when the specification admits it, with the
  actions the catalog lists; nothing reaches the backend before that; and
  to a requester the bucket is hidden from, the response is byte for byte
  the one the same request gets from a gateway where the bucket does not
  exist.

The last check is non-interference, tested as such: it compares two runs
and needs no statement of what the response should be, so it covers
whatever a response carries — status, headers (the CORS ones included),
body.

The requests are derived from the catalog, and an operation that is routed
without a request in the test fails it, so a new operation is covered or
the build is red.

The scope is small on purpose, on the small-scope hypothesis: a rule that
breaks, breaks in a world of two tenants and two statements. Within it the
check is exhaustive, not sampled.

## Alternatives considered

- **A model in Alloy or TLA+.** It would check that the rules are
  consistent, which reading the specification already does at this size.
  What is at risk is the code not following them, and a model in another
  language says nothing about the code without a second, hand-kept link.
- **Fuzzing the handler.** The space that matters is small and structured;
  enumerating it gives the same answer on every run and a failure that
  names its world.
- **More scenarios.** Coverage of the operations would still be whichever
  ones someone remembered.

## Consequences

- An operation added to the route tables needs a line in `isoRoutes`; a
  rule of the specification that changes is changed there deliberately,
  with this record's successor.
- The policy language of the specification is a subset. What a full
  pattern or a condition does is the policy package's to show
  ([0050](0050-hold-the-policy-evaluator-to-a-reference-model.md)); here
  the policy evaluator is taken as given and its composition is checked.
- CORS preflights are outside the property: they are unauthenticated by
  design and answer for any bucket that has rules
  ([0005](0005-cors-in-the-gateway.md)).
- The enumeration adds about ten seconds to the package's tests under the race
  detector; it runs its worlds in parallel to keep that down.
