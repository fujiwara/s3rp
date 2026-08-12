#!/usr/bin/env bash
# Simple performance benchmark for s3rp fronting the local microceph RGW.
# Runs warp (put / get / multipart) through the proxy and directly against
# RGW as a baseline, sampling CPU usage (s3rp / radosgw / warp) with pidstat.
# Results are aggregated into bench/report.md by report.py.
set -euo pipefail
cd "$(dirname "$0")"
ROOT=$(cd .. && pwd)
OUT=out

WARP=${WARP:-$HOME/bin/warp}
DURATION=${DURATION:-20s}
CONCURRENT=${CONCURRENT:-8}
OBJ_SIZE=${OBJ_SIZE:-1MiB}
GET_OBJECTS=${GET_OBJECTS:-500}
PART_SIZE=${PART_SIZE:-5MiB}
PARTS=${PARTS:-50}

RGW_ENDPOINT=${RGW_ENDPOINT:-127.0.0.1:7490}
S3RP_ENDPOINT=${S3RP_ENDPOINT:-127.0.0.1:8090}
BACKEND_BUCKET=s3rp-bench
FRONT_BUCKET=warp-benchmark-bucket
FRONT_KEY=S3RPBENCHKEY
FRONT_SECRET=S3RPBENCHSECRET

# default to the backend user created by s3tests/setup-microceph.sh;
# exported so s3rp's config expansion (${RGW_ACCESS_KEY} in config.yml) sees them
export RGW_ACCESS_KEY=${RGW_ACCESS_KEY:-backendkey}
export RGW_SECRET_KEY=${RGW_SECRET_KEY:-backendsecret}

mkdir -p "$OUT"
rm -f "$OUT"/*.json.zst "$OUT"/*.txt "$OUT"/cpu-*.log

rgw_aws() {
    env -u AWS_PROFILE \
        AWS_ACCESS_KEY_ID="$RGW_ACCESS_KEY" AWS_SECRET_ACCESS_KEY="$RGW_SECRET_KEY" \
        AWS_REGION=us-east-1 AWS_EC2_METADATA_DISABLED=true \
        aws --endpoint-url "http://$RGW_ENDPOINT" "$@"
}

echo "==> ensuring backend bucket $BACKEND_BUCKET exists on RGW"
rgw_aws s3api head-bucket --bucket "$BACKEND_BUCKET" 2>/dev/null ||
    rgw_aws s3api create-bucket --bucket "$BACKEND_BUCKET"

echo "==> building s3rp"
(cd "$ROOT" && go build -o bench/out/s3rp ./cmd/s3rp)

echo "==> starting s3rp on $S3RP_ENDPOINT"
S3RP_LOG_LEVEL=${S3RP_LOG_LEVEL:-warn}
./out/s3rp --config config.yml --log-level "$S3RP_LOG_LEVEL" >"$OUT/s3rp.log" 2>&1 &
S3RP_PID=$!
cleanup() {
    kill "$S3RP_PID" 2>/dev/null || true
    [ -n "${PIDSTAT_PID:-}" ] && kill "$PIDSTAT_PID" 2>/dev/null || true
}
trap cleanup EXIT

for i in $(seq 1 50); do
    code=$(curl -s -o /dev/null -w '%{http_code}' "http://$S3RP_ENDPOINT/" || true)
    [ "$code" != "000" ] && break
    kill -0 "$S3RP_PID" 2>/dev/null || { echo "s3rp died; see $OUT/s3rp.log" >&2; exit 1; }
    sleep 0.2
done

# run_bench <name> <proxy|direct> <warp subcommand + extra flags...>
run_bench() {
    local name=$1 target=$2
    shift 2
    local host key secret bucket
    if [ "$target" = proxy ]; then
        host=$S3RP_ENDPOINT key=$FRONT_KEY secret=$FRONT_SECRET bucket=$FRONT_BUCKET
    else
        host=$RGW_ENDPOINT key=$RGW_ACCESS_KEY secret=$RGW_SECRET_KEY bucket=$BACKEND_BUCKET
    fi
    echo "==> [$name] warp $* ($target)"
    LC_ALL=C pidstat -h -u -C 's3rp|radosgw|warp' 1 >"$OUT/cpu-$name.log" &
    PIDSTAT_PID=$!
    "$WARP" "$@" --host "$host" --access-key "$key" --secret-key "$secret" \
        --bucket "$bucket" --concurrent "$CONCURRENT" \
        --benchdata "$OUT/$name" --no-color --quiet >"$OUT/$name.txt" 2>&1 ||
        { kill "$PIDSTAT_PID" 2>/dev/null; echo "warp failed; see $OUT/$name.txt" >&2; exit 1; }
    kill "$PIDSTAT_PID" 2>/dev/null || true
    wait "$PIDSTAT_PID" 2>/dev/null || true
    PIDSTAT_PID=
}

PUT_ARGS=(put --obj.size "$OBJ_SIZE" --duration "$DURATION" --disable-multipart)
GET_ARGS=(get --obj.size "$OBJ_SIZE" --objects "$GET_OBJECTS" --duration "$DURATION" --disable-multipart)
# multipart-put = MPU write path (CreateMultipartUpload/UploadPart/Complete);
# `warp multipart` measures GET of a multipart object instead. part.concurrent=1
# keeps total parallelism at CONCURRENT (8 MPUs, parts uploaded serially).
MPU_ARGS=(multipart-put --part.size "$PART_SIZE" --parts "$PARTS" --part.concurrent 1 --duration "$DURATION")

run_bench put-proxy proxy "${PUT_ARGS[@]}"
run_bench get-proxy proxy "${GET_ARGS[@]}"
run_bench multipart-proxy proxy "${MPU_ARGS[@]}"

run_bench put-direct direct "${PUT_ARGS[@]}"
run_bench get-direct direct "${GET_ARGS[@]}"
run_bench multipart-direct direct "${MPU_ARGS[@]}"

kill "$S3RP_PID" 2>/dev/null || true
wait "$S3RP_PID" 2>/dev/null || true
S3RP_PID=

echo "==> generating report.md"
python3 report.py
echo "done: bench/report.md"
