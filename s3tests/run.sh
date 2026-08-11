#!/usr/bin/env bash
# Runs the ceph/s3-tests compatibility suite against s3rp proxying to the
# Ceph RGW backend from compose.yml. See s3tests/README.md.
set -euo pipefail

S3TESTS_REPO=${S3TESTS_REPO:-https://github.com/ceph/s3-tests}
# pinned so runs are comparable; bump deliberately
S3TESTS_REF=${S3TESTS_REF:-5522d1c351f75bc00ae0f64f742f3f095f5939d9}
PORT=${PORT:-7481}
BACKEND_ENDPOINT=${S3RP_TEST_BACKEND_ENDPOINT:-http://127.0.0.1:7480}

# markers excluded up front (zero signal for s3rp):
#   lifecycle_expiration/transition, cloud_*: need RGW debug-interval tuning
#     and minutes of sleeping; lifecycle writes are 501 anyway (the plain
#     "lifecycle" marker stays selected so those 501s are counted)
#   s3website, sns, storage_class: features absent from the whole chain
#   fails_on_rgw: known-broken against the very backend we proxy
#   auth_aws2: s3rp is deliberately SigV4-only
MARKERS=${MARKERS:-"not lifecycle_expiration and not lifecycle_transition \
and not cloud_transition and not cloud_restore and not s3website and not sns \
and not storage_class and not fails_on_rgw and not auth_aws2"}
# tests that crash the backend RGW daemon itself, killing the rest of the
# run (Ceph 19.2.0 aborts in RGWObjectCtx::set_atomic on UploadPartCopy
# with a percent-encoded copy-source key) — deselected until the backend
# image carries a fix
DESELECT=${DESELECT:-"--deselect s3tests/functional/test_s3.py::test_upload_part_copy_percent_encoded_key"}
PYTEST_ARGS=${PYTEST_ARGS:-}

cd "$(dirname "$0")/.."   # repo root
ROOT=$(pwd)
WORK=s3tests/work
RESULTS=$WORK/results
mkdir -p "$RESULTS"

echo "==> starting ceph backend"
docker compose up -d --wait ceph

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

echo "==> checking out s3-tests @ $S3TESTS_REF"
if [ ! -d "$WORK/s3-tests" ]; then
    git clone "$S3TESTS_REPO" "$WORK/s3-tests"
fi
git -C "$WORK/s3-tests" fetch --quiet origin
git -C "$WORK/s3-tests" checkout --quiet "$S3TESTS_REF"

if [ ! -x "$WORK/venv/bin/pytest" ]; then
    echo "==> creating venv (mise + uv, tools pinned in s3tests/mise.toml)"
    mise --cd s3tests trust --quiet 2>/dev/null || true
    mise --cd s3tests install
    mise --cd s3tests exec -- uv venv --quiet "$ROOT/$WORK/venv"
    mise --cd s3tests exec -- uv pip install --quiet \
        -p "$ROOT/$WORK/venv/bin/python" -r "$ROOT/$WORK/s3-tests/requirements.txt"
fi

sed "s/@PORT@/$PORT/" s3tests/s3tests.conf.in > "$WORK/s3tests.conf"

echo "==> running suite (results in $RESULTS)"
VENV=$(pwd)/$WORK/venv
CONF=$(pwd)/$WORK/s3tests.conf
XML=$(pwd)/$RESULTS/results.xml
OUT=$(pwd)/$RESULTS/pytest.out
(
    cd "$WORK/s3-tests"
    S3TEST_CONF="$CONF" "$VENV/bin/pytest" \
        s3tests/functional/test_s3.py s3tests/functional/test_headers.py \
        -m "$MARKERS" \
        $DESELECT \
        -rA -q -p no:cacheprovider \
        --junitxml="$XML" \
        $PYTEST_ARGS \
        2>&1 | tee "$OUT"
) || true

echo "==> triaging"
"$VENV/bin/python" s3tests/triage.py "$XML" "$RESULTS/harness.log" > "$RESULTS/report.md"
echo "report: $RESULTS/report.md"
