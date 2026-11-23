# Policy specification

The policy semantics in [Lean 4](https://lean-lang.org/), with the properties they are meant to have stated as theorems. Lean checks each proof when it builds the project, so a successful build means every property holds for every policy and every request — not for the ones a test happened to try.

- [`PolicySpec/Semantics.lean`](PolicySpec/Semantics.lean) — the definitions: when a statement applies, what a bucket policy and a user policy decide, and what the gateway makes of the two (`permits`).
- [`PolicySpec/Requests.lean`](PolicySpec/Requests.lean) — the theorems about whole requests: several needs, a copy, DeleteObjects.
- [`PolicySpec/Glob.lean`](PolicySpec/Glob.lean) — what a pattern matches, as a relation (`Matches`), and the proof that `glob` computes it.
- [`PolicySpec/Properties.lean`](PolicySpec/Properties.lean) — the theorems. Read the statements; the proofs after `:= by` are Lean's to check.
- [`Main.lean`](Main.lean) — the definitions as a program, which the Go tests compare the evaluator and the gateway with (through `internal/policyspec`).

| Theorem | Says |
|---|---|
| `glob_iff`, `matchResource_iff`, `matchAction_iff` | A pattern matches what `Matches` says: `*` any run of characters, `?` any one, anything else itself |
| `matches_star`, `matches_literal`, `matches_prefix` | `*` matches everything; a pattern without wildcards only itself; literal characters and `*` the values starting with them |
| `matchAction_case` | Spellings of an action differing in the case of ASCII letters are one action |
| `user_entry_covers`, `tenant_entry_covers` | Whom `tenant/user` and `tenant/*` name |
| `everyone_includes`, `listed_includes`, `allBut_includes` | Whom `"*"`, `Principal` and `NotPrincipal` are about |
| `condition_unknown_source`, `condition_known_source` | When a condition holds; an unknown source address fails closed |
| `decide_deny_iff`, `decide_allow_iff`, `decide_none_iff` | What a bucket policy decides, and that the statement it names is the first that applies |
| `deny_wins`, `deny_refuses` | An applying Deny decides, whoever asks |
| `mentions_iff` | When a policy mentions a principal |
| `allow_applies_mentions` | The visibility gate hides no bucket its policy grants |
| `userAllows_iff`, `user_deny_wins` | What a user policy allows |
| `permits_iff` | The gateway's decision, statement by statement |
| `owner_baseline`, `foreign_baseline` | The same for a user of the owning tenant, and for anyone else |
| `foreign_needs_allow` | Another tenant's user gets in only by an applying Allow |
| `empty_policy_is_private` | Without statements, only the owning tenant gets in |
| `adding_deny_admits_nothing` | Adding a Deny never admits a refused request |
| `adding_allow_leaves_owner` | Adding an Allow changes nothing for the owning tenant |
| `admits_iff`, `copyReadable_iff` | A request is admitted when its bucket is visible, everything it needs is permitted and its copy source is readable |
| `hidden_bucket_admits_nothing` | A bucket that is not there for the requester admits no request |
| `admitted_needs_permitted`, `more_needs_admit_less` | No need is skipped; an action a header adds can only refuse |
| `foreign_admitted_by_allows` | Another tenant's user is admitted only with an applying Allow for each need |
| `copy_source_is_own_tenant` | A copy never reads from another tenant's bucket, whatever it grants |
| `deletes_each_as_alone`, `deletes_none_iff` | DeleteObjects answers for each key what a request for it alone would get, and refuses as a whole only a bucket that is not there |

A theorem ending in `_iff`, and the others stated with `↔`, give a definition's meaning in full, so the definition cannot change without the theorem failing. The rest are consequences worth naming.

## Comparison with the Go code

The theorems are about the definitions here, not about the Go code. What ties the two is comparison with the `policyspec` executable, and a test fails on any answer that differs:

- `TestSpec` in `policy` generates policies and requests and compares the evaluator: the decision and the statement it names (`Decide`), visibility (`MentionsPrincipal`) and the user policy (`UserPolicy.Decide`).
- `TestAuthorizationMatchesLeanSpec` in `s3gw` compares the gateway's own decision — bucket resolution, `authorize` and the per-object authorizer — with `visible` and `permits`, for every policy of up to two statements over a small vocabulary, as every user ([ADR 0051](../../docs/ADR/0051-check-tenant-isolation-by-enumeration.md)).
- `TestIsolationSpecMatchesLeanSpec` in `s3gw` compares `admits` and `deletes` with the Go isolation specification the gateway's handlers are held to (ADR 0051): every operation of the catalog with the actions it needs, a copy from each bucket, DeleteObjects key by key.

```console
$ cd policy/spec
$ mise install
$ mise exec -- lake build --wfail PolicySpec policyspec
$ cd ../..
$ export S3RP_POLICY_SPEC=$PWD/policy/spec/.lake/build/bin/policyspec
$ go test ./policy -run 'TestSpec$' -v
$ go test ./s3gw -run 'LeanSpec$' -v
```

`--wfail` turns warnings into failures, so a proof left unfinished (`sorry`) fails the build. Without `S3RP_POLICY_SPEC` the tests are skipped: Lean is not needed for `go test`. A run of `TestSpec` logs its seed; `S3RP_POLICY_SPEC_SEED` repeats it and `S3RP_POLICY_SPEC_CASES` sets how many policies are drawn.

## Scope

- **Text is not specified.** The specification starts from a parsed policy: a principal is a tenant and a user, an address its bits. How `Parse` reads the JSON document, a principal name or a CIDR block is outside it, and the comparison converts those with Go's own parsers.
- **Actions are ASCII.** `matchAction` compares `A`–`Z` as `a`–`z` and nothing else, as the evaluator does; `Parse` refuses an action pattern with a character outside ASCII.
- **Which actions an operation needs is not specified.** A request arrives at `admits` with its needs. That `PutObject` with a tagging header needs `s3:PutObjectTagging`, or that a `versionId` changes the action, is the catalog's (`s3op/operations.json`), read by the Go isolation specification and checked against the handlers there (ADR 0051).
