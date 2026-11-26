----------------------------- MODULE PostUpload -----------------------------
\* Browser POST uploads to one key of an unversioned bucket
\* (s3gw/postobject.go), some of them smaller than the minimum of their
\* policy's content-length-range — which is known only where the body ends.
\*
\* The gateway refuses such an upload by failing the body where it ends, so
\* the backend's write fails with it. It used to let the write finish and
\* delete the object afterwards (Compensate): two more states, in which an
\* object the policy did not permit was there, and from which the delete
\* took whatever was at the key by then.
EXTENDS Naturals

CONSTANTS Uploads,     \* concurrent uploads to the key
          Small,       \* those under their policy's minimum
          Compensate,  \* TRUE is write-then-delete, as it once was: checked
                       \* to fail (PostUpload.fails.cfg)
          None         \* no object at the key

ASSUME Small \subseteq Uploads /\ Compensate \in BOOLEAN

VARIABLES key,    \* the upload whose object is at the key, or None
          pc,     \* each upload: "sending", "stored", "done" or "refused"
          actor   \* the upload that took the last step

vars == <<key, pc, actor>>

Init == /\ key = None
        /\ pc = [u \in Uploads |-> "sending"]
        /\ actor = None

\* The body ends. The backend stores the object unless the body failed.
Finish(u) ==
    /\ pc[u] = "sending"
    /\ actor' = u
    /\ IF u \in Small /\ ~Compensate
       THEN /\ pc' = [pc EXCEPT ![u] = "refused"]
            /\ UNCHANGED key
       ELSE /\ key' = u
            /\ pc' = [pc EXCEPT ![u] = IF u \in Small THEN "stored" ELSE "done"]

\* Compensate: the stored object turned out too small, and is deleted — or
\* the delete fails, which nothing looked at.
Undo(u) ==
    /\ pc[u] = "stored"
    /\ actor' = u
    /\ pc' = [pc EXCEPT ![u] = "refused"]
    /\ key' \in {None, key}

Next == \E u \in Uploads : Finish(u) \/ Undo(u)

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------

\* An object the policy did not permit is never at the key.
NothingSmallStored == key \notin Small

\* An upload that is refused changes nothing: it does not take away what
\* another request wrote.
RefusedChangesNothing == [][actor' \in Small => key' = key]_vars

=============================================================================
