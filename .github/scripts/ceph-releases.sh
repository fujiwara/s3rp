#!/bin/bash
# Reports where the Ceph releases under test have fallen behind upstream,
# as the "Following Ceph releases" rule in AGENTS.md asks: a release that
# reached GA but is not the default backend yet, and a tested release that
# reached its end of life. Prints a JSON array of {title, body}; the weekly
# ceph-releases workflow turns each element into an issue.
#
# The source is Ceph's own release index, doc/releases/releases.yml.
# Needs yq and jq (mise.toml).
#
#   RELEASES_FILE  read the index from a file instead of fetching it
#   TODAY          date to judge end of life against (default: today, UTC)
set -euo pipefail

RELEASES_URL=https://raw.githubusercontent.com/ceph/ceph/main/doc/releases/releases.yml
TODAY=${TODAY:-$(date -u +%F)}

cd "$(dirname "$0")/../.."   # repo root

major_of() { sed -n 's|.*quay\.io/ceph/ceph:v\([0-9][0-9]*\).*|\1|p'; }

# the major the compose `ceph` service runs
default=$(yq '.services.ceph.image' compose.yml | major_of)
if [ -z "$default" ]; then
    echo "error: the compose ceph service has no quay.io/ceph/ceph image" >&2
    exit 1
fi
# the major of every Ceph image under test
tested=$(grep -ho 'quay\.io/ceph/ceph:v[0-9]*' compose.yml .github/workflows/s3tests.yml \
    | major_of | sort -un | jq -sc .)

if [ -n "${RELEASES_FILE:-}" ]; then
    cat "$RELEASES_FILE"
else
    curl -fsSL --retry 3 "$RELEASES_URL"
fi | yq -o=json '.releases' | jq --argjson default "$default" --argjson tested "$tested" --arg today "$TODAY" '
    if type != "object" or length == 0 then error("no releases in the index") else . end
    | [ to_entries[]
        | .key as $name | .value as $line
        # x.2.z is a stable release (x.0.z is development, x.1.z a release
        # candidate); a line without one is not out yet
        | ([$line.releases[]?.version | tostring | select(test("^[0-9]+\\.2\\.[0-9]+$"))][0]) as $ga
        | select($ga != null)
        | ($ga | split(".")[0] | tonumber) as $major
        # the date is an upstream estimate until actual_eol confirms it
        | (if $line.actual_eol then "reached its end of life" else "passed its estimated end of life" end) as $state
        | (($line.actual_eol // $line.target_eol // "") | tostring) as $eol
        | (select($major > $default) | {
            major: $major,
            title: "Ceph \($name) (v\($major)) is GA: make it the default test backend",
            body: ("Ceph \($name) has a stable release (v\($major).2.x), and the compose `ceph` service still runs v\($default).\n\n"
              + "Per \"Following Ceph releases\" in AGENTS.md: point `ceph` at the new release once its image is on quay.io, "
              + "move the previous default to a `ceph-<release>` service (compose.yml, the matrix in test.yml, the image choices in s3tests.yml), "
              + "fix `docker/ceph-demo.sh` if the bootstrap changed, and run the s3-tests workflow on the new default.")
          }),
          (select(($tested | index($major)) != null and $eol != "" and $eol <= $today) | {
            major: $major,
            title: "Ceph \($name) (v\($major)) is EOL: drop it from the test targets",
            body: ("Ceph \($name) (v\($major)) \($state) on \($eol), and it is still a test target.\n\n"
              + "Per \"Following Ceph releases\" in AGENTS.md: remove its compose service, matrix entry and s3-tests image choice, "
              + "together with the workarounds that existed only for it. "
              + "An estimated date can slip: check https://docs.ceph.com/en/latest/releases/ first.")
          })
      ]
    | sort_by(.major, .title) | map(del(.major))'
