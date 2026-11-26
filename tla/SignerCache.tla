---------------------------- MODULE SignerCache ----------------------------
\* One slot of the signing key cache (sigv4/signer.go), with requests for
\* different credentials — another access key that maps to the slot, the
\* same key after its secret was rotated, another day — arriving at once.
\* The slot is read under its read lock and written under its write lock,
\* and the key is derived between the two, with neither.
\*
\* There is no table behind this one: the slot's state is its occupant.
EXTENDS Naturals

CONSTANTS Ids,       \* what a signing key is derived from: access key id,
                     \* secret version, date, region
          Requests,  \* concurrent verifications
          None       \* an empty slot

VARIABLES
    slotId, slotKey,  \* the slot: its occupant and the occupant's key
    pc,               \* each request: "idle", "missed", "derived" or "done"
    want,             \* the id each request asks for
    got               \* the key it was handed

vars == <<slotId, slotKey, pc, want, got>>

\* The signing key of an id, which nothing but the id determines.
KeyOf(id) == <<"key", id>>

Init == /\ slotId = None /\ slotKey = None
        /\ pc = [r \in Requests |-> "idle"]
        /\ want \in [Requests -> Ids]
        /\ got = [r \in Requests |-> None]

\* Under the read lock: a hit hands out the occupant's key.
Look(r) ==
    /\ pc[r] = "idle"
    /\ IF slotId = want[r]
       THEN /\ got' = [got EXCEPT ![r] = slotKey]
            /\ pc' = [pc EXCEPT ![r] = "done"]
       ELSE /\ pc' = [pc EXCEPT ![r] = "missed"]
            /\ UNCHANGED got
    /\ UNCHANGED <<slotId, slotKey, want>>

\* Without a lock: the key is derived, or the derivation fails and nothing
\* is cached.
Derive(r) ==
    /\ pc[r] = "missed"
    /\ \/ /\ got' = [got EXCEPT ![r] = KeyOf(want[r])]
          /\ pc' = [pc EXCEPT ![r] = "derived"]
       \/ /\ pc' = [pc EXCEPT ![r] = "done"]
          /\ UNCHANGED got
    /\ UNCHANGED <<slotId, slotKey, want>>

\* Under the write lock: the slot takes the id and its key together.
Store(r) ==
    /\ pc[r] = "derived"
    /\ slotId' = want[r]
    /\ slotKey' = got[r]
    /\ pc' = [pc EXCEPT ![r] = "done"]
    /\ UNCHANGED <<want, got>>

\* The request is over and another one comes.
Again(r) ==
    /\ pc[r] = "done"
    /\ pc' = [pc EXCEPT ![r] = "idle"]
    /\ \E id \in Ids : want' = [want EXCEPT ![r] = id]
    /\ got' = [got EXCEPT ![r] = None]
    /\ UNCHANGED <<slotId, slotKey>>

Next == \E r \in Requests : Look(r) \/ Derive(r) \/ Store(r) \/ Again(r)

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------

\* The slot holds an id with that id's key, never another's.
SlotIsConsistent == slotId # None => slotKey = KeyOf(slotId)

\* A request is handed the key of the credential it asked for, or none: not
\* the key of another access key that shares the slot, and not the key of a
\* secret since rotated.
OwnKeyOnly == \A r \in Requests : got[r] \in {None, KeyOf(want[r])}

=============================================================================
