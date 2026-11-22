# 0043. Every S3 operation is in the catalog or listed as unsupported

- Status: Accepted
- Date: 2026-11-22
- Pull request: [#254](https://github.com/fujiwara/s3rp/pull/254)

## Context

The operation catalog (`s3op/operations.json`) listed what the gateway
recognizes: the operations it performs and the ones it refuses by name.
Everything else in the S3 API was "any other request", answered
`501 NotImplemented` and written down nowhere, so the question "what of
S3 does the gateway not do" had no answer short of comparing the catalog
with the API reference.

Nor did anything notice S3 growing. Object annotations arrived in the SDK
with four operations and a CopyObject header, and the gateway learned of
them when aws-cli, which sends that header by default, was run against it
([0042](0042-accept-annotation-directive-exclude.md)).

## Decision

- `operations.json` gains `unsupported`: the features the gateway does
  not implement, each with the S3 operations it consists of, its kind
  and, where there is one, the reason. `s3op.Unsupported` exposes it.
- The kind says what the feature is and so what supporting it would take:
  an operation on objects, which a backend would be passed; configuration
  of a bucket, which the control plane owns and the store would answer
  for; or something outside the gateway's model.
- A note records the gateway's own reason and nothing else. That Amazon
  has closed a feature to new customers, or which backend implements it,
  is true until it is not, and no test can tell.
- `TestCatalogCoversSDK` (s3gw) requires every operation of the AWS SDK
  for Go's S3 client to be in the catalog or in that list, and everything
  in the list to be an SDK operation. An SDK update that brings an
  operation fails the test until the operation is classified.
- docs/s3-api.md renders the list ("Unsupported features"), kept in step
  by `TestCatalogDocumented`.
- An unsupported operation stays unrouted: no `Op*` constant, no name on
  `Op.Operation`. Being listed changes what is documented, not what a
  request is answered.

## Alternatives considered

- **A third `support` value on catalog entries.** Each of the 59
  operations would need a scope and get an `Op*` constant, which reads as
  "the gateway records this name" when it does not; and a reader wants the
  features, not 59 names.
- **Route each one by name, like the refused operations**, so a service
  could count attempts. That is a route and a query-parameter match per
  operation for requests no client of the gateway is known to send; it can
  be done for a single operation when someone needs the count.
- **A list in the documentation only.** Nothing would fail when S3 grows,
  which is the half of the problem that cost something.
- **The same check for request headers.** The SDK exposes operations as
  methods but header names only inside its serializers; a new header
  arrives with a new or changed operation often enough that the operation
  check is a first notice. Left for when it proves not to be.

## Consequences

- Bumping `aws-sdk-go-v2/service/s3` can now fail a test. The fix is a
  line in `operations.json` and, for a feature worth a sentence, its note.
- The list is the SDK's view of the API at the version in go.mod, not
  Amazon's documentation: an operation exists for the gateway once the SDK
  has it.
