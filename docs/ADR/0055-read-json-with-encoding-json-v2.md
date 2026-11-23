# 0055. JSON is read and written with encoding/json/v2

- Status: Accepted
- Date: 2026-11-23
- Pull request: [#282](https://github.com/fujiwara/s3rp/pull/282)

## Context

`encoding/json` reads leniently: it skips a member no field is named for,
matches a member name without regard to case, lets the last of two members
of one name win and replaces invalid UTF-8. For a policy document each of
these made a document grant more than it said, and
[0053](0053-read-policy-documents-strictly.md) answered with a reader of the
package's own (`members`), written on the token stream because the standard
decoder could not be told to refuse any of it.

`encoding/json/v2`, in the standard library since Go 1.27, refuses a repeated
name and invalid UTF-8 and matches names exactly by default, and refuses an
unknown member when asked (`RejectUnknownMembers`).

## Decision

The module imports `encoding/json/v2` (and `encoding/json/jsontext`)
everywhere, tests included, and no longer `encoding/json`. Go 1.27 is the
oldest release it builds with.

- **Policy documents** are decoded into structs by `unmarshal`
  (policy/dialect.go): v2's `Unmarshal` with `RejectUnknownMembers`, after a
  pass over the tokens that refuses a `null`, which v2 reads as the member
  being absent. `members` is gone. What 0053 decided is unchanged; the
  errors now name the place in the document by JSON Pointer
  (`unknown element /Statement/0/Conditon`).
- **POST policy documents** (sigv4/postpolicy.go) are read the same way: a
  member other than `expiration` and `conditions`, either in another case,
  a member given twice and a field given twice in one condition object are
  `InvalidPolicyDocument`. They were ignored, matched and overwritten.
- **The operation catalog** is read with `RejectUnknownMembers`, as it was
  with `DisallowUnknownFields`.
- **Written JSON** keeps its shape. `omitempty` on a number, a boolean or a
  pointer became `omitzero`, since v2's `omitempty` goes by the encoded
  value and would keep a `0` and drop a pointer to `""`. Maps are written
  with `Deterministic`, v2 not sorting keys otherwise. `RequestInfo`
  writes its duration as nanoseconds itself, v2 having no representation of
  a `time.Duration`. Two things differ: `<`, `>` and `&` are no longer
  escaped, and a nil slice without `omitempty` is `[]`, not `null`.
  localbackend's `meta.json` is byte for byte what it was, so
  `formatVersion` stays.

## Alternatives considered

- **Keep `members` and `encoding/json`.** It worked, but strictness then
  depends on every object being read through one helper, and the next JSON
  input (the POST policy was one) starts lenient again.
- **v2 only in `policy`.** Two JSON packages with different defaults in one
  module, and which one a file imports decides whether a duplicate name is
  an error.
- **Refuse `null` with an unmarshaler per type.** Every type in the
  document would need one, strings included; one pass over the tokens
  covers the whole document.

## Consequences

- Go 1.26 can no longer build the module; CI tests 1.27 only.
- A POST policy with a member the gateway never read is now refused. The
  SDKs' presigned-POST helpers write `expiration` and `conditions` only.
- Error texts of a refused policy document changed.
- v2 has no stable `format` tag option yet, which is why the duration is
  written by hand; that can go when it has one.
