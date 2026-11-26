-------------------------- MODULE PayloadVerifier --------------------------
\* The check of a request body against the SHA-256 the client signed in
\* x-amz-content-sha256 (payloadVerifier in s3gw/proxy.go). The body is what
\* arrives, not what was signed: altered on the way, cut short, or failing.
\*
\* The phases are PayloadSteps, written out from the Go table. This module
\* adds the lengths, which event a Read comes to (payloadVerifier.Read), and
\* what the consumer — the upload to the backend — was handed.
EXTENDS Naturals, PayloadTable

CONSTANTS Declared   \* the signed Content-Length

ASSUME Declared \in Nat

VARIABLES phase,      \* "reading", "verified" or "refused"
          remaining,  \* of the declared length, not read yet
          \* the body as it arrives
          left,       \* bytes still to come; net/http ends it at Content-Length
          altered,    \* a byte read so far is not the one that was signed
          \* not in the code: what the consumer saw
          delivered,  \* bytes handed out
          ended       \* a Read reported the end of the body

vars == <<phase, remaining, left, altered, delivered, ended>>

Phases == {"reading", "verified", "refused"}

ASSUME \A t, u \in PayloadSteps : t.from = u.from /\ t.event = u.event => t = u

Has(p, e) == \E t \in PayloadSteps : t.from = p /\ t.event = e
To(p, e) == (CHOOSE t \in PayloadSteps : t.from = p /\ t.event = e).to

\* payloadVerifier.advance: an event the table does not have refuses the body.
After(p, e) == IF Has(p, e) THEN To(p, e) ELSE "refused"

\* The body may be shorter than declared (the client stops), never longer:
\* the "extra" step of the table is not reached here.
Init == /\ phase = "reading"
        /\ remaining = Declared
        /\ left \in 0..Declared
        /\ altered = FALSE
        /\ delivered = 0
        /\ ended = FALSE

\* The hash of what was read is the signed one only when it is all of the
\* declared length, unaltered.
Matches(read) == ~altered' /\ read = Declared

\* One Read: n bytes arrive, possibly altered, and with them the end of the
\* body (eof), an error (broken), or neither. A refused verifier does not
\* read the body again.
Read(n, tamper, eof, broken) ==
    /\ phase # "refused"
    /\ ~(eof /\ broken)
    /\ eof => n = left
    /\ altered' = (altered \/ (tamper /\ n > 0))
    /\ left' = left - n
    /\ IF phase = "verified"
       THEN /\ phase' = IF n > 0 THEN After(phase, "extra") ELSE After(phase, "end")
            /\ ended' = (ended \/ (n = 0 /\ eof))
            /\ UNCHANGED <<remaining, delivered>>
       ELSE LET rest == remaining - n
                complete == rest <= 0 \/ eof
                event == IF complete
                         THEN IF Matches(Declared - rest) THEN "matched" ELSE "mismatched"
                         ELSE IF n > 0 THEN "more" ELSE "end"
            IN  /\ phase' = After(phase, event)
                /\ remaining' = rest
                /\ delivered' = IF phase' = "refused" THEN delivered ELSE delivered + n
                /\ ended' = (ended \/ (eof /\ phase' # "refused"))

Next == \E n \in 0..left, tamper \in BOOLEAN, eof \in BOOLEAN, broken \in BOOLEAN :
            Read(n, tamper, eof, broken)

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------

TypeOK == /\ phase \in Phases
          /\ remaining \in 0..Declared
          /\ left \in 0..Declared
          /\ altered \in BOOLEAN
          /\ delivered \in 0..Declared
          /\ ended \in BOOLEAN

\* The bytes that complete the body are handed out only when all of it is
\* what was signed: a consumer holding the declared length holds the
\* signed payload.
FullIsSigned == (Declared > 0 /\ delivered = Declared) => (phase = "verified" /\ ~altered)

\* The body is never reported to have ended unless it was verified: one cut
\* short or altered does not read as a complete one.
EndIsVerified == ended => phase = "verified"

\* What is verified is the whole declared length, unaltered.
VerifiedIsWhole == phase = "verified" => (remaining = 0 /\ ~altered)

\* A refusal stays one, whatever a later Read would come to.
RefusalIsFinal == \A t \in PayloadSteps : t.from = "refused" => t.to = "refused"

=============================================================================
