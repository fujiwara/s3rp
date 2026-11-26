--------------------------- MODULE VersioningTable ---------------------------
\* Code generated from the Go transition table. DO NOT EDIT.
\* Regenerate: S3RP_TLA_UPDATE=1 go test ./s3gw/localbackend -run TestVersioningTable

VersioningSteps == {
    [from |-> "off", event |-> "enable", to |-> "enabled"],
    [from |-> "off", event |-> "suspend", to |-> "suspended"],
    [from |-> "enabled", event |-> "enable", to |-> "enabled"],
    [from |-> "enabled", event |-> "suspend", to |-> "suspended"],
    [from |-> "enabled", event |-> "lock", to |-> "locked"],
    [from |-> "suspended", event |-> "enable", to |-> "enabled"],
    [from |-> "suspended", event |-> "suspend", to |-> "suspended"],
    [from |-> "locked", event |-> "enable", to |-> "locked"],
    [from |-> "locked", event |-> "lock", to |-> "locked"]
}

=============================================================================
