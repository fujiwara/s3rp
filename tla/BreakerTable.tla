---------------------------- MODULE BreakerTable ----------------------------
\* Code generated from the Go transition table. DO NOT EDIT.
\* Regenerate: S3RP_TLA_UPDATE=1 go test ./s3gw -run TestBreakerTable

BreakerSteps == {
    [from |-> "closed", event |-> "allow-early", to |-> "closed", admit |-> TRUE, count |-> FALSE, stamp |-> FALSE],
    [from |-> "closed", event |-> "allow-due", to |-> "closed", admit |-> TRUE, count |-> FALSE, stamp |-> FALSE],
    [from |-> "closed", event |-> "success", to |-> "closed", admit |-> FALSE, count |-> FALSE, stamp |-> FALSE],
    [from |-> "closed", event |-> "failure", to |-> "closed", admit |-> FALSE, count |-> TRUE, stamp |-> FALSE],
    [from |-> "closed", event |-> "failure-tripping", to |-> "open", admit |-> FALSE, count |-> FALSE, stamp |-> TRUE],
    [from |-> "open", event |-> "allow-early", to |-> "open", admit |-> FALSE, count |-> FALSE, stamp |-> FALSE],
    [from |-> "open", event |-> "allow-due", to |-> "half-open", admit |-> TRUE, count |-> FALSE, stamp |-> TRUE],
    [from |-> "open", event |-> "success", to |-> "closed", admit |-> FALSE, count |-> FALSE, stamp |-> FALSE],
    [from |-> "open", event |-> "failure", to |-> "open", admit |-> FALSE, count |-> FALSE, stamp |-> FALSE],
    [from |-> "open", event |-> "failure-tripping", to |-> "open", admit |-> FALSE, count |-> FALSE, stamp |-> FALSE],
    [from |-> "half-open", event |-> "allow-early", to |-> "half-open", admit |-> FALSE, count |-> FALSE, stamp |-> FALSE],
    [from |-> "half-open", event |-> "allow-due", to |-> "half-open", admit |-> TRUE, count |-> FALSE, stamp |-> TRUE],
    [from |-> "half-open", event |-> "success", to |-> "closed", admit |-> FALSE, count |-> FALSE, stamp |-> FALSE],
    [from |-> "half-open", event |-> "failure", to |-> "open", admit |-> FALSE, count |-> FALSE, stamp |-> TRUE],
    [from |-> "half-open", event |-> "failure-tripping", to |-> "open", admit |-> FALSE, count |-> FALSE, stamp |-> TRUE]
}

=============================================================================
