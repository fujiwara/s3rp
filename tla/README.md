# State machines, model-checked

The parts of s3rp that are state machines, written in [TLA+](https://lamport.azurewebsites.net/tla/tla.html) with what they are meant to guarantee stated as properties. [TLC](https://github.com/tlaplus/tlaplus) checks each property over every behavior of its model — every order of the concurrent steps, every choice of an adversary, the process stopping at every step — not over the ones a test happened to try.

```console
$ tla/check.sh            # every configuration
$ tla/check.sh Breaker    # those of one module
```

`check.sh` fetches the pinned TLC release once and holds it to its checksum; it needs Java 11 or later (`mise.toml`). The `tla` workflow runs it on every change to this directory or to the code the specifications are of.

## The models

| Specification | Of | Guarantees (each a checked property) |
|---|---|---|
| [`Breaker.tla`](Breaker.tla) | The circuit breaker, `s3gw/breaker.go` | N failures in a row open it (`OpensAfterN`); while not closed it admits one probe per cooldown and nothing else (`OneProbePerCooldown`); it cannot wedge — a probe always comes, whatever became of the last (`ProbeComes`); a failed probe opens it again (`FailedProbeReopens`); a backend that stays up is let back in (`Recovers`) |
| [`ChunkedReader.tla`](ChunkedReader.tla) | The aws-chunked decoder, `sigv4/chunked.go`, against a stream an attacker writes | A consumer holding the declared length holds only verified chunks, verified to the terminal chunk and the trailers (`FullIsVerified`, `DoneIsAuthentic`); nothing of a chunk whose signature does not verify outlives the refusal (`NothingForged`); a truncated stream is never a complete shorter one; a refusal is final |
| [`PayloadVerifier.tla`](PayloadVerifier.tla) | The check of a body against the signed `x-amz-content-sha256`, `s3gw/proxy.go` | The bytes that complete a body are handed out only when all of it is what was signed (`FullIsSigned`); a body cut short or altered never ends cleanly (`EndIsVerified`); a refusal is final |
| [`RequestLifecycle.tla`](RequestLifecycle.tla) | One request through `s3gw/handler.go`: the order of its checks, the record for the observer, and how it ends | The record names an identity only when the signature proved it and an `Op` only when the policies passed; hooks see only what the policies passed and a backend only what the hooks admitted; at most one error document, never appended to a response that began; the observer is called once for everything but a hook's silent abort; a panic never leaves a response that reads as complete |
| [`PostUpload.tla`](PostUpload.tla) | A browser upload under its policy's minimum length, `s3gw/postobject.go` | An object the policy did not permit is never at the key, and a refused upload changes nothing |
| [`SignerCache.tla`](SignerCache.tla) | One slot of the signing key cache, `sigv4/signer.go`, under concurrent requests | A request is handed the key of the credential it asked for or none — not that of another access key sharing the slot, nor of a secret since rotated |
| [`ClientCache.tla`](ClientCache.tla) | The backend client cache, `s3gw/gateway.go`, under concurrent misses and eviction | A request goes on with the client that is in the cache, and a cached client is never replaced by one built beside it: one connection pool and one breaker per backend |
| [`BlobStore.tla`](BlobStore.tla) | The directory of a `localbackend` made by `Open` — blob files, `meta.json`, the books in memory — with writes, deletes, downloads and flushes in every order, failed writes of `meta.json`, and the process stopping at every step | Whenever the process stops, every blob `meta.json` names is there (`DiskSound`); the state in memory refers only to blobs that are there; no file is removed under a response reading it; a blob taken for garbage is never referred to again |
| [`Flusher.tla`](Flusher.tla) | How a change reaches the directory: `mutate` and the flusher goroutine, `s3gw/localbackend/persist.go` | No change is left unwritten for want of a wake-up (`Written`) |
| [`MultipartUpload.tla`](MultipartUpload.tla) | Multipart uploads in `localbackend`, with parts arriving while the upload is completed or aborted | An upload that ended takes no part and makes no second object; an aborted one makes none; aborting an ended upload succeeds, an id that never was does not |
| [`VersionStore.tla`](VersionStore.tla) | One key of a `localbackend` bucket through every change of versioning and Object Lock | No request removes a version under a legal hold or a compliance retention in force, shortens such a retention or changes its mode (`ProtectedIsKept`); governance gives way only to the bypass; a bucket with Object Lock never leaves versioning; a key holds one `null` version at most |

The constants each model is checked with are in its `.cfg`. They are small — two or three of everything — because the state machines are: a property that fails does so with a couple of requests and a couple of steps, and TLC tries all of them.

## How the models are held to the code

A model that is only a description can drift from what it describes. The state machines are therefore **transition tables in the Go code** — `breakerTransitions`, `chunkTransitions`, `payloadTransitions`, `reportTransitions`, `blobTransitions`, `uploadTransitions`, `versioningTransitions` — and the code does what its table says and nothing else. A test in each package writes the table out as a TLA+ module (`BreakerTable.tla`, ...), and the specification `EXTENDS` it: what TLC checks is the table the code runs on.

```console
$ S3RP_TLA_UPDATE=1 go test ./s3gw ./sigv4 ./s3gw/localbackend -run 'Table$'
$ tla/check.sh
```

Without `S3RP_TLA_UPDATE` the same tests fail when a `*Table.tla` in the tree is not what the Go table renders to, so a table cannot change without the module changing, and the workflow then checks the properties against it.

What a table does not hold is written by hand in the specification, next to the name of the Go function it transcribes: which event a call is (how `Allow` reads the clock, how `Read` classifies what arrived), the counters, and the steps of concurrent requests. `RequestLifecycle.tla` has the order of the checks from `handleRequest` and `dispatch`, which is Go's control flow and has no table. `SignerCache.tla`, `ClientCache.tla`, `Flusher.tla` and `PostUpload.tla` have no table at all. That part is held to the code by reading, as a comment is — the tests of the same behavior (`TestOperationsEnforceIsolation` for the order of the checks, the fuzz targets for the decoder) are what check it mechanically.

`<Module>.fails.cfg` is a configuration of something known to be wrong, which TLC must find violated; `check.sh` fails when it does not. Each is a defect these models found, kept as the evidence that the property beside it can be broken:

- `ChunkedReader.fails.cfg` — the decoder handing out the bytes that arrived together with a read error. They can be a chunk's last: a consumer that has the declared length by them (`io.ReadFull`) never looks at the error and holds a chunk nobody verified. A failed `Read` now hands out nothing (`TestChunkedReaderReadErrorDeliversNothing`).
- `PostUpload.fails.cfg` — an undersized upload stored and then deleted. The object was there in between, stayed when the delete failed, and the delete took whatever another request had written to the key by then. The body now fails where it ends, so the backend's write fails with it.

## What is not modeled

- The HTTP server's own life (`Serve`, `Shutdown`): it is net/http's state machine, and s3rp only starts and stops it.
- `sync.Once` uses (`Policy.compileOnce`, `Backend.closeOnce`): one transition, which the standard library guarantees.
- Policy evaluation and SigV4 canonicalization: functions of their input, not state machines. The policy semantics are stated and proved in [`policy/spec`](../policy/spec).
- A real backend. `BlobStore`, `MultipartUpload` and `VersionStore` are of `localbackend`; what Ceph RGW or Amazon S3 do is measured by the fidelity and s3-tests suites.

Two things the models show and the code leaves as they are:

- A breaker is closed by any attempt the backend answers, including one admitted before it opened that returns during the cooldown (`SuccessCloses`). The backend did answer; the cooldown is cut short.
- A blob whose write commits and whose object is deleted again before the write returns stays in the directory until the next change is flushed or the directory is opened again. Nothing refers to it and nothing is lost; `BlobStore.tla` checks soundness, not that every file is collected at once.

## Changing a state machine

Change the Go table, regenerate its module, and run `tla/check.sh`. A property that fails prints the shortest behavior that breaks it. When the change is to what a specification writes by hand — a new step of a write, another check in the request path — change the specification in the same commit. A new guarantee is a new property; do not weaken one to make a change pass.
