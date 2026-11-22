# 0050. The policy evaluator is held to a reference model

- Status: Accepted
- Date: 2026-11-22
- Pull request: [#273](https://github.com/fujiwara/s3rp/pull/273)

## Context

Policy evaluation decides every request, and a wrong answer there is a
tenant reading or writing what a policy forbids. The evaluator is also the
most optimized code in the package: patterns precompiled to runes, a
backtracking wildcard matcher, per-object evaluators that pre-match the
principal, the action and the condition
([0015](0015-bucket-policy-model.md)), and shortcuts that skip the
per-object check altogether. Its tests were tables of cases someone thought
of.

Go has no verifier that would prove this code against a specification. What
is available is the lightweight form: write the specification as a program
simple enough to check by reading, and let a fuzzer look for an input on
which the two differ.

## Decision

`policy/model_test.go` holds a reference model of the policy semantics —
the wildcard definition as a recurrence, principal coverage, the source
address condition, "an applying Deny decides, else an applying Allow" — that
reads only a `Policy`'s exported fields and shares no code with the
evaluator. Fuzz targets (`policy/fuzz_test.go`) hold every evaluation path
to it: `Decide`, the Deny and Allow evaluators with their skip shortcuts,
`MentionsPrincipal`, `UserPolicy.Decide`, `Match`, and for arbitrary text
that `Parse` accepts, the structure the evaluator assumes.

The model is kept naive. An optimization belongs in the evaluator; a rule
that changes is changed in both, deliberately, in one commit.

The first run found two statements that did not apply where their author
meant them to, both fixed with this record:

- A `*` in a pattern was taken as a literal character where the value had a
  `*` at that position, which ended its run there: a Deny on `b/logs/*` did
  not cover the key `logs/*x`.
- An IPv4-mapped CIDR value (`::ffff:10.0.0.0/104`) was kept as an IPv6
  range while sources are unmapped to IPv4, so it contained no source: a
  Deny with `IpAddress` on it never applied, an Allow with `NotIpAddress` on
  it always did. Such a value is now its IPv4 range, and one wider than
  `::ffff:0:0/96` is refused.

## Alternatives considered

- **Prove the Go code (Gobra).** It verifies a subset of Go against
  annotations; the evaluator would have to be rewritten to fit, and the
  annotations maintained by people who do not otherwise use the tool.
- **Encode policies for an SMT solver.** That answers questions about one
  policy ("is this bucket open to another tenant"), which is a feature for a
  control plane. It says nothing about whether this evaluator computes what
  the encoding says.
- **A model in another language (Lean, Dafny, TLA+).** The comparison with
  the Go code would still be differential testing, with a second toolchain
  in between.
- **More table tests.** Both findings were inputs nobody had thought of.

## Consequences

- The fuzz workflow discovers the targets, so they run nightly; their seed
  corpus and the committed crashers run in every `go test`.
- A key containing `*` is matched by a wildcard at that position, as the
  documentation always said. A policy that relied on the old behavior
  cannot exist: it only ever made a statement apply to fewer keys.
- Equivalence with the model is not correctness of the model. `TestModel`
  pins it to the answers the documentation gives, and the model is short
  enough to review as the specification.
