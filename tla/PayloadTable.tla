---------------------------- MODULE PayloadTable ----------------------------
\* Code generated from the Go transition table. DO NOT EDIT.
\* Regenerate: S3RP_TLA_UPDATE=1 go test ./s3gw -run TestPayloadTable

PayloadSteps == {
    [from |-> "reading", event |-> "more", to |-> "reading"],
    [from |-> "reading", event |-> "matched", to |-> "verified"],
    [from |-> "reading", event |-> "mismatched", to |-> "refused"],
    [from |-> "reading", event |-> "end", to |-> "reading"],
    [from |-> "verified", event |-> "extra", to |-> "refused"],
    [from |-> "verified", event |-> "end", to |-> "verified"],
    [from |-> "refused", event |-> "more", to |-> "refused"],
    [from |-> "refused", event |-> "matched", to |-> "refused"],
    [from |-> "refused", event |-> "mismatched", to |-> "refused"],
    [from |-> "refused", event |-> "extra", to |-> "refused"],
    [from |-> "refused", event |-> "end", to |-> "refused"]
}

=============================================================================
