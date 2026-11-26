------------------------------ MODULE Breaker ------------------------------
\* The circuit breaker of s3gw/breaker.go (ConsecutiveFailures), under any
\* number of concurrent attempts against a backend whose health changes at
\* will, with attempts abandoned unreported.
\*
\* What each event does in each state is BreakerSteps, written out from the
\* Go table. This module adds what the table does not hold: the clock, the
\* failure count, which event a call is (Allow and Report in breaker.go),
\* and the attempts in flight.
EXTENDS Naturals, BreakerTable

CONSTANTS N,           \* consecutive failures that open the breaker
          Cooldown,    \* in ticks of the clock
          MaxInflight  \* attempts admitted and not yet reported

ASSUME N \in Nat \ {0} /\ Cooldown \in Nat \ {0} /\ MaxInflight \in Nat \ {0}

VARIABLES state,     \* "closed", "open" or "half-open"
          failures,  \* consecutive failures counted while closed
          age,       \* ticks since mark was stamped, counted up to Cooldown
          inflight,  \* attempts admitted, their outcome not reported yet
          healthy,   \* whether the backend answers now
          \* not in the code: what the properties are stated with
          sinceOpened,  \* ticks since it last opened, up to Cooldown
          sinceGated,   \* ticks since an attempt was last admitted while
                        \* not closed, up to Cooldown
          early,        \* an attempt was admitted while not closed sooner
                        \* than Cooldown after either
          streak,       \* failures reported since the last success, up to N
          last          \* what the last step was

vars == <<state, failures, age, inflight, healthy, sinceOpened, sinceGated, early, streak, last>>

States == {"closed", "open", "half-open"}
Events == {"allow-early", "allow-due", "success", "failure", "failure-tripping"}

\* The table is total, and a function of state and event.
ASSUME \A s \in States, e \in Events :
           \E t \in BreakerSteps : t.from = s /\ t.event = e
ASSUME \A t, u \in BreakerSteps : t.from = u.from /\ t.event = u.event => t = u

Step(s, e) == CHOOSE t \in BreakerSteps : t.from = s /\ t.event = e

Saturated(n) == IF n < Cooldown THEN n + 1 ELSE Cooldown

Init == /\ state = "closed"
        /\ failures = 0
        /\ age = Cooldown
        /\ inflight = 0
        /\ healthy \in BOOLEAN
        /\ sinceOpened = Cooldown
        /\ sinceGated = Cooldown
        /\ early = FALSE
        /\ streak = 0
        /\ last = "none"

\* ConsecutiveFailures.step: carry out what the table gives.
Take(e) ==
    LET t == Step(state, e) IN
    /\ state' = t.to
    /\ failures' = IF t.count THEN failures + 1
                   ELSE IF t.to # state \/ e = "success" THEN 0
                   ELSE failures
    /\ age' = IF t.stamp THEN 0 ELSE age
    /\ sinceOpened' = IF t.to = "open" /\ state # "open" THEN 0 ELSE sinceOpened

\* Allow: the cooldown is measured from mark, and not at all while closed.
Allow ==
    LET e == IF state # "closed" /\ age >= Cooldown THEN "allow-due" ELSE "allow-early"
        t == Step(state, e)
    IN  /\ t.admit => inflight < MaxInflight
        /\ Take(e)
        /\ inflight' = IF t.admit THEN inflight + 1 ELSE inflight
        /\ IF t.admit /\ state # "closed"
           THEN /\ early' = (early \/ sinceOpened < Cooldown \/ sinceGated < Cooldown)
                /\ sinceGated' = 0
           ELSE UNCHANGED <<early, sinceGated>>
        /\ last' = IF t.admit THEN "admitted" ELSE "refused"
        /\ UNCHANGED <<healthy, streak>>

\* Report: an attempt in flight ends, with what the backend does now.
Report ==
    LET e == IF healthy THEN "success"
             ELSE IF failures + 1 >= N THEN "failure-tripping"
             ELSE "failure"
    IN  /\ inflight > 0
        /\ inflight' = inflight - 1
        /\ Take(e)
        /\ streak' = IF healthy THEN 0 ELSE IF streak < N THEN streak + 1 ELSE N
        /\ last' = IF healthy THEN "success" ELSE "failure"
        /\ UNCHANGED <<healthy, sinceGated, early>>

\* The client abandoned an attempt: the breaker is never told.
Abandon == /\ inflight > 0
           /\ inflight' = inflight - 1
           /\ last' = "abandoned"
           /\ UNCHANGED <<state, failures, age, healthy, sinceOpened, sinceGated, early, streak>>

Tick == /\ age' = Saturated(age)
        /\ sinceOpened' = Saturated(sinceOpened)
        /\ sinceGated' = Saturated(sinceGated)
        /\ last' = "tick"
        /\ UNCHANGED <<state, failures, inflight, healthy, early, streak>>

Flip == /\ healthy' = ~healthy
        /\ last' = "flip"
        /\ UNCHANGED <<state, failures, age, inflight, sinceOpened, sinceGated, early, streak>>

Next == Allow \/ Report \/ Abandon \/ Tick \/ Flip

\* Attempts keep coming, time passes, and a probe is not abandoned for ever.
Fairness == WF_vars(Allow) /\ WF_vars(Tick) /\ SF_vars(Report)

Spec == Init /\ [][Next]_vars /\ Fairness

-----------------------------------------------------------------------------

TypeOK == /\ state \in States
          /\ failures \in 0..N
          /\ age \in 0..Cooldown
          /\ inflight \in 0..MaxInflight
          /\ healthy \in BOOLEAN
          /\ sinceOpened \in 0..Cooldown
          /\ sinceGated \in 0..Cooldown
          /\ early \in BOOLEAN
          /\ streak \in 0..N

\* Failures are counted while closed only, and never reach N there.
FailuresCounted == /\ failures < N
                   /\ state # "closed" => failures = 0

\* N failures in a row open it: while it is closed, fewer were reported
\* since the last success.
OpensAfterN == state = "closed" => streak < N

\* While not closed, nothing is admitted inside a cooldown of its opening,
\* and no two attempts inside one cooldown of each other: the probe.
OneProbePerCooldown == ~early

\* A closed breaker refuses nothing.
ClosedAdmits == \A e \in {"allow-early", "allow-due"} : Step("closed", e).admit

\* It cannot wedge: open or half-open, the next cooldown brings a probe,
\* whatever became of the one before.
ProbeComes == \A s \in {"open", "half-open"} : Step(s, "allow-due").admit

\* An attempt the backend answered closes it, whenever it was admitted, and
\* nothing else does.
SuccessCloses == [][last' = "success" => state' = "closed"]_vars
OnlySuccessCloses == [][state # "closed" /\ state' = "closed" => last' = "success"]_vars

\* A failure of an attempt admitted before it opened is not news: it does
\* not put the probe off.
StaleFailureIsNoNews == [][state = "open" /\ last' = "failure" => state' = "open" /\ age' = age]_vars

\* A probe that fails opens it again, for another cooldown.
FailedProbeReopens == [][state = "half-open" /\ last' = "failure" => state' = "open" /\ age' = 0]_vars

\* A backend that stays up is not kept shut out.
Recovers == <>[]healthy => <>[](state = "closed")

=============================================================================
