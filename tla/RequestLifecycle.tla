-------------------------- MODULE RequestLifecycle --------------------------
\* One request through the gateway (s3gw/handler.go): the checks it passes
\* in order, the record assembled for the observer on the way, and the end
\* of it — returned, failed, panicked or aborted, before or after the
\* response began, with the client gone or not.
\*
\* What is done at the end is ReportSteps, written out from the Go table
\* (reportTransitions). The order of the checks is transcribed from
\* handleRequest, handlePostObject, dispatch and runOp: it is Go's control
\* flow, which has no table to write out.
EXTENDS Naturals, ReportTable

CONSTANTS HasObserver   \* whether SetObserver was called

ASSUME HasObserver \in BOOLEAN

VARIABLES
    kind,       \* "preflight", "post" (browser upload) or "signed"
    pc,         \* how far the request has come
    \* what is true of the request
    sigOK,      \* its signature (or POST policy signature) verified
    visible,    \* its bucket resolved for the requester
    allowed,    \* the policies passed
    admitted,   \* the Authorizer admitted it
    \* what was done for it
    hooked,     \* a hook was called with its Op
    backend,    \* a backend was called
    started,    \* the response began
    gone,       \* the client closed the connection
    stems,      \* the failure stems from that (canceled context, body read)
    \* the record for the observer
    recKey, recIdentity, recRoute, recOp,
    \* the end
    outcome,    \* "none" until the handler ends
    startedAtEnd,
    documents,  \* error documents written
    observed,   \* times the observer was called
    cut,        \* the connection was cut
    relabeled,  \* recorded as 499 ClientClosedRequest
    broke       \* the reporting itself panicked

vars == <<kind, pc, sigOK, visible, allowed, admitted, hooked, backend, started, gone, stems,
          recKey, recIdentity, recRoute, recOp,
          outcome, startedAtEnd, documents, observed, cut, relabeled, broke>>

Outcomes == {"returned", "failed", "panicked", "aborted"}
Phase == IF started THEN "started" ELSE "pending"

ASSUME \A o \in Outcomes, p \in {"pending", "started"} :
           \E t \in ReportSteps : t.outcome = o /\ t.response = p
ASSUME \A t, u \in ReportSteps : t.outcome = u.outcome /\ t.response = u.response => t = u

Step(o, p) == CHOOSE t \in ReportSteps : t.outcome = o /\ t.response = p

Init == /\ kind \in {"preflight", "post", "signed"}
        /\ pc = "received"
        /\ sigOK = FALSE /\ visible = FALSE /\ allowed = FALSE /\ admitted = FALSE
        /\ hooked = FALSE /\ backend = FALSE /\ started = FALSE
        /\ gone = FALSE /\ stems = FALSE
        /\ recKey = FALSE /\ recIdentity = FALSE /\ recRoute = FALSE /\ recOp = FALSE
        /\ outcome = "none" /\ startedAtEnd = FALSE
        /\ documents = 0 /\ observed = 0 /\ cut = FALSE /\ relabeled = FALSE /\ broke = FALSE

Running == pc \notin {"ended", "finished"}

\* The handler ends with o.
End(o) == /\ outcome' = o
          /\ pc' = "ended"

flags  == <<sigOK, visible, allowed, admitted>>
done   == <<hooked, backend, started>>
client == <<gone, stems>>
rec    == <<recKey, recIdentity, recRoute, recOp>>
end    == <<startedAtEnd, documents, observed, cut, relabeled, broke>>

\* A check refuses the request.
Refuse == /\ End("failed")
          /\ UNCHANGED <<kind, flags, done, client, rec, end>>

\* Any code on the way has a bug.
Panic == /\ Running
         /\ End("panicked")
         /\ UNCHANGED <<kind, flags, done, client, rec, end>>

\* The client leaves, whenever it likes.
Leave == /\ Running
         /\ gone' = TRUE
         /\ UNCHANGED <<kind, pc, flags, done, stems, rec, outcome, end>>

\* A CORS preflight is answered without authentication, and reaches nothing.
Preflight == /\ kind = "preflight" /\ pc = "received"
             /\ \/ Refuse
                \/ /\ started' \in BOOLEAN
                   /\ End("returned")
                   /\ UNCHANGED <<kind, flags, hooked, backend, client, rec, end>>

\* handleRequest: verifyRequest, then the identity goes on the record. A
\* failed verification records the key it claimed, if it named one.
Verify == /\ kind = "signed" /\ pc = "received"
          /\ \/ /\ recKey' \in BOOLEAN
                /\ End("failed")
                /\ UNCHANGED <<kind, flags, done, client, recIdentity, recRoute, recOp, end>>
             \/ /\ sigOK' = TRUE
                /\ recKey' = TRUE /\ recIdentity' = TRUE
                /\ pc' = "verified"
                /\ UNCHANGED <<kind, visible, allowed, admitted, done, client, recRoute, recOp, outcome, end>>

\* handleRequest: the operation is named before anything else is checked.
Route == /\ kind = "signed" /\ pc = "verified"
         /\ \/ Refuse   \* the path cannot be read
            \/ /\ recRoute' = TRUE
               /\ pc' = "routed"
               /\ UNCHANGED <<kind, flags, done, client, recKey, recIdentity, recOp, outcome, end>>

\* handlePostObject: a browser upload is named by its shape, then its policy
\* is verified, then the identity goes on the record.
PostRoute == /\ kind = "post" /\ pc = "received"
             /\ recRoute' = TRUE
             /\ pc' = "posted"
             /\ UNCHANGED <<kind, flags, done, client, recKey, recIdentity, recOp, outcome, end>>

PostVerify == /\ kind = "post" /\ pc = "posted"
              /\ \/ Refuse   \* a query parameter, the form, the key field
                 \/ /\ recKey' \in BOOLEAN
                    /\ End("failed")
                    /\ UNCHANGED <<kind, flags, done, client, recIdentity, recRoute, recOp, end>>
                 \/ /\ sigOK' = TRUE
                    /\ pc' = "post-verified"
                    /\ UNCHANGED <<kind, visible, allowed, admitted, done, client, rec, outcome, end>>

PostIdentify == /\ kind = "post" /\ pc = "post-verified"
                /\ \/ Refuse   \* an x-amz-* header nothing reads
                   \/ /\ recKey' = TRUE /\ recIdentity' = TRUE
                      /\ pc' = "routed"
                      /\ UNCHANGED <<kind, flags, done, client, recRoute, recOp, outcome, end>>

\* The bucket is resolved for the requester (resolveBucket), or there is
\* none and the request is ListBuckets, which the user policy decides.
Resolve == /\ pc = "routed"
           /\ \/ Refuse   \* an unknown header, the method, no such bucket for them
              \/ /\ visible' = TRUE
                 /\ pc' = "resolved"
                 /\ UNCHANGED <<kind, sigOK, allowed, admitted, done, client, rec, outcome, end>>
              \/ /\ kind = "signed"
                 /\ allowed' = TRUE
                 /\ recOp' = TRUE
                 /\ pc' = "authorized"
                 /\ UNCHANGED <<kind, sigOK, visible, admitted, done, client, recKey, recIdentity, recRoute, outcome, end>>

\* dispatch: the route's checks, the policies, the copy source; then the Op
\* exists and goes on the record.
Authorize == /\ pc = "resolved"
             /\ \/ Refuse
                \/ /\ allowed' = TRUE
                   /\ recOp' = TRUE
                   /\ pc' = "authorized"
                   /\ UNCHANGED <<kind, sigOK, visible, admitted, done, client, recKey, recIdentity, recRoute, outcome, end>>

\* runOp: the Authorizer, which may refuse, or abort in silence.
Hook == /\ pc = "authorized"
        /\ hooked' = TRUE
        /\ \/ /\ End("failed")
              /\ UNCHANGED <<admitted>>
           \/ /\ End("aborted")
              /\ UNCHANGED <<admitted>>
           \/ /\ admitted' = TRUE
              /\ pc' = "handling"
              /\ UNCHANGED <<outcome>>
        /\ UNCHANGED <<kind, sigOK, visible, allowed, backend, started, client, rec, end>>

\* The interceptors and the operation's handler.
Handle == /\ pc = "handling"
          /\ \/ /\ backend' = TRUE
                /\ UNCHANGED <<pc, started, stems, outcome>>
             \/ /\ started' = TRUE
                /\ UNCHANGED <<pc, backend, stems, outcome>>
             \/ /\ End("returned")
                /\ UNCHANGED <<backend, started, stems>>
             \/ \* a failure, which is the client's doing only if it left
                /\ stems' \in {FALSE, gone}
                /\ End("failed")
                /\ UNCHANGED <<backend, started>>
             \/ /\ End("aborted")
                /\ UNCHANGED <<backend, started, stems>>
          /\ UNCHANGED <<kind, flags, hooked, gone, rec, end>>

\* wrapHandler's finish: what the table says for how the handler ended.
Finish ==
    LET t == Step(outcome, Phase) IN
    /\ pc = "ended"
    /\ pc' = "finished"
    /\ startedAtEnd' = started
    /\ documents' = IF t.document THEN documents + 1 ELSE documents
    /\ started' = (started \/ t.document)
    /\ observed' = IF t.observe /\ HasObserver THEN observed + 1 ELSE observed
    /\ cut' = t.abort
    /\ relabeled' = (outcome = "failed" /\ gone /\ stems)
    /\ UNCHANGED <<kind, flags, hooked, backend, client, rec, outcome, broke>>

\* The reporting panics midway: the document may be out, the observer may
\* have been called. Of a second finish only the cut connection is left,
\* and a panic in the one that reports a panic is net/http's to recover.
FinishBreaks ==
    LET t == Step(outcome, Phase) IN
    /\ pc = "ended"
    /\ outcome \in {"returned", "failed", "panicked"}
    /\ pc' = "finished"
    /\ broke' = TRUE
    /\ startedAtEnd' = started
    /\ \E wrote \in {FALSE, t.document}, told \in {FALSE, t.observe /\ HasObserver} :
          /\ documents' = IF wrote THEN documents + 1 ELSE documents
          /\ started' = (started \/ wrote)
          /\ observed' = IF told THEN observed + 1 ELSE observed
    /\ cut' = IF outcome = "panicked" THEN TRUE ELSE Step("panicked", IF started' THEN "started" ELSE "pending").abort
    /\ relabeled' = FALSE
    /\ UNCHANGED <<kind, flags, hooked, backend, client, rec, outcome>>

Next == \/ Preflight \/ Verify \/ Route \/ PostRoute \/ PostVerify \/ PostIdentify
        \/ Resolve \/ Authorize \/ Hook \/ Handle
        \/ Panic \/ Leave \/ Finish \/ FinishBreaks

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------

Finished == pc = "finished"

\* The record names an identity only when the signature proved it, and an
\* operation only when the policies passed for that identity.
IdentityIsProven == recIdentity => sigOK
OpIsAuthorized == recOp => (recIdentity /\ allowed)
\* A route is named for a verified request, or for a browser upload, which
\* is told by its shape.
RouteIsVerified == recRoute => (sigOK \/ kind = "post")

\* Hooks see what the policies passed, and a backend what the hooks admitted.
HooksAfterPolicies == hooked => (allowed /\ recOp)
BackendAfterHooks == backend => (admitted /\ hooked /\ allowed /\ sigOK)
AllowedIsVerified == allowed => sigOK

\* A request is answered with at most one error document, and never one
\* appended to a response that had begun.
OneDocument == documents <= 1
DocumentReplacesNothing == documents = 1 => ~startedAtEnd

\* The observer is called once, for every request but one a hook aborted
\* in silence — every failure and every panic included.
ObservedOnce == observed <= 1
Observed == (Finished /\ HasObserver /\ ~broke /\ outcome # "aborted") => observed = 1

\* A failure the client can still be told is told; one found after the
\* response began is on the record alone.
FailureIsTold == (Finished /\ ~broke /\ outcome = "failed") => (documents = 1 <=> ~startedAtEnd)

\* A panic never leaves a response that reads as complete: the client gets
\* an error document, or a cut connection.
PanicIsNotSuccess == (Finished /\ outcome = "panicked") => (documents = 1 \/ cut)

\* A hook's abort is passed on untouched, the silence included.
AbortIsSilent == (Finished /\ outcome = "aborted") => (documents = 0 /\ observed = 0 /\ cut)

\* Only a failure of a request the client left is recorded as the client's.
ClientClosedIsClients == relabeled => (gone /\ outcome = "failed")

\* The connection is cut for nothing else than an abort, a panic after the
\* response began, or the reporting breaking.
CutIsDeliberate == cut => (outcome \in {"aborted", "panicked"} \/ broke)

=============================================================================
