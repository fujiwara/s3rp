# Policy specification

The policy semantics in [Lean 4](https://lean-lang.org/), with the properties they are meant to have stated as theorems. Lean checks each proof when it builds the project, so a successful build means every property holds for every policy and every request — not for the ones a test happened to try.

- [`PolicySpec/Semantics.lean`](PolicySpec/Semantics.lean) — the definitions: when a statement applies, what a bucket policy and a user policy decide, and what the gateway makes of the two (`permits`).
- [`PolicySpec/Properties.lean`](PolicySpec/Properties.lean) — the theorems. Read the statements; the proofs after `:= by` are Lean's to check.
- [`Main.lean`](Main.lean) — the definitions as a program, which `policy/spec_test.go` compares the Go evaluator with.

| Theorem | Says |
|---|---|
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

A theorem ending in `_iff`, and the others stated with `↔`, give a definition's meaning in full, so the definition cannot change without the theorem failing. The rest are consequences worth naming.

## Comparison with the evaluator

The theorems are about the definitions here, not about the Go code. What ties the two is a comparison: `TestSpec` in the `policy` package generates policies and requests, asks the `policyspec` executable and the evaluator, and fails on any answer that differs — the decision and the statement it names (`Decide`), visibility (`MentionsPrincipal`) and the user policy (`UserPolicy.Decide`).

```console
$ cd policy/spec
$ mise install
$ mise exec -- lake build --wfail PolicySpec policyspec
$ cd ../..
$ S3RP_POLICY_SPEC=$PWD/policy/spec/.lake/build/bin/policyspec go test ./policy -run 'TestSpec$' -v
```

`--wfail` turns warnings into failures, so a proof left unfinished (`sorry`) fails the build. Without `S3RP_POLICY_SPEC` the test is skipped: Lean is not needed for `go test`. A run logs its seed; `S3RP_POLICY_SPEC_SEED` repeats it and `S3RP_POLICY_SPEC_CASES` sets how many policies are drawn.

## Scope

- **Text is not specified.** The specification starts from a parsed policy: a principal is a tenant and a user, an address its bits. How `Parse` reads the JSON document, a principal name or a CIDR block is outside it, and the comparison converts those with Go's own parsers.
- **Actions are ASCII.** `matchAction` compares `A`–`Z` as `a`–`z` and nothing else, as the evaluator does; `Parse` refuses an action pattern with a character outside ASCII.
- **`permits` is not compared.** The gateway's decision over both policies is specified and has theorems, but its Go counterpart is in `s3gw`, which `s3gw`'s own enumeration tests cover ([ADR 0051](../../docs/ADR/0051-check-tenant-isolation-by-enumeration.md)).
- **`glob` has no theorems yet**; it is held to the evaluator by the comparison alone.
