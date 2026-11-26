----------------------------- MODULE UploadTable -----------------------------
\* Code generated from the Go transition table. DO NOT EDIT.
\* Regenerate: S3RP_TLA_UPDATE=1 go test ./s3gw/localbackend -run TestUploadTable

UploadSteps == {
    [from |-> "unknown", event |-> "created", to |-> "open"],
    [from |-> "open", event |-> "used", to |-> "open"],
    [from |-> "open", event |-> "completed", to |-> "finished"],
    [from |-> "open", event |-> "aborted", to |-> "finished"],
    [from |-> "finished", event |-> "aborted", to |-> "finished"]
}

=============================================================================
