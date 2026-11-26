------------------------------ MODULE BlobTable ------------------------------
\* Code generated from the Go transition table. DO NOT EDIT.
\* Regenerate: S3RP_TLA_UPDATE=1 go test ./s3gw/localbackend -run TestBlobTable

BlobSteps == {
    [from |-> "new", event |-> "ingested", to |-> "pending"],
    [from |-> "new", event |-> "restored", to |-> "stored"],
    [from |-> "pending", event |-> "committed", to |-> "stored"],
    [from |-> "pending", event |-> "abandoned", to |-> "removed"],
    [from |-> "stored", event |-> "dropped", to |-> "garbage"],
    [from |-> "garbage", event |-> "collected", to |-> "removed"]
}

=============================================================================
