-------------------------- MODULE MultipartUpload --------------------------
\* Multipart uploads to one key of a localbackend bucket
\* (s3gw/localbackend/multipart.go), with requests in every order: parts
\* uploaded while the upload is completed or aborted under them, the same
\* request twice, an id that was never issued.
\*
\* An upload's life is UploadSteps, written out from the Go table
\* (uploadTransitions). This module adds the parts, the two steps of
\* UploadPart — the body is read outside the lock, the part stored under it —
\* and what each request is answered.
EXTENDS Naturals, FiniteSets, UploadTable

CONSTANTS Uploads,   \* upload ids
          Parts,     \* part numbers
          Requests   \* UploadPart requests in flight at once

VARIABLES
    ust,      \* each upload's state: "unknown", "open" or "finished"
    parts,    \* the part numbers each open upload holds
    reading,  \* each UploadPart request: {} or the <<upload, part>> whose
              \* body it is reading
    \* not in the code: what the properties are stated with
    objects,  \* the uploads that made an object, and of which parts
    ended,    \* how each upload ended: "no", "completed" or "aborted"
    answer    \* the last request and what it was answered

vars == <<ust, parts, reading, objects, ended, answer>>

ASSUME \A t, u \in UploadSteps : t.from = u.from /\ t.event = u.event => t = u

\* bucket.findUpload: a request the table has no step for is NoSuchUpload.
Has(s, e) == \E t \in UploadSteps : t.from = s /\ t.event = e
To(s, e) == (CHOOSE t \in UploadSteps : t.from = s /\ t.event = e).to

Init == /\ ust = [u \in Uploads |-> "unknown"]
        /\ parts = [u \in Uploads |-> {}]
        /\ reading = [r \in Requests |-> {}]
        /\ objects = {}
        /\ ended = [u \in Uploads |-> "no"]
        /\ answer = <<"none", "none", "ok">>

Answer(request, u, ok) == answer' = <<request, u, IF ok THEN "ok" ELSE "NoSuchUpload">>

\* CreateMultipartUpload issues an id that was never used.
Create(u) ==
    /\ Has(ust[u], "created")
    /\ ust' = [ust EXCEPT ![u] = To(@, "created")]
    /\ Answer("create", u, TRUE)
    /\ UNCHANGED <<parts, reading, objects, ended>>

\* UploadPart looks the upload up, then reads the body without the lock.
PartBegin(r, u, p) ==
    /\ reading[r] = {}
    /\ IF Has(ust[u], "used")
       THEN /\ reading' = [reading EXCEPT ![r] = {<<u, p>>}]
            /\ UNCHANGED answer
       ELSE /\ Answer("part", u, FALSE)
            /\ UNCHANGED reading
    /\ UNCHANGED <<ust, parts, objects, ended>>

\* The body is read: under the lock again, the upload may have ended.
PartEnd(r) ==
    /\ reading[r] # {}
    /\ LET up == CHOOSE x \in reading[r] : TRUE
           u == up[1]
           p == up[2]
       IN  IF Has(ust[u], "used")
           THEN /\ parts' = [parts EXCEPT ![u] = @ \cup {p}]
                /\ ust' = [ust EXCEPT ![u] = To(@, "used")]
                /\ Answer("part", u, TRUE)
           ELSE /\ Answer("part", u, FALSE)
                /\ UNCHANGED <<ust, parts>>
    /\ reading' = [reading EXCEPT ![r] = {}]
    /\ UNCHANGED <<objects, ended>>

\* The body cannot be read: nothing is stored and nothing is looked up again.
PartFails(r) ==
    /\ reading[r] # {}
    /\ reading' = [reading EXCEPT ![r] = {}]
    /\ UNCHANGED <<ust, parts, objects, ended, answer>>

\* CompleteMultipartUpload names parts; with one missing it changes nothing.
Complete(u, named) ==
    /\ named # {}
    /\ IF Has(ust[u], "completed")
       THEN IF named \subseteq parts[u]
            THEN /\ ust' = [ust EXCEPT ![u] = To(@, "completed")]
                 /\ objects' = objects \cup {<<u, named>>}
                 /\ ended' = [ended EXCEPT ![u] = "completed"]
                 /\ Answer("complete", u, TRUE)
            ELSE /\ answer' = <<"complete", u, "InvalidPart">>
                 /\ UNCHANGED <<ust, objects, ended>>
       ELSE /\ Answer("complete", u, FALSE)
            /\ UNCHANGED <<ust, objects, ended>>
    /\ UNCHANGED <<parts, reading>>

Abort(u) ==
    /\ IF Has(ust[u], "aborted")
       THEN /\ ended' = [ended EXCEPT ![u] = IF ust[u] = "open" THEN "aborted" ELSE @]
            /\ ust' = [ust EXCEPT ![u] = To(@, "aborted")]
            /\ Answer("abort", u, TRUE)
       ELSE /\ Answer("abort", u, FALSE)
            /\ UNCHANGED <<ust, ended>>
    /\ UNCHANGED <<parts, reading, objects>>

Next == \/ \E u \in Uploads : Create(u) \/ Abort(u)
        \/ \E u \in Uploads, named \in SUBSET Parts : Complete(u, named)
        \/ \E r \in Requests, u \in Uploads, p \in Parts : PartBegin(r, u, p)
        \/ \E r \in Requests : PartEnd(r) \/ PartFails(r)

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------

TypeOK == /\ ust \in [Uploads -> {"unknown", "open", "finished"}]
          /\ parts \in [Uploads -> SUBSET Parts]
          /\ reading \in [Requests -> SUBSET (Uploads \X Parts)]
          /\ objects \subseteq (Uploads \X SUBSET Parts)
          /\ ended \in [Uploads -> {"no", "completed", "aborted"}]

\* The books and the history agree.
StateIsHistory == \A u \in Uploads :
    /\ ust[u] = "finished" <=> ended[u] # "no"
    /\ ust[u] = "unknown" => parts[u] = {}

\* An upload makes at most one object, and an aborted one none.
OneObject == \A u \in Uploads :
    /\ Cardinality({o \in objects : o[1] = u}) <= 1
    /\ (\E o \in objects : o[1] = u) <=> ended[u] = "completed"

\* An object is made of parts its upload held when it was completed.
ObjectOfItsParts == \A o \in objects : o[2] # {} /\ o[2] \subseteq parts[o[1]]

\* An upload that ended takes no part, whenever the part's body arrives —
\* and none it held is replaced.
EndedIsClosed == [][\A u \in Uploads : ust[u] = "finished" => parts'[u] = parts[u]]_vars

\* Nothing comes back: an upload that ended stays ended.
EndedStays == [][\A u \in Uploads : ust[u] = "finished" => ust'[u] = "finished"]_vars

\* Aborting an upload that ended succeeds; aborting an id that never was
\* does not, and every other request on anything but an open upload is
\* NoSuchUpload.
Answers ==
    [][/\ answer'[1] = "abort" => (answer'[3] = "ok" <=> ust[answer'[2]] # "unknown")
       /\ answer'[1] \in {"part", "complete"} /\ answer'[3] = "ok" => ust[answer'[2]] = "open"
      ]_answer

=============================================================================
