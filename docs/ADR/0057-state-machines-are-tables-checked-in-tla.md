# 0057. State machines are transition tables, model-checked in TLA+

- Status: Accepted
- Date: 2026-11-26
- Pull request: TBD

## Context

Several parts of the code are state machines that were not written as one.
The aws-chunked decoder was three flags and two counters whose combinations
were its phases; the payload verifier one `done` flag that stood for both
"verified" and "refused"; a blob file's life three maps it was in or not;
a multipart upload two maps; the end of a request a closure over `started`
and `reported`. The circuit breaker had its states named, with what each
event does spread over two methods.

What these machines guarantee was in comments — "a probe that is never
reported is simply followed by the next", "a blob is on disk before
meta.json names it" — and each comment was an argument about every
interleaving, checked by whoever read it. Tests and the fuzz targets try
the orders they come to; none of them tries the process stopping between
two particular steps of a flush.

[0052](0052-state-the-policy-semantics-in-lean.md) stated the policy
semantics in Lean. Those are functions of their input, and a prover suits
them. These are transitions over time, with concurrency and failure, which
is what a model checker is for.

## Decision

A state machine in the code is a named state, a named event and a
**transition table** — `breakerTransitions`, `chunkTransitions`,
`payloadTransitions`, `reportTransitions`, `blobTransitions`,
`uploadTransitions`, `versioningTransitions` — and the code around it only
decides which event happened and carries out the table's step. An event the
table does not have in a state fails closed.

`tla/` has a TLA+ specification of each machine, with what it guarantees as
properties, and `tla/check.sh` checks them with TLC. A test in each package
writes its table out as a TLA+ module the specification extends
(`internal/tlatable`), and fails when the module in the tree is stale: what
is model-checked is the table the code runs on. What a table does not hold
— the clock, the counters, the steps of concurrent requests — is written in
the specification by hand, beside the name of the Go function it follows.

The `tla` workflow runs both on a change to the specifications or to the
packages they are of. TLC is the pinned release jar, fetched by `check.sh`
and held to its checksum; Java comes from mise (`tla/mise.toml`).

## Alternatives considered

- **Leave the flags, write the models beside them.** The specification
  would then be of a reading of the code, tied to it by nothing; the first
  change to a flag would leave a model that still passes.
- **Generate Go from the specification, or check Go traces against it.**
  Either ties the handwritten part too. PlusCal-to-Go does not exist in a
  form to depend on, and trace validation needs the code instrumented and
  TLC in every test run, for machines of four states.
- **Lean, as for the policy.** The properties here are about interleavings
  and about every point a process can stop at. Stating them as theorems
  means building the transition system and its induction by hand; TLC
  enumerates it.
- **Property tests in Go over the tables.** They check a table against
  properties of the table. The properties that matter are of the table
  together with what happens around it — two requests, a failed write, a
  crash.

## Consequences

- Changing a state machine is changing its table, regenerating the module
  (`S3RP_TLA_UPDATE=1 go test ... -run 'Table$'`) and running
  `tla/check.sh`, in one commit. A new guarantee is a new property.
- The handwritten part of a specification is held to the code by reading.
  `RequestLifecycle.tla` is mostly that: the order of the checks is Go's
  control flow. The tests of the same behavior remain what checks it.
- The models are small — two or three of everything. A property is checked
  for every behavior within those bounds, not beyond them.
- Writing the models showed three things the code got wrong, fixed in the
  same change:
  - The decoder handed out the bytes that arrived together with a read
    error. Where they were a chunk's last, a consumer that had the declared
    length by them and did not look at the error (`io.ReadFull`) held a
    chunk nobody verified. A failed `Read` now hands out nothing.
  - The payload verifier's refusal was not final: a `Read` after
    `XAmzContentSHA256Mismatch` reported the end of the body. It now
    refuses again, and refuses bytes beyond the length it hashed.
  - A browser upload under its policy's minimum length was stored and then
    deleted. The object was there in between, stayed if the delete failed
    (its error was not looked at), and on an unversioned bucket the delete
    took whatever another request had written to the key by then. The body
    now fails where it ends, so the backend's write fails with it.
- They also showed two things that are left as they are, recorded in
  tla/README.md: any answered attempt closes the breaker, including one
  admitted before it opened; and a blob whose object is deleted before its
  write returns waits for the next flushed change to be collected.
- `localbackend` keeps a blob's state on the blob in place of the `pending`
  and `garbage` maps. `meta.json` is unchanged.
