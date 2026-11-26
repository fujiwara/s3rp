------------------------------- MODULE Flusher -------------------------------
\* How a change to a localbackend made by Open reaches the directory
\* (mutate and flusher in s3gw/localbackend/persist.go): a request marks the
\* state dirty under the lock and then wakes the flusher through a channel
\* that holds one wake-up and drops the rest; the flusher waits for one,
\* waits out the delay changes are gathered over, and flushes.
\*
\* What is checked is that no change is left unwritten for want of a
\* wake-up. A flush that cannot write leaves the state dirty until the next
\* change, Sync or Close (BlobStore.tla has that, and what it means for the
\* directory); here every flush writes.
EXTENDS Naturals

CONSTANTS Mutators   \* requests changing the state at once

VARIABLES
    dirty,  \* a change is not written yet
    wake,   \* wake-ups in the channel: 0 or 1
    mpc,    \* each request: "idle" or "marked" (dirty set, flusher not woken yet)
    fpc     \* the flusher: "waiting", "delaying" or "flushing"

vars == <<dirty, wake, mpc, fpc>>

Init == /\ dirty = FALSE
        /\ wake = 0
        /\ mpc = [m \in Mutators |-> "idle"]
        /\ fpc = "waiting"

\* mutate: the change is made and marked under the lock.
Mark(m) == /\ mpc[m] = "idle"
           /\ dirty' = TRUE
           /\ mpc' = [mpc EXCEPT ![m] = "marked"]
           /\ UNCHANGED <<wake, fpc>>

\* mutate: after the lock, a wake-up is sent unless one is waiting already.
Wake(m) == /\ mpc[m] = "marked"
           /\ wake' = 1
           /\ mpc' = [mpc EXCEPT ![m] = "idle"]
           /\ UNCHANGED <<dirty, fpc>>

Woken == /\ fpc = "waiting" /\ wake = 1
         /\ wake' = 0
         /\ fpc' = "delaying"
         /\ UNCHANGED <<dirty, mpc>>

Delayed == /\ fpc = "delaying"
           /\ fpc' = "flushing"
           /\ UNCHANGED <<dirty, wake, mpc>>

\* flush: the snapshot is taken, and dirty cleared, under the lock.
Flush == /\ fpc = "flushing"
         /\ dirty' = FALSE
         /\ fpc' = "waiting"
         /\ UNCHANGED <<wake, mpc>>

Next == Woken \/ Delayed \/ Flush \/ \E m \in Mutators : Mark(m) \/ Wake(m)

\* The flusher runs, and a request that changed the state gets to send its
\* wake-up.
Fairness == /\ WF_vars(Woken) /\ WF_vars(Delayed) /\ WF_vars(Flush)
            /\ \A m \in Mutators : WF_vars(Wake(m))

Spec == Init /\ [][Next]_vars /\ Fairness

-----------------------------------------------------------------------------

TypeOK == /\ dirty \in BOOLEAN
          /\ wake \in 0..1
          /\ mpc \in [Mutators -> {"idle", "marked"}]
          /\ fpc \in {"waiting", "delaying", "flushing"}

\* An unwritten change always has a flush coming: the flusher is on its
\* way, a wake-up is in the channel, or the request that made the change
\* is about to send one.
FlushIsComing ==
    dirty => (fpc # "waiting" \/ wake = 1 \/ \E m \in Mutators : mpc[m] = "marked")

\* Every change is written.
Written == dirty ~> ~dirty

=============================================================================
