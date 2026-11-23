# 0052. The policy semantics are stated in Lean, with their properties proved

- Status: Accepted
- Date: 2026-11-23
- Pull request: [#276](https://github.com/fujiwara/s3rp/pull/276)

## Context

The reference model of [0050](0050-hold-the-policy-evaluator-to-a-reference-model.md)
answers one question: does the evaluator compute what the model computes.
It cannot answer the other: is what the model computes what was meant. That
a Deny always decides, that another tenant gets in only by an Allow, that
the visibility gate hides no bucket its policy grants — these were sentences
in the documentation, true of the model as far as a reader could tell.

0050 set a model in another language aside because the comparison with the
Go code would still be testing, with a second toolchain in between. That
remains so. What a prover adds is on the other side of the model: the
properties themselves become checked statements.

There was also no document to call the specification. The semantics were
spread over prose in docs/s3-api.md, the model in a test file, and the
validation code.

## Decision

`policy/spec` states the semantics in Lean 4: when a statement applies, what
a bucket policy and a user policy decide, and what the gateway makes of the
two. The properties are theorems beside the definitions, and Lean checks
their proofs when it builds the project, so the build is the check.

Where a definition's whole meaning can be said, the theorem is an
equivalence (`permits_iff`, `decide_deny_iff`, `condition_known_source`,
...): the definition then cannot change without the theorem failing. A
one-way property constrains only what it mentions.

The definitions are also built as a program, and `TestSpec` in the `policy`
package compares the evaluator with it on generated policies and requests.
The test needs `S3RP_POLICY_SPEC` to name the executable and is skipped
without it; the `policy-spec` workflow builds the specification and runs the
comparison on every change under `policy/`.

Lean is installed by mise (`policy/spec/mise.toml`), the project has no
dependency beyond Lean's own library, and Mathlib is not used.

The Go model of 0050 stays. It is what the fuzz targets run against in every
`go test` and every night, without Lean.

## Alternatives considered

- **Properties as tests over the Go model.** No new toolchain, and a
  property is then checked on the policies a test draws, not proved. The
  model would still be test code with no place to state what it guarantees.
- **Alloy.** A declarative specification with bounded checking of its
  properties, but nothing to run: it could not be compared with the
  evaluator, and a specification tied to the code only by a reader drifts.
- **Replace the Go model with the Lean one.** Every fuzz run would then
  need Lean, and the nightly fuzzing would cross a process boundary per
  input.

## Consequences

- A change to the policy semantics is made in three places in one commit:
  the evaluator, the Go model, and `policy/spec`. The comparison and the
  fuzz targets fail until they agree.
- The theorems are about the definitions, not the Go code; the comparison is
  the only tie, and it is testing. What it does not draw is not compared.
- A theorem proves what it states. Whether the statements are the right
  ones, and whether one is missing, is for a reader of
  `PolicySpec/Properties.lean` to judge — a few hundred lines, in place of
  the evaluator.
- The specification starts from a parsed policy. The text of a document, a
  principal name or a CIDR block is outside it.
- Writing the action comparison down showed that the evaluator's was not
  the one meant. It lower-cased by Unicode, so a letter outside ASCII whose
  lower case is ASCII spelled an action: `s3:ListBuc\u212aet`, with the
  Kelvin sign, matched `s3:ListBucket`. Actions are now ASCII — `Parse` and
  `ValidateUserPolicy` refuse a pattern with any other character — and only
  `A`–`Z` fold, in the evaluator, the Go model and the specification alike.
  A policy with such a pattern stops loading; none can have been written by
  accident.
- A Lean upgrade can break a proof. The version is pinned and bumped by
  hand.
