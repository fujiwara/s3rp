---------------------------- MODULE VersionStore ----------------------------
\* One key of a localbackend bucket through every change of the bucket's
\* versioning and Object Lock (s3gw/localbackend: localbackend.go, object.go,
\* lock.go): writes, deletes, deletes of a version, retention and legal
\* holds set and changed, with and without the governance bypass, as time
\* passes.
\*
\* What the versioning can become is VersioningSteps, written out from the
\* Go table (versioningTransitions). This module adds the key's versions and
\* what each request does to them under each state (bucket.put, deleteOne,
\* applyLockOnWrite, PutObjectRetention, PutObjectLegalHold).
EXTENDS Naturals, Sequences, FiniteSets, VersioningTable

CONSTANTS MaxVersions,  \* versions the key holds at once
          MaxIds,       \* version ids issued
          MaxTime       \* ticks of the clock

VARIABLES
    vstate,    \* "off", "enabled", "suspended" or "locked" (enabled, with Object Lock)
    versions,  \* the key's versions, newest first
    nextId,    \* the next version id; 0 is "null"
    now,
    \* not in the code: the request that made the last step
    act,       \* its name
    bypassed   \* whether it carried x-amz-bypass-governance-retention

vars == <<vstate, versions, nextId, now, act, bypassed>>

Modes == {"none", "governance", "compliance"}
Version == [id : 0..MaxIds, marker : BOOLEAN, mode : Modes, until : 0..MaxTime, hold : BOOLEAN]

ASSUME \A t, u \in VersioningSteps : t.from = u.from /\ t.event = u.event => t = u

\* A request the table has no step for is InvalidBucketState.
Has(s, e) == \E t \in VersioningSteps : t.from = s /\ t.event = e
To(s, e) == (CHOOSE t \in VersioningSteps : t.from = s /\ t.event = e).to

\* A bucket is created without versioning, or with Object Lock.
Init == /\ vstate \in {"off", "locked"}
        /\ versions = <<>>
        /\ nextId = 1
        /\ now = 0
        /\ act = "none"
        /\ bypassed = FALSE

Versioned == vstate \in {"enabled", "locked"}

\* bucket.put: a new version when enabled, otherwise in place of the null one.
Stack(v) == IF Versioned THEN <<v>> \o versions
            ELSE IF vstate = "suspended" THEN <<v>> \o SelectSeq(versions, LAMBDA x : x.id # 0)
            ELSE <<v>>

NewId == IF Versioned THEN nextId ELSE 0

Room == Len(versions) < MaxVersions /\ nextId <= MaxIds

\* PutBucketVersioning, PutObjectLockConfiguration.
SetVersioning(e) ==
    /\ Has(vstate, e)
    /\ vstate' = To(vstate, e)
    /\ act' = "versioning" /\ bypassed' = FALSE
    /\ UNCHANGED <<versions, nextId, now>>

\* PutObject, CopyObject, CompleteMultipartUpload. Lock settings are
\* refused on a bucket without Object Lock, and a retention must end in the
\* future (applyLockOnWrite).
Put(mode, until, hold) ==
    /\ Room
    /\ (mode # "none" \/ hold) => vstate = "locked"
    /\ IF mode = "none" THEN until = 0 ELSE until > now
    /\ versions' = Stack([id |-> NewId, marker |-> FALSE, mode |-> mode, until |-> until, hold |-> hold])
    /\ nextId' = IF Versioned THEN nextId + 1 ELSE nextId
    /\ act' = "put" /\ bypassed' = FALSE
    /\ UNCHANGED <<vstate, now>>

\* DeleteObject without a version id: a delete marker, or the key itself
\* where there never was versioning.
Delete ==
    /\ IF vstate = "off"
       THEN /\ versions' = <<>>
            /\ UNCHANGED nextId
       ELSE /\ Room
            /\ versions' = Stack([id |-> NewId, marker |-> TRUE, mode |-> "none", until |-> 0, hold |-> FALSE])
            /\ nextId' = IF Versioned THEN nextId + 1 ELSE nextId
    /\ act' = "delete" /\ bypassed' = FALSE
    /\ UNCHANGED <<vstate, now>>

\* object.locked: a legal hold always, compliance retention until it ends,
\* governance retention unless the request bypasses it.
InForce(v) == v.mode # "none" /\ v.until > now
Locked(v, bypass) == v.hold \/ (InForce(v) /\ (v.mode = "compliance" \/ ~bypass))

Without(i) == SubSeq(versions, 1, i - 1) \o SubSeq(versions, i + 1, Len(versions))

\* DeleteObject of a version.
DeleteVersion(i, bypass) ==
    /\ ~Locked(versions[i], bypass)
    /\ versions' = Without(i)
    /\ act' = "delete-version" /\ bypassed' = bypass
    /\ UNCHANGED <<vstate, nextId, now>>

\* PutObjectRetention: a retention in force may be extended in its own
\* mode; anything else compliance never allows, and governance only with
\* the bypass.
PutRetention(i, mode, until, bypass) ==
    LET v == versions[i]
        changes == mode # v.mode \/ until < v.until
    IN  /\ vstate = "locked"
        /\ ~v.marker
        /\ mode # "none" /\ until > now
        /\ ~(InForce(v) /\ changes /\ (v.mode = "compliance" \/ ~bypass))
        /\ versions' = [versions EXCEPT ![i] = [@ EXCEPT !.mode = mode, !.until = until]]
        /\ act' = "retention" /\ bypassed' = bypass
        /\ UNCHANGED <<vstate, nextId, now>>

PutLegalHold(i, hold) ==
    /\ vstate = "locked"
    /\ ~versions[i].marker
    /\ versions' = [versions EXCEPT ![i] = [@ EXCEPT !.hold = hold]]
    /\ act' = "legal-hold" /\ bypassed' = FALSE
    /\ UNCHANGED <<vstate, nextId, now>>

Tick == /\ now < MaxTime
        /\ now' = now + 1
        /\ act' = "tick" /\ bypassed' = FALSE
        /\ UNCHANGED <<vstate, versions, nextId>>

Next == \/ \E e \in {"enable", "suspend", "lock"} : SetVersioning(e)
        \/ \E mode \in Modes, until \in 0..MaxTime, hold \in BOOLEAN : Put(mode, until, hold)
        \/ Delete
        \/ \E i \in DOMAIN versions, bypass \in BOOLEAN : DeleteVersion(i, bypass)
        \/ \E i \in DOMAIN versions, mode \in Modes, until \in 0..MaxTime, bypass \in BOOLEAN :
               PutRetention(i, mode, until, bypass)
        \/ \E i \in DOMAIN versions, hold \in BOOLEAN : PutLegalHold(i, hold)
        \/ Tick

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------

TypeOK == /\ vstate \in {"off", "enabled", "suspended", "locked"}
          /\ versions \in Seq(Version)
          /\ Len(versions) <= MaxVersions
          /\ nextId \in 1..(MaxIds + 1)
          /\ now \in 0..MaxTime

Ids == {versions[i].id : i \in DOMAIN versions}

\* Version ids name one version each: a key holds at most one "null".
IdsAreUnique == Cardinality(Ids) = Len(versions)

\* A key in a bucket that never had versioning holds one version, "null".
OffHoldsOne == vstate = "off" => (Len(versions) <= 1 /\ Ids \subseteq {0})

\* Versioning is never off again, and with Object Lock stays enabled: the
\* two states in which a write can take the place of a version are out of
\* reach of a bucket that locks versions.
NoWayBack == \A t \in VersioningSteps :
    /\ t.to # "off"
    /\ t.from = "locked" => t.to = "locked"
    /\ t.to = "locked" => t.from \in {"enabled", "locked"}

\* Only a bucket with Object Lock holds a version with a retention or a hold.
LockNeedsObjectLock ==
    (\E i \in DOMAIN versions : versions[i].mode # "none" \/ versions[i].hold) => vstate = "locked"

Protected(v) == v.hold \/ (InForce(v) /\ v.mode = "compliance")

Still(v) == \E j \in DOMAIN versions' : versions'[j].id = v.id /\ versions'[j].marker = v.marker

\* No request removes a version under a legal hold or a compliance
\* retention in force — not a delete, not a write, not a change of the
\* bucket's versioning — and none shortens such a retention or changes its
\* mode.
ProtectedIsKept ==
    [][\A i \in DOMAIN versions :
          LET v == versions[i] IN
          /\ Protected(v) => Still(v)
          /\ (InForce(v) /\ v.mode = "compliance") =>
                 \E j \in DOMAIN versions' : /\ versions'[j].id = v.id
                                             /\ versions'[j].mode = "compliance"
                                             /\ versions'[j].until >= v.until
      ]_vars

\* A governance retention in force gives way only to the bypass.
GovernanceNeedsBypass ==
    [][\A i \in DOMAIN versions :
          LET v == versions[i] IN
          (InForce(v) /\ v.mode = "governance" /\ ~bypassed') =>
              \E j \in DOMAIN versions' : /\ versions'[j].id = v.id
                                          /\ versions'[j].mode = "governance"
                                          /\ versions'[j].until >= v.until
      ]_vars

\* While versioning is enabled, a write or a delete without a version id
\* removes nothing.
EnabledKeepsAll ==
    [][(Versioned /\ act' \in {"put", "delete"}) =>
          \A i \in DOMAIN versions : \E j \in DOMAIN versions' : versions'[j] = versions[i]
      ]_vars

=============================================================================
