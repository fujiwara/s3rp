# 0049. localbackend can keep its state in a directory

- Status: Accepted
- Date: 2026-11-22
- Pull request: [#270](https://github.com/fujiwara/s3rp/pull/270)

## Context

The in-process backend ([0047](0047-backend-factory-and-in-process-backend.md))
held everything in memory. A local stand-in for a service is more useful
when what a developer put into it yesterday is there today, and an upload
should not have to fit in memory to be accepted.

Three properties of the backend as written made this cheap. An object's
bytes never change once stored; only metadata does. Every change of state
happens under one lock. And a write reads its body before it takes that
lock.

## Decision

`localbackend.Open(dir)` returns the same backend with files where `New`
has memory.

- **Bytes and metadata are kept apart.** Each uploaded body is one
  immutable file, `blobs/<id>`. Everything else — buckets, keys, versions,
  attributes, tags, lock state, uploads in progress — is `meta.json`,
  rewritten as a whole (temporary file, then rename) shortly after a
  change.
- **An object is ranges of blobs.** A completed multipart upload is its
  parts end to end and a copy is its source's ranges, so neither copies
  bytes. `New` uses the same model with the blobs in memory.
- **Order is the consistency argument.** A blob is on disk before a
  `meta.json` names it, and is removed only after a `meta.json` that no
  longer does, and not while a response is still reading it. Whenever the
  process stops, the directory holds a state that was true, plus at most
  some files nothing refers to, which the next `Open` removes.
- **Blob files have opaque names.** Keys are in `meta.json`.
- **The format is the package's own**: record types apart from the SDK's,
  under a version number that `Open` checks.
- It stays a stand-in: no sync to the device, no lock on the directory.

## Alternatives considered

- **Keys as file paths**, so the directory can be browsed. A filesystem
  cannot keep apart keys S3 does (`a` and `a/`, names differing only in
  case, empty segments), and versions, parts and metadata need somewhere
  to go regardless.
- **An append-only journal of operations.** Loses less on a crash, at the
  price of replay and compaction. What is lost here is a fraction of a
  second of changes to a development tool.
- **One file with everything, written at exit.** The least code, but the
  bytes would go through the metadata encoding, a crash would lose the
  whole session, and memory would still bound the data.
- **An embedded database for the metadata.** A dependency for the module
  ([0008](0008-reusable-gateway-packages.md)) to hold what is a few
  kilobytes per bucket in the use this is for.
- **Reference counts on blobs** instead of comparing against the snapshot
  being written. Every path that drops an object would have to release its
  blobs; the snapshot already walks every reference.

## Consequences

- An emulator built on the gateway can offer a data directory. The
  service's own definition store has to persist separately, and the two
  be reconciled at startup.
- Writes stream to disk and hash as they go; an upload is no longer held
  in memory by `Open`'s backend. Hashing content already stored (a copy
  that names a new checksum algorithm, a full-object checksum at
  completion) reads it under the backend's lock, which is fine for one
  developer and would not be for a server.
- `meta.json` is rewritten whole on every change, so the cost of a change
  grows with the number of objects. That bounds the sizes this is good
  for, deliberately.
- A change to the record types is a format change and needs a version
  bump; there is no migration.
- Both forms run the same tests (`eachBackend`), the integration suite
  and, through the harness (`directory:PATH`), the fidelity suite.
