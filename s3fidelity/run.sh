#!/usr/bin/env bash
# Runs the S3 fidelity tests of aws/aws-emulator-evaluation-suite against
# s3rp proxying to the Ceph RGW backend from compose.yml. See
# s3fidelity/README.md.
set -euo pipefail

SUITE_REPO=${SUITE_REPO:-https://github.com/aws/aws-emulator-evaluation-suite}
# pinned so runs are comparable and expected.tsv stays exact; bump deliberately
SUITE_REF=${SUITE_REF:-abd08117ad242c3e244125df9d525c41eb6f7d91}
PORT=${PORT:-7482}
BACKEND_ENDPOINT=${S3RP_TEST_BACKEND_ENDPOINT:-http://127.0.0.1:7480}
JOBS=${JOBS:-4}

cd "$(dirname "$0")/.."   # repo root
ROOT=$(pwd)
WORK=$ROOT/s3fidelity/work
RESULTS=$WORK/results
rm -rf "$RESULTS"
mkdir -p "$RESULTS/reports" "$RESULTS/logs"

if [ "$BACKEND_ENDPOINT" = "http://127.0.0.1:7480" ]; then
    echo "==> starting ceph backend"
    docker compose up -d --wait ceph
    # the service uses the host network, and its healthcheck is satisfied by
    # anything answering on the port: another Ceph already bound there lets
    # --wait succeed while this one fails to start
    if ! docker compose ps --status running --services | grep -qx ceph; then
        echo "the compose ceph service is not running; is another Ceph using its ports?" >&2
        exit 2
    fi
else
    echo "==> using external backend $BACKEND_ENDPOINT"
fi

echo "==> building and starting harness on 127.0.0.1:$PORT"
go build -o "$WORK/s3tests-harness" ./cmd/s3tests-harness
"$WORK/s3tests-harness" -listen "127.0.0.1:$PORT" -backend-endpoint "$BACKEND_ENDPOINT" \
    2> "$RESULTS/harness.log" &
HARNESS_PID=$!
trap 'kill "$HARNESS_PID" 2>/dev/null || true' EXIT
for _ in $(seq 1 50); do
    curl -s -o /dev/null "http://127.0.0.1:$PORT/" && break
    sleep 0.2
done

echo "==> checking out the suite @ $SUITE_REF"
SUITE=$WORK/suite
if [ ! -d "$SUITE/.git" ]; then
    git init --quiet "$SUITE"
    git -C "$SUITE" remote add origin "$SUITE_REPO"
    # the S3 tests and the runner are all that is needed of a large repository
    git -C "$SUITE" sparse-checkout set behavioral/s3 src
fi
git -C "$SUITE" fetch --quiet --depth 1 --filter=blob:none origin "$SUITE_REF"
git -C "$SUITE" checkout --quiet FETCH_HEAD

echo "==> installing the runner (mise + uv, pinned in s3fidelity/mise.toml)"
# the root mise.toml (jq) applies under s3fidelity/ too, so both need trusting
mise trust --quiet 2>/dev/null || true
mise --cd s3fidelity trust --quiet 2>/dev/null || true
mise install --quiet
mise --cd s3fidelity install --quiet
(cd "$SUITE" && mise --cd "$ROOT/s3fidelity" exec -- uv sync --quiet --locked --extra checks --project "$SUITE")
RUNNER=$SUITE/.venv/bin/fidelity-tests-runner

# The tests the gateway can serve: every operation a test calls is one the
# catalog supports, or one of the two the harness answers itself.
echo "==> selecting tests"
mise exec -- jq -r --slurpfile catalog s3op/operations.json '
    ($catalog[0].operations | map(select(.support == "supported") | .name)
        + ["CreateBucket", "DeleteBucket"]) as $served
    | select(.service == "s3")
    | select([.operations[] | ltrimstr("s3:")] - $served == [])
    | .path' "$SUITE/manifest.jsonl" > "$RESULTS/selected.txt"
echo "    $(wc -l < "$RESULTS/selected.txt") of $(grep -c '"service": "s3"' "$SUITE/manifest.jsonl") S3 tests"

# The runner signs with a key file it insists only the owner can write,
# directory included. The key is the harness's fixed "main" tenant key.
CREDS=$WORK/creds
rm -rf "$CREDS"
mkdir -m 700 "$CREDS"
(umask 077; printf 'aws_access_key_id=S3RPMAINKEY000000000\naws_secret_access_key=s3rpmainsecret0000000000000000000000000k\n' > "$CREDS/key")

# One runner per test, as the suite prescribes. The region is the
# gateway's own, which GetBucketLocation and HeadBucket report.
echo "==> running suite (results in $RESULTS)"
export RUNNER PORT CREDS RESULTS
(
    cd "$SUITE"
    xargs -P "$JOBS" -I{} sh -c '
        name=$(basename "$1" .py)
        "$RUNNER" run "$1" --endpoint "http://127.0.0.1:$PORT" --region us-east-1 \
            --credentials-file "$CREDS/key" --report "$RESULTS/reports/$name.json" \
            > "$RESULTS/logs/$name.log" 2>&1 || true
    ' _ {} < "$RESULTS/selected.txt"
)

echo "==> triaging"
status=0
"$SUITE/.venv/bin/python" s3fidelity/triage.py "$RESULTS/reports" s3fidelity/expected.tsv \
    "$RESULTS/selected.txt" > "$RESULTS/report.md" || status=$?
echo "report: $RESULTS/report.md"
exit "$status"
