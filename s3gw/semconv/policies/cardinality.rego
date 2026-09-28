package after_resolution

import rego.v1

# Identifiers that are unbounded per request or per principal. They belong in
# the log (RequestInfo carries them), never on a metric.
forbidden := {
	"aws.s3.key",
	"aws.s3.upload_id",
	"aws.s3.copy_source",
	"aws.request_id",
	"user.id",
	"user.name",
	"s3gw.user",
	"s3gw.access_key_id",
	"s3gw.request_id",
}

# Bounded only by the number of tenants or buckets: allowed, but the service
# decides whether it can afford them.
opt_in_only := {"s3gw.tenant", "s3gw.bucket_owner", "aws.s3.bucket"}

deny contains violation(sprintf("Metric '%s' must not carry the unbounded attribute '%s'.", [g.metric_name, a.name]), g.id, a.name) if {
	some g in input.groups
	g.type == "metric"
	some a in g.attributes
	a.name in forbidden
}

deny contains violation(sprintf("Metric '%s' must declare '%s' opt_in, not %v.", [g.metric_name, a.name, a.requirement_level]), g.id, a.name) if {
	some g in input.groups
	g.type == "metric"
	some a in g.attributes
	a.name in opt_in_only
	a.requirement_level != "opt_in"
}

violation(description, group, attr) := {
	"id": description,
	"type": "semconv_attribute",
	"category": "cardinality",
	"group": group,
	"attr": attr,
}
