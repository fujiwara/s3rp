---------------------------- MODULE ClientCache ----------------------------
\* The cache of backend clients (Gateway.backendClient in s3gw/gateway.go),
\* for one backend: requests that miss it build a client each, at once, and
\* the cache evicts as it likes.
\*
\* A client carries what is kept per backend — its connection pool, its
\* circuit breaker — so two requests must not go on with two clients where
\* one was in the cache. There is no table behind this one: the state is
\* the cache's entry.
EXTENDS Naturals

CONSTANTS Requests,    \* concurrent requests for the backend
          MaxClients,  \* clients built in all
          None

VARIABLES
    cached,  \* the client in the cache, or None
    pc,      \* each request: "idle", "building", "built" or "done"
    mine,    \* the client it built
    got,     \* the client it goes on with
    built    \* clients built so far

vars == <<cached, pc, mine, got, built>>

Init == /\ cached = None
        /\ pc = [r \in Requests |-> "idle"]
        /\ mine = [r \in Requests |-> None]
        /\ got = [r \in Requests |-> None]
        /\ built = 0

\* clients.Get
Look(r) ==
    /\ pc[r] = "idle"
    /\ IF cached # None
       THEN /\ got' = [got EXCEPT ![r] = cached]
            /\ pc' = [pc EXCEPT ![r] = "done"]
       ELSE /\ pc' = [pc EXCEPT ![r] = "building"]
            /\ UNCHANGED got
    /\ UNCHANGED <<cached, mine, built>>

\* newBackendClient, outside any lock; it may fail.
Build(r) ==
    /\ pc[r] = "building"
    /\ \/ /\ built < MaxClients
          /\ built' = built + 1
          /\ mine' = [mine EXCEPT ![r] = built + 1]
          /\ pc' = [pc EXCEPT ![r] = "built"]
       \/ /\ pc' = [pc EXCEPT ![r] = "done"]
          /\ UNCHANGED <<mine, built>>
    /\ UNCHANGED <<cached, got>>

\* clients.PeekOrAdd: the client another request added in the meantime is
\* the one to use, and the one just built is dropped.
Add(r) ==
    /\ pc[r] = "built"
    /\ IF cached # None
       THEN /\ got' = [got EXCEPT ![r] = cached]
            /\ UNCHANGED cached
       ELSE /\ cached' = mine[r]
            /\ got' = [got EXCEPT ![r] = mine[r]]
    /\ pc' = [pc EXCEPT ![r] = "done"]
    /\ UNCHANGED <<mine, built>>

Evict == /\ cached # None
         /\ cached' = None
         /\ UNCHANGED <<pc, mine, got, built>>

Again(r) ==
    /\ pc[r] = "done"
    /\ pc' = [pc EXCEPT ![r] = "idle"]
    /\ mine' = [mine EXCEPT ![r] = None]
    /\ got' = [got EXCEPT ![r] = None]
    /\ UNCHANGED <<cached, built>>

Next == Evict \/ \E r \in Requests : Look(r) \/ Build(r) \/ Add(r) \/ Again(r)

Spec == Init /\ [][Next]_vars

-----------------------------------------------------------------------------

\* A request goes on with the client that is in the cache as it does: never
\* with one it built beside a cached one.
OneClientPerBackend ==
    [][\A r \in Requests : (got'[r] # got[r] /\ got'[r] # None) => cached' = got'[r]]_vars

\* A cached client is evicted, never replaced: the one a request built
\* beside it is dropped.
CachedIsNotReplaced == [][cached # None => cached' \in {cached, None}]_vars

=============================================================================
