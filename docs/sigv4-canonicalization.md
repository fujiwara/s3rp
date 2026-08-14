# SigV4 query canonicalization across implementations

How S3 implementations verify requests whose query string is not in
canonical encoding — measured, because it is specified nowhere. This
records what the [botocore cross-check](../sigv4/testdata/crosscheck/generate.py)
surfaced, what probing real implementations showed, and why s3rp's behavior
is deliberately left as it is.

## The two schools

SigV4 verification reconstructs a canonical request from the wire bytes,
and the canonical query string can be built two ways:

- **Sign-as-sent** (botocore): take the query string as it appears in the
  URL, split into pairs, sort the pairs as raw strings, join. No decoding,
  no re-encoding.
- **Normalize** (aws-sdk-go-v2, which s3rp verifies with): parse the query
  (decoding `%XX` and `+`), sort keys, sort each key's values by decoded
  form, re-encode canonically (unreserved characters literal, everything
  else `%XX` upper-case).

The two agree exactly when the wire query is already canonically encoded —
which is what every mainstream client serializer produces. They disagree on
wire forms only a hand-rolled client can emit:

1. **Raw reserved characters in a value**: `?a=photos/`, `?a=a+b`, `?a=*`.
   Sign-as-sent signs them literally; normalize re-encodes them (`%2F`,
   `%20`, `%2A`).
2. **Non-canonical escapes**: `?a=%7E`. Sign-as-sent keeps `%7E`; normalize
   emits the unreserved literal `~`.
3. **Duplicate keys whose value ordering differs raw vs decoded**:
   `?a=%E3%81%82&a=~`. `~` (0x7E) sorts after `%` (0x25) as a raw string
   but before 0xE3 by decoded bytes, so the two schools order the pairs
   differently inside the canonical string.

The AWS documentation describes canonicalization for *signers* and says
nothing about which interpretation a *verifier* must apply to
non-canonical wire — so this was measured instead.

## Measurements (2026-08-15)

Method: ListBuckets `GET` requests (`prefix` is a genuine ListBuckets
parameter) with the query written raw into the request line and sent
verbatim; the signature is computed sign-as-sent, or over an explicitly
chosen canonical string for the duplicate-key matrix. `SignatureDoesNotMatch`
means the endpoint's canonicalization disagreed with the one signed;
anything else means the signature was accepted. Reproduce with
[`sigv4/testdata/crosscheck/probe.py`](../sigv4/testdata/crosscheck/probe.py)
(botocore 1.43.71 at the time of measurement).

Endpoints: AWS S3 (us-east-1), versitygw v1.7.0, Ceph RGW 19.2.0
(`quay.io/ceph/demo:latest-squid`), and s3rp itself.

| wire form, signature | AWS S3 | versitygw | Ceph RGW | s3rp |
|---|---|---|---|---|
| `?prefix=photos/` sign-as-sent | reject | reject | reject | reject |
| `?prefix=a+b` sign-as-sent | reject | reject | reject | reject |
| `?prefix=*` sign-as-sent | reject | reject | reject | reject |
| `?prefix=%7E` sign-as-sent | reject | reject | reject | reject |
| dup A: wire `%E3…`,`~` — canonical sorted encoded | accept | reject | accept | reject |
| dup B: wire `~`,`%E3…` — canonical sorted encoded | accept | reject | reject | reject |
| dup C: wire `~`,`%E3…` — canonical in wire order | accept | reject | reject | reject |
| dup D: wire `%E3…`,`~` — canonical sorted decoded | accept | reject | accept | **accept** |
| dup E: wire `%E3…`,`~` — canonical in wire order | accept | reject | accept | reject |

Readings:

- **Raw reserved characters and non-canonical escapes: unanimous
  rejection.** Every implementation, AWS included, refuses sign-as-sent
  signatures over such wire. s3rp's behavior is the ecosystem's.
- **Duplicate-key ordering: total disarray.** AWS accepts *every* ordering
  for the same wire — since an HMAC matches exactly one canonical string,
  AWS must be verifying against multiple candidate canonicalizations, which
  reads as a deliberate leniency added because implementations never
  converged here. versitygw rejects all five variants tested. RGW accepts a
  wire-order-dependent subset. s3rp accepts exactly one ordering (sorted by
  decoded value — what aws-sdk-go-v2's signer computes).

## Why s3rp stays as it is

- **No real client reaches the divergence.** Every SDK serializer emits
  canonically encoded queries, where all schools agree; no S3 operation
  uses duplicate query keys. Hitting the divergence requires a hand-rolled
  client that both builds URLs without encoding *and* signs as-sent — and
  such a client is already broken against versitygw and (partially) RGW,
  not just s3rp.
- **The failure mode is safe and observable.** A mismatch answers
  `SignatureDoesNotMatch` (403, fail-closed) and the observer records the
  cause, so an affected client — should one ever exist — is visible in the
  log, not silent.
- **Multi-candidate verification is not worth its cost.** Accepting the
  other orderings would mean implementing canonical-query construction by
  hand next to the SDK's, for wire forms with no traffic; the verifier's
  design rule is to re-sign with the SDK's own signer rather than
  reimplement canonicalization. s3rp sits between versitygw (rejects all)
  and AWS (accepts all), well inside ecosystem norms.

The [botocore cross-check corpus](../sigv4/testdata/crosscheck/) —
committed vectors replayed in `go test` and fresh ones generated nightly —
only ever emits canonical encodings, matching what real clients send. If a
real client ever surfaces on the wrong side of this table, the revisit path
is a second verification pass against the alternative ordering, applied
only when the first fails.
