----------------------------- MODULE ReportTable -----------------------------
\* Code generated from the Go transition table. DO NOT EDIT.
\* Regenerate: S3RP_TLA_UPDATE=1 go test ./s3gw -run TestReportTable

ReportSteps == {
    [outcome |-> "returned", response |-> "pending", document |-> FALSE, observe |-> TRUE, abort |-> FALSE],
    [outcome |-> "returned", response |-> "started", document |-> FALSE, observe |-> TRUE, abort |-> FALSE],
    [outcome |-> "failed", response |-> "pending", document |-> TRUE, observe |-> TRUE, abort |-> FALSE],
    [outcome |-> "failed", response |-> "started", document |-> FALSE, observe |-> TRUE, abort |-> FALSE],
    [outcome |-> "panicked", response |-> "pending", document |-> TRUE, observe |-> TRUE, abort |-> FALSE],
    [outcome |-> "panicked", response |-> "started", document |-> FALSE, observe |-> TRUE, abort |-> TRUE],
    [outcome |-> "aborted", response |-> "pending", document |-> FALSE, observe |-> FALSE, abort |-> TRUE],
    [outcome |-> "aborted", response |-> "started", document |-> FALSE, observe |-> FALSE, abort |-> TRUE]
}

=============================================================================
