# 0054. The policy document has a JSON Schema, held to Parse

- Status: Accepted
- Date: 2026-11-23
- Pull request: [#280](https://github.com/fujiwara/s3rp/pull/280)

## Context

The Lean specification ([0052](0052-state-the-policy-semantics-in-lean.md))
starts from a parsed policy. What a document may contain — which elements,
of what type, how many — was written nowhere but in `Parse` and in prose.
An author learned that a policy is malformed when a store loaded it.

A grammar is also something a policy author's tools can use, which the
semantics are not: an editor completes and checks against a schema, and a
pipeline can refuse a policy before it reaches a control plane.

## Decision

`docs/policy.schema.json` is the JSON Schema (draft 2020-12) of a bucket
policy document in the default dialect.

It is held to `Parse` by tests, in both directions:

- every document `Parse` accepts is valid against the schema;
- a document that is valid and that `Parse` refuses is refused for a reason
  a schema cannot state. Those are listed (`schemaResidue`): a resource that
  does not refer to the policy's bucket, a `Sid` given twice, a condition
  value that is not an address or a CIDR block, and a pattern over its
  limit in bytes where the schema counts characters.

`TestSchema` checks this over generated policies and mutations of them —
members dropped, added, renamed, retyped, lists grown past their limits.
The validator is the `jsonschema` CLI, installed by mise and named by
`S3RP_JSONSCHEMA`; without it the test is skipped, and the `policy-spec`
workflow runs it. `TestSchemaLimits`, which needs no tool, keeps the limits
in the schema equal to the package's constants.

The first direction is the one the schema promises its users. The second
keeps it from being trivially true: a schema that accepts everything would
satisfy the first.

## Alternatives considered

- **State well-formedness in Lean only.** One toolchain, and nothing a
  policy author can use.
- **A Go validator library as a test dependency.** The comparison could
  then be a fuzz target. It adds a module dependency to a package kept to
  the standard library for the sake of a test.
- **Generate the schema from the Go types, or the parser from the schema.**
  Either makes one side the other's output, and then they agree by
  construction and a mistake is in both. Two artifacts written separately
  and compared is the arrangement the reference model already uses
  ([0050](0050-hold-the-policy-evaluator-to-a-reference-model.md)).
- **A schema per dialect.** A dialect is a service's own surface syntax; its
  schema is the service's to publish.

## Consequences

- A change to what `Parse` accepts is a change to the schema in the same
  commit; the comparison fails otherwise.
- A document valid against the schema can still be refused, for the reasons
  listed. The schema's description says so.
- The user policy has no schema: its document form belongs to the store
  that carries it.
- A document with two members of one name cannot be told from one without
  by a validator that parses before it validates; `Parse` refuses it
  ([0053](0053-read-policy-documents-strictly.md)), and the comparison does
  not generate it.
- The validator gave wrong verdicts for a `.jsonl` file of documents
  (17.3.3), so the test writes one file per document.
