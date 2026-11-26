----------------------------- MODULE BlobStore -----------------------------
\* The directory of a localbackend made by Open (s3gw/localbackend:
\* content.go, persist.go): blob files, the meta.json that names them, and
\* the books kept of them in memory — with writes, deletes, downloads and
\* flushes interleaved in every order, a meta.json that cannot be written,
\* and the process stopping at any step and being opened again.
\*
\* A blob's life in the books is BlobSteps, written out from the Go table
\* (blobTransitions). This module adds the files, the state in memory and
\* on disk, and the steps of a write (ingest, release) and of a flush.
EXTENDS Naturals, FiniteSets, BlobTable

CONSTANTS Blobs,       \* blob ids
          Writers,     \* requests that upload a body
          MaxReaders   \* response bodies reading one blob at a time

VARIABLES
    st,       \* the books: each blob's state (blob.state)
    file,     \* the directory: "none", "temp" (a tmp- name) or "named"
    mem,      \* the blobs the state in memory refers to
    disk,     \* the blobs meta.json names
    dirty,    \* the state changed since it was last snapshot
    readers,  \* response bodies reading each blob
    wpc, wb,  \* each writer: where it is, and the blob it holds
    fpc,      \* the flush: "idle", "write" or "remove"
    fsnap     \* the snapshot a flush is writing

vars == <<st, file, mem, disk, dirty, readers, wpc, wb, fpc, fsnap>>

States == {"new", "pending", "stored", "garbage", "removed"}

ASSUME \A t, u \in BlobSteps : t.from = u.from /\ t.event = u.event => t = u

\* blob.advance: an event that does not happen in a state leaves it.
After(s, e) == IF \E t \in BlobSteps : t.from = s /\ t.event = e
               THEN (CHOOSE t \in BlobSteps : t.from = s /\ t.event = e).to
               ELSE s

Init == /\ st = [b \in Blobs |-> "new"]
        /\ file = [b \in Blobs |-> "none"]
        /\ mem = {}
        /\ disk = {}
        /\ dirty = FALSE
        /\ readers = [b \in Blobs |-> 0]
        /\ wpc = [w \in Writers |-> "idle"]
        /\ wb = [w \in Writers |-> {}]
        /\ fpc = "idle"
        /\ fsnap = {}

\* The flush removes files under the write lock, which a request that reads
\* or changes the state waits for.
Unlocked == fpc # "remove"

Held(w) == CHOOSE b \in wb[w] : TRUE

-----------------------------------------------------------------------------
\* A write: PutObject, UploadPart.

\* ingest: the body goes to a temporary file.
WCreate(w) ==
    /\ wpc[w] = "idle"
    /\ \E b \in Blobs :
        /\ st[b] = "new" /\ file[b] = "none"
        /\ \A v \in Writers : b \notin wb[v]
        /\ file' = [file EXCEPT ![b] = "temp"]
        /\ wb' = [wb EXCEPT ![w] = {b}]
    /\ wpc' = [wpc EXCEPT ![w] = "temp"]
    /\ UNCHANGED <<st, mem, disk, dirty, readers, fpc, fsnap>>

\* ingest: reading the body failed.
WCopyFails(w) ==
    /\ wpc[w] = "temp"
    /\ file' = [file EXCEPT ![Held(w)] = "none"]
    /\ wpc' = [wpc EXCEPT ![w] = "idle"]
    /\ wb' = [wb EXCEPT ![w] = {}]
    /\ UNCHANGED <<st, mem, disk, dirty, readers, fpc, fsnap>>

\* ingest: the blob goes into the books before the file has its name.
WRegister(w) ==
    /\ wpc[w] = "temp"
    /\ st' = [st EXCEPT ![Held(w)] = After(@, "ingested")]
    /\ wpc' = [wpc EXCEPT ![w] = "registered"]
    /\ UNCHANGED <<file, mem, disk, dirty, readers, wb, fpc, fsnap>>

WRename(w) ==
    /\ wpc[w] = "registered"
    /\ file' = [file EXCEPT ![Held(w)] = "named"]
    /\ wpc' = [wpc EXCEPT ![w] = "named"]
    /\ UNCHANGED <<st, mem, disk, dirty, readers, wb, fpc, fsnap>>

WRenameFails(w) ==
    /\ wpc[w] = "registered"
    /\ st' = [st EXCEPT ![Held(w)] = After(@, "abandoned")]
    /\ file' = [file EXCEPT ![Held(w)] = "none"]
    /\ wpc' = [wpc EXCEPT ![w] = "idle"]
    /\ wb' = [wb EXCEPT ![w] = {}]
    /\ UNCHANGED <<mem, disk, dirty, readers, fpc, fsnap>>

\* Under the write lock: the state comes to refer to the blob.
WCommit(w) ==
    /\ wpc[w] = "named" /\ Unlocked
    /\ mem' = mem \cup wb[w]
    /\ dirty' = TRUE
    /\ wpc' = [wpc EXCEPT ![w] = "committed"]
    /\ UNCHANGED <<st, file, disk, readers, wb, fpc, fsnap>>

\* The write is refused instead: a digest, a precondition, a bucket gone.
WReject(w) ==
    /\ wpc[w] = "named" /\ Unlocked
    /\ dirty' \in {dirty, TRUE}
    /\ wpc' = [wpc EXCEPT ![w] = "rejected"]
    /\ UNCHANGED <<st, file, mem, disk, readers, wb, fpc, fsnap>>

\* release: the write is over.
WRelease(w) ==
    /\ \/ /\ wpc[w] = "committed"
          /\ st' = [st EXCEPT ![Held(w)] = After(@, "committed")]
          /\ UNCHANGED file
       \/ /\ wpc[w] = "rejected"
          /\ st' = [st EXCEPT ![Held(w)] = After(@, "abandoned")]
          /\ file' = [file EXCEPT ![Held(w)] = "none"]
    /\ wpc' = [wpc EXCEPT ![w] = "idle"]
    /\ wb' = [wb EXCEPT ![w] = {}]
    /\ UNCHANGED <<mem, disk, dirty, readers, fpc, fsnap>>

-----------------------------------------------------------------------------
\* The state stops referring to a blob: a delete, an overwrite, an abort.
\* (A copy, or the completion of an upload, refers again to blobs the state
\* refers to already, which changes nothing here.)
Unref(b) ==
    /\ Unlocked
    /\ b \in mem
    /\ mem' = mem \ {b}
    /\ dirty' = TRUE
    /\ UNCHANGED <<st, file, disk, readers, wpc, wb, fpc, fsnap>>

\* GetObject opens a body under the read lock, and reads it afterwards.
OpenReader(b) ==
    /\ Unlocked
    /\ b \in mem
    /\ readers[b] < MaxReaders
    /\ readers' = [readers EXCEPT ![b] = @ + 1]
    /\ UNCHANGED <<st, file, mem, disk, dirty, wpc, wb, fpc, fsnap>>

CloseReader(b) ==
    /\ readers[b] > 0
    /\ readers' = [readers EXCEPT ![b] = @ - 1]
    /\ UNCHANGED <<st, file, mem, disk, dirty, wpc, wb, fpc, fsnap>>

-----------------------------------------------------------------------------
\* flush, one at a time.

\* Under the write lock: the snapshot, and what it no longer refers to.
FlushBegin ==
    /\ fpc = "idle"
    /\ IF dirty
       THEN /\ dirty' = FALSE
            /\ fsnap' = mem
            /\ st' = [b \in Blobs |-> IF b \in mem THEN st[b] ELSE After(st[b], "dropped")]
            /\ fpc' = "write"
       ELSE /\ fpc' = "remove"
            /\ UNCHANGED <<dirty, fsnap, st>>
    /\ UNCHANGED <<file, mem, disk, readers, wpc, wb>>

\* writeMeta replaces meta.json as a whole, or fails and leaves it.
FlushWrite ==
    /\ fpc = "write"
    /\ \/ /\ disk' = fsnap
          /\ fpc' = "remove"
          /\ UNCHANGED dirty
       \/ /\ dirty' = TRUE
          /\ fpc' = "idle"
          /\ UNCHANGED disk
    /\ UNCHANGED <<st, file, mem, readers, wpc, wb, fsnap>>

\* Under the write lock: a garbage file no response reads is removed.
FlushRemove(b) ==
    /\ fpc = "remove"
    /\ st[b] = "garbage"
    /\ readers[b] = 0
    /\ file' = [file EXCEPT ![b] = "none"]
    /\ st' = [st EXCEPT ![b] = After(@, "collected")]
    /\ UNCHANGED <<mem, disk, dirty, readers, wpc, wb, fpc, fsnap>>

\* The flush ends, having removed what it could.
FlushEnd ==
    /\ fpc = "remove"
    /\ fpc' = "idle"
    /\ UNCHANGED <<st, file, mem, disk, dirty, readers, wpc, wb, fsnap>>

-----------------------------------------------------------------------------
\* The process stops here, whatever it was doing, and Open loads the
\* directory: temporary files and files meta.json does not name are
\* removed, and the state is what meta.json says.
Restored(b) == file[b] = "named" /\ b \in disk

CrashAndOpen ==
    /\ file' = [b \in Blobs |-> IF Restored(b) THEN "named" ELSE "none"]
    /\ st' = [b \in Blobs |-> IF Restored(b) THEN After("new", "restored") ELSE "new"]
    /\ mem' = disk
    /\ dirty' = FALSE
    /\ readers' = [b \in Blobs |-> 0]
    /\ wpc' = [w \in Writers |-> "idle"]
    /\ wb' = [w \in Writers |-> {}]
    /\ fpc' = "idle"
    /\ fsnap' = {}
    /\ UNCHANGED disk

Write(w) == \/ WCreate(w) \/ WCopyFails(w) \/ WRegister(w) \/ WRename(w)
            \/ WRenameFails(w) \/ WCommit(w) \/ WReject(w) \/ WRelease(w)

Use(b) == Unref(b) \/ OpenReader(b) \/ CloseReader(b)

Flush == FlushBegin \/ FlushWrite \/ FlushEnd \/ (\E b \in Blobs : FlushRemove(b))

Next == \/ \E w \in Writers : Write(w)
        \/ \E b \in Blobs : Use(b)
        \/ Flush
        \/ CrashAndOpen

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------

TypeOK == /\ st \in [Blobs -> States]
          /\ file \in [Blobs -> {"none", "temp", "named"}]
          /\ mem \subseteq Blobs
          /\ disk \subseteq Blobs
          /\ dirty \in BOOLEAN
          /\ readers \in [Blobs -> 0..MaxReaders]
          /\ wpc \in [Writers -> {"idle", "temp", "registered", "named", "committed", "rejected"}]
          /\ wb \in [Writers -> SUBSET Blobs]
          /\ fpc \in {"idle", "write", "remove"}
          /\ fsnap \subseteq Blobs

\* Whenever the process stops, the directory is one Open can load: every
\* blob meta.json names is there. A blob is on disk before meta.json names
\* it, and is removed only after a meta.json that does not.
DiskSound == \A b \in disk : file[b] = "named"

\* So is every blob a snapshot about to be written names.
SnapshotSound == fpc = "write" => \A b \in fsnap : file[b] = "named"

\* The state in memory refers only to blobs that are there, and no file is
\* removed under a response body reading it.
MemSound == \A b \in mem : file[b] = "named" /\ st[b] \in {"pending", "stored"}
ReaderSafe == \A b \in Blobs : readers[b] > 0 => file[b] = "named"

\* The books are true of the directory.
BooksSound == \A b \in Blobs :
    /\ st[b] = "pending" => file[b] \in {"temp", "named"}
    /\ st[b] \in {"stored", "garbage"} => file[b] = "named"

\* A blob taken for garbage is never referred to again — which is what
\* allows deciding it under the lock and removing it later.
GarbageIsForgotten == \A b \in Blobs :
    st[b] = "garbage" => /\ b \notin mem
                         /\ (fpc = "write" => b \notin fsnap)

\* A blob of a write in progress is never taken for garbage.
PendingIsKept == \A w \in Writers :
    wpc[w] \in {"registered", "named", "committed", "rejected"} => st[Held(w)] = "pending"

=============================================================================
