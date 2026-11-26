----------------------------- MODULE ChunkTable -----------------------------
\* Code generated from the Go transition table. DO NOT EDIT.
\* Regenerate: S3RP_TLA_UPDATE=1 go test ./sigv4 -run TestChunkTable

ChunkSteps == {
    [from |-> "start", event |-> "opened", to |-> "data"],
    [from |-> "start", event |-> "terminated", to |-> "done"],
    [from |-> "start", event |-> "refused", to |-> "failed"],
    [from |-> "data", event |-> "read", to |-> "data"],
    [from |-> "data", event |-> "verified", to |-> "between"],
    [from |-> "data", event |-> "refused", to |-> "failed"],
    [from |-> "between", event |-> "opened", to |-> "data"],
    [from |-> "between", event |-> "terminated", to |-> "done"],
    [from |-> "between", event |-> "refused", to |-> "failed"]
}

=============================================================================
