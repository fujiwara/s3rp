# 0053. A policy document is read strictly

- Status: Accepted
- Date: 2026-11-23
- Pull request: [#279](https://github.com/fujiwara/s3rp/pull/279)

## Context

`Parse` decoded a policy document with `encoding/json` into structs. That
decoder is lenient in three ways, and each let a document be accepted as
something other than what it says:

- A member no field is named for is skipped. A statement with a misspelled
  `Condition` lost its condition: an Allow restricted to an address range
  became an Allow from anywhere. `NotResource` and `NotAction`, which are
  not supported, were skipped the same way, so an Allow on `b/*` with
  `"NotResource": "b/secret/*"` granted `b/secret/*`. A principal object
  with a second key beside `S3RP` lost that key.
- A member name matches a field without regard to case, so `"effect"` was
  `Effect`. Another parser reading the same document sees no `Effect`.
- Of two members with one name the last wins: `"Effect": "Deny", "Effect":
  "Allow"` was an Allow. A parser that keeps the first reads a Deny.

A `null` was read as an absent element.

These came up while writing a schema for the document: a schema says which
elements exist, and `Parse` accepted documents no such schema could.

The rest of the gateway already refuses what it does not act on — an unknown
query parameter, an unknown `x-amz-*` header, an unknown condition operator
or key ([0002](0002-refuse-what-cannot-be-honored.md)).

## Decision

`Dialect.Parse` reads every JSON object of a policy through one function,
`members`, which returns the object's members as written and refuses:

- a name the grammar does not have for that object,
- a name that appears twice,
- a member whose value is `null`,
- anything after the document.

Names are compared exactly. The objects are the document (`Version`, `Id`,
`Statement`), a statement (`Sid`, `Effect`, `Principal`, `NotPrincipal`,
`Action`, `Resource`, `Condition`), a principal (the dialect's key alone),
a condition (its operators) and an operator (its one key). That key,
`aws:SourceIp`, is still matched without case, as on AWS — over ASCII
letters only — so an operator with two keys is the one key in two
spellings and is refused too: their values would have been merged into a
wider range.

`Id` is accepted and not read. It is a label that policies written for AWS
carry and that changes nothing a policy grants.

## Alternatives considered

- **`Decoder.DisallowUnknownFields`.** It refuses unknown members of a
  struct, not of a map, and leaves case-insensitive names and duplicates as
  they are.
- **`encoding/json/v2`.** Case-sensitive, and it refuses duplicates and can
  refuse unknown members. It is not available without an experiment flag in
  every Go release the module supports.
- **Accept and ignore unknown elements, as before, and say so.** An ignored
  element is harmless only when it restricts nothing. The ones a policy
  author reaches for — `Condition`, `NotResource` — restrict.
- **Refuse `Id` too.** It would turn away policies that load today, for an
  element with no meaning to get wrong.

## Consequences

- A policy that loaded with an unknown element, a mis-cased name, a
  repeated element or a `null` no longer loads; a store sees the error where
  it parses. Such a policy was already not doing what its text says.
- The elements of a policy are listed in one place per object, so a schema
  of the document can be held to `Parse`.
- `json.Unmarshal` into a `policy.Policy` is still lenient. It is how a
  hand-built policy is made, not how a tenant's document is read, and `Parse`
  remains the only entry that validates.
