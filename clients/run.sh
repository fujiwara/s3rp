#!/usr/bin/env bash
# Runs S3 clients that real deployments use against s3rp proxying to the
# Ceph RGW backend (ceph-tls) from compose.yml, https on both sides. See
# clients/README.md.
#
#   ./clients/run.sh [client...]     default: every client in clients/defs
#   SCHEMES=http ./clients/run.sh    default: https, or what a definition names
#   JOBS=1 ./clients/run.sh          default: 4 clients at a time
set -euo pipefail

# the first gateway's port; each of the JOBS workers has its own from there
PORT=${PORT:-7483}
JOBS=${JOBS:-4}
SCHEMES=${SCHEMES:-}
BACKEND_ENDPOINT=${S3RP_TEST_BACKEND_ENDPOINT:-https://127.0.0.1:7443}

cd "$(dirname "$0")/.."   # repo root
ROOT=$(pwd)
WORK=$ROOT/clients/work
RESULTS=$WORK/results
rm -rf "$RESULTS" "$WORK/out" "$WORK/state"
mkdir -p "$RESULTS" "$WORK/fixtures"

if [ "$#" -gt 0 ]; then
    CLIENTS=("$@")
else
    CLIENTS=()
    for f in clients/defs/*.yml; do CLIENTS+=("$(basename "$f" .yml)"); done
fi

# Nothing of the caller's AWS configuration reaches a client: a profile it
# does not have, or a session token sent along with the run's key, fails
# every request of the clients that read the environment.
for v in $(compgen -e AWS_); do unset "$v"; done

# what the gateway config (clients/s3rp.yml), the client definitions and
# the scenarios read
export PORT BACKEND_ENDPOINT
export BUCKET=compat
export ACCESS_KEY=S3RPCLIENTKEY0000000
export SECRET_KEY=s3rpclientsecret00000000000000000000000k
export BACKEND_ACCESS_KEY=${S3RP_TEST_BACKEND_ACCESS_KEY_ID:-backendkey}
export BACKEND_SECRET_KEY=${S3RP_TEST_BACKEND_SECRET_ACCESS_KEY:-backendsecret}
export FIXTURES=$WORK/fixtures
export LIB=$ROOT/clients/lib
export OUT_DIR=$WORK/out
# where a definition's setup keeps a client mise has no package for
export TOOLS=$WORK/tools
RUN_ID=$(date +%s)

echo "==> installing tools (mise, pinned in clients/mise.toml)"
mise trust --quiet 2>/dev/null || true
mise --cd clients trust --quiet 2>/dev/null || true
mise install --quiet
mise --cd clients install --quiet
# the tools under test, runn, and the root mise.toml's yq and jq
eval "$(mise --cd clients env --shell bash)"
export PATH=$ROOT/clients/bin:$PATH

echo "==> generating fixtures"
[ -f "$FIXTURES/1k.bin" ] || head -c 1024 /dev/urandom > "$FIXTURES/1k.bin"
# above the largest multipart threshold among the clients (the AWS SDK for
# Ruby: 100 MiB)
BIG=$((128 * 1024 * 1024))
[ "$(wc -c < "$FIXTURES/big.bin" 2>/dev/null)" = "$BIG" ] || head -c "$BIG" /dev/urandom > "$FIXTURES/big.bin"
if [ ! -d "$FIXTURES/tree" ]; then
    mkdir "$FIXTURES/tree"
    for i in $(seq 1 1100); do echo "$i" > "$FIXTURES/tree/f$i.txt"; done
fi

# A certificate for the gateway and the backend, issued by a CA of the run's own and
# trusted through the environment: SSL_CERT_FILE by Go, Rust and OpenSSL
# clients, CURL_CA_BUNDLE by libcurl ones. Not one self-signed certificate
# for both roles: rustls refuses a CA certificate presented by a server.
# The bundle keeps the system roots, since a variable that replaces them
# would also stop a client's package manager reaching its registry.
CERT=$WORK/tls/cert.pem
export CA_FILE=$WORK/tls/bundle.pem
if [ ! -f "$WORK/tls/ca.pem" ]; then
    rm -rf "$WORK/tls"
    mkdir -p "$WORK/tls"
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
        -subj "/CN=s3rp client compatibility CA" \
        -addext "basicConstraints=critical,CA:TRUE" -addext "keyUsage=critical,keyCertSign,cRLSign" \
        -keyout "$WORK/tls/ca-key.pem" -out "$WORK/tls/ca.pem" 2>/dev/null
    openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes -days 3650 \
        -CA "$WORK/tls/ca.pem" -CAkey "$WORK/tls/ca-key.pem" \
        -subj "/CN=s3rp client compatibility" -addext "subjectAltName=IP:127.0.0.1" \
        -addext "basicConstraints=critical,CA:FALSE" -addext "extendedKeyUsage=serverAuth" \
        -keyout "$WORK/tls/key.pem" -out "$CERT" 2>/dev/null
    # RGW reads the key as its own user, not the one that owns the file
    chmod 644 "$WORK/tls/key.pem"
    cat "$(openssl version -d | cut -d'"' -f2)/cert.pem" "$WORK/tls/ca.pem" > "$CA_FILE"
fi
export SSL_CERT_FILE=$CA_FILE CURL_CA_BUNDLE=$CA_FILE
if [ "$BACKEND_ENDPOINT" = "https://127.0.0.1:7443" ]; then
    echo "==> starting Ceph RGW backend"
    # It reads the certificate only when it starts, so one started before
    # the certificate was made is recreated; otherwise a running one is
    # used as it is.
    recreate=--force-recreate
    started=$(docker compose ps -q ceph-tls | xargs -r docker inspect -f '{{.State.StartedAt}}' 2>/dev/null || true)
    if [ -n "$started" ] && [ "$(date -d "$started" +%s)" -gt "$(stat -c %Y "$CERT")" ]; then
        recreate=""
    fi
    docker compose up -d --wait $recreate ceph-tls
    # A reused backend still holds what earlier runs deleted: RGW frees a
    # deleted object's data two hours later, and a few runs in that time
    # fill the 10 GiB OSD (507 InsufficientCapacity).
    docker compose exec -T ceph-tls radosgw-admin gc process --include-all > /dev/null
else
    echo "==> using external backend $BACKEND_ENDPOINT"
fi
{
    cat clients/s3rp.yml
    printf 'tls:\n  cert_file: %s\n  key_file: %s\n' "$CERT" "$WORK/tls/key.pem"
} > "$WORK/s3rp-https.yml"

echo "==> building s3rp"
go build -o "$WORK/s3rp" ./cmd/s3rp

# The requests a client must never be answered with, whatever its exit
# code says: a refusal of something it sent (501), a failure (5xx), or a
# signature the gateway could not verify.
refused() {
    jq -c 'select(.msg == "request")
        | select(.status >= 500 or .code == "SignatureDoesNotMatch" or .code == "AccessDenied")
        | {method, path, query, status, code, operation, user_agent}' "$1"
}

# A common scenario is named after its file; one of a client's own
# (clients/own/<client>/) is prefixed with the client.
scenario_name() {
    case $1 in
        clients/own/*) echo "$(basename "$(dirname "$1")"):$(basename "$1" .yml)" ;;
        *) basename "$1" .yml ;;
    esac
}

# What a client's run leaves: its mount or the like, the gateway, and the
# backend bucket. Without the last, runs pile up in the backend.
cleanup() {
    if [ -n "${teardown:-}" ]; then
        (eval "$teardown") >> "$dir/setup.log" 2>&1 || true
    fi
    [ -z "${gateway_pid:-}" ] || kill "$gateway_pid" 2>/dev/null || true
    rclone purge ":s3,provider=Other,endpoint='$BACKEND_ENDPOINT',access_key_id='$BACKEND_ACCESS_KEY',secret_access_key='$BACKEND_SECRET_KEY':$BACKEND_BUCKET" \
        > "$dir/cleanup.log" 2>&1 || echo "    could not remove backend bucket $BACKEND_BUCKET (see $dir/cleanup.log)" >&2
}

# One gateway and one backend bucket per scheme and client, so a client
# never finds objects another run left: rclone skips an upload whose
# destination already matches.
run_client() {
    local scheme=$1 client=$2
    export PORT=$3
    local def=$ROOT/clients/defs/$client.yml
    # not local: cleanup reads them when the subshell exits
    dir=$RESULTS/$scheme/$client teardown="" gateway_pid=""
    mkdir -p "$dir" "$OUT_DIR/$scheme/$client"
    export SCHEME=$scheme ENDPOINT=$scheme://127.0.0.1:$PORT
    export BACKEND_BUCKET=compat-$RUN_ID-$scheme-$client
    export STATE_DIR=$WORK/state/$scheme/$client OUT_DIR=$WORK/out/$scheme/$client
    curl -fsS -o /dev/null -X PUT --aws-sigv4 "aws:amz:us-east-1:s3" \
        --user "$BACKEND_ACCESS_KEY:$BACKEND_SECRET_KEY" "$BACKEND_ENDPOINT/$BACKEND_BUCKET"
    trap cleanup EXIT

    local config=clients/s3rp.yml
    [ "$scheme" = https ] && config=$WORK/s3rp-https.yml
    local log=$dir/s3rp.log
    # a gateway an aborted run left would serve this one's requests unseen
    if curl -sk -o /dev/null "$ENDPOINT/" || curl -s -o /dev/null "http://127.0.0.1:$PORT/"; then
        echo "port $PORT is already in use" >&2
        exit 2
    fi
    # the verdict reads the info-level request lines, whatever the caller's
    # S3RP_LOG_LEVEL
    "$WORK/s3rp" --config "$config" --log-level info 2> "$log" &
    gateway_pid=$!
    for _ in $(seq 1 50); do
        curl -s -o /dev/null "$ENDPOINT/" && break
        sleep 0.2
    done
    kill -0 "$gateway_pid" 2>/dev/null || { echo "s3rp did not start (see $log)" >&2; exit 2; }

    # the client's own configuration, with this run's endpoint and key
    eval "$(yq -r '.env // {} | to_entries | .[]
        | "export \(.key)=\(.value | envsubst | @sh)"' "$def")"
    export CLIENT_FILE=$def
    mkdir -p "$STATE_DIR"
    if ! (eval "$(yq -r '.setup // ""' "$def")") > "$dir/setup.log" 2>&1; then
        printf '%s\t%s\t%s\tFAIL\tsetup failed\n' "$client" "$scheme" "(setup)" >> "$RESULTS/results.tsv"
        echo "    $client $scheme: setup FAILED (see $dir/setup.log)"
        return
    fi
    teardown=$(yq -r '.teardown // ""' "$def")

    # the common scenarios, then the client's own
    local scenario name out missing verb offset result detail
    for scenario in clients/scenarios/*.yml clients/own/"$client"/*.yml; do
        [ -f "$scenario" ] || continue
        name=$(scenario_name "$scenario")
        out=$dir/${name#*:}
        missing=""
        for verb in $(grep -o 'vars\.client\.verbs\.[a-z_]*' "$scenario" | sort -u | sed 's/.*\.//'); do
            [ "$(yq -r ".verbs.$verb // \"\"" "$def")" != "" ] || missing="$missing $verb"
        done
        if [ -n "$missing" ]; then
            printf '%s\t%s\t%s\tskip\tno verb:%s\n' "$client" "$scheme" "$name" "$missing" >> "$RESULTS/results.tsv"
            continue
        fi
        offset=$(wc -c < "$log")
        result=pass detail=""
        # --debug puts each command and its output in the log; stdin closed
        # because runn waits on an open one
        if ! runn run --debug --scopes run:exec,read:parent "$scenario" > "$out.log" 2>&1 < /dev/null; then
            result=FAIL detail="scenario failed"
        fi
        # a line that is not JSON (a Go runtime message) must not end the read
        tail -c +"$((offset + 1))" "$log" | jq -cR 'fromjson? | select(.msg == "request")' > "$out.requests.jsonl"
        refused "$out.requests.jsonl" > "$out.refused.jsonl"
        if [ -s "$out.refused.jsonl" ]; then
            result=FAIL
            detail="${detail:+$detail; }$(jq -r '"\(.status) \(.code)"' "$out.refused.jsonl" | sort | uniq -c | xargs)"
        fi
        # a client that sent the file in one request did not run this one
        if [ "$name" = multipart ] && ! grep -q '"operation":"CreateMultipartUpload"' "$out.requests.jsonl"; then
            result=FAIL
            detail="${detail:+$detail; }no multipart upload"
        fi
        printf '%s\t%s\t%s\t%s\t%s\n' "$client" "$scheme" "$name" "$result" "$detail" >> "$RESULTS/results.tsv"
        echo "    $client $scheme $name: $result${detail:+ ($detail)}"
    done
}

COLUMNS=()
for client in "${CLIENTS[@]}"; do
    # https unless the definition names the schemes its client can be run
    # over; SCHEMES asks for others, within what the definition names
    for scheme in ${SCHEMES:-$(yq '.schemes // ["https"] | .[]' "clients/defs/$client.yml")}; do
        [ "$(SCHEME=$scheme yq '.schemes // [strenv(SCHEME)] | contains([strenv(SCHEME)])' "clients/defs/$client.yml")" = true ] || continue
        COLUMNS+=("$client/$scheme")
    done
done

# JOBS workers, each with a gateway port of its own, take the next column
# until none is left. A scenario's verdict reads the gateway's log, which
# is why the gateway is per client and only the backend is shared.
: > "$RESULTS/next"
worker() {
    local port=$1 n column rc
    while :; do
        n=$(flock "$RESULTS/next" sh -c 'n=$(cat "$1"); echo $((n + 1)) > "$1"; echo "${n:-0}"' sh "$RESULTS/next")
        [ "$n" -lt "${#COLUMNS[@]}" ] || return 0
        column=${COLUMNS[$n]}
        # a subshell, so one client's environment does not reach the next
        # and not as the left side of ||, where bash ignores errexit in the
        # whole of run_client: a failed step there must abort the run
        set +e
        (set -e; run_client "${column#*/}" "${column%/*}" "$port")
        rc=$?
        set -e
        if [ "$rc" -ne 0 ]; then
            echo "$column" >> "$RESULTS/aborted"
            return 1
        fi
        # the others finish the client they are on and take no more
        [ ! -e "$RESULTS/aborted" ] || return 0
    done
}
echo "==> running ${#COLUMNS[@]} clients, $JOBS at a time"
for job in $(seq 0 $((JOBS - 1))); do
    worker $((PORT + job)) &
done
wait
if [ -e "$RESULTS/aborted" ]; then
    echo "aborted at $(paste -sd' ' "$RESULTS/aborted")" >&2
    exit 2
fi

echo "==> report"
status=0
# expected.tsv is exact: an unlisted failure fails the run, and so does a
# listed scenario that passes
grep -v '^#' clients/expected.tsv | cut -f1-3 | sort > "$RESULTS/expected.txt" || true
awk -F'\t' '$4 == "FAIL" { print $1 "\t" $2 "\t" $3 }' "$RESULTS/results.tsv" | sort > "$RESULTS/failed.txt"
comm -13 "$RESULTS/expected.txt" "$RESULTS/failed.txt" > "$RESULTS/unexpected.txt"
# only for what this run covered
comm -23 "$RESULTS/expected.txt" "$RESULTS/failed.txt" \
    | awk -F'\t' -v cs=" ${COLUMNS[*]} " 'index(cs, " " $1 "/" $2 " ")' > "$RESULTS/fixed.txt"

SCENARIOS=()
for scenario in clients/scenarios/*.yml; do SCENARIOS+=("$(scenario_name "$scenario")"); done
{
    echo "# Client compatibility"
    echo
    echo "| client | $(printf '%s | ' "${SCENARIOS[@]}")own |"
    echo "|---|$(printf -- '---|%.0s' "${SCENARIOS[@]}")---|"
    for column in "${COLUMNS[@]}"; do
        client=${column%/*} scheme=${column#*/}
        # a failure expected.tsv lists is an xfail
        cell() {
            awk -F'\t' -v c="$client" -v h="$scheme" -v s="$1" -v x="$RESULTS/expected.txt" '
                BEGIN { while ((getline line < x) > 0) expected[line] = 1 }
                $1 == c && $2 == h && $3 == s {
                    print ($4 == "FAIL" && (c "\t" h "\t" s) in expected) ? "xfail" : $4
                }' "$RESULTS/results.tsv"
        }
        row="| $column |"
        for name in "${SCENARIOS[@]}"; do row="$row $(cell "$name") |"; done
        own=""
        for scenario in clients/own/"$client"/*.yml; do
            [ -f "$scenario" ] || continue
            name=$(scenario_name "$scenario")
            own="$own${own:+, }${name#*:}: $(cell "$name")"
        done
        echo "$row $own |"
    done
    echo
    echo "pass; xfail = failed as expected.tsv says; FAIL = failed otherwise; skip = the client has no such verb."
    if [ -s "$RESULTS/unexpected.txt" ]; then
        echo
        echo "## Unexpected failures"
        echo
        awk -F'\t' 'NR == FNR { u[$0] = 1; next } ($1 "\t" $2 "\t" $3) in u { printf "- %s/%s %s: %s\n", $1, $2, $3, $5 }' \
            "$RESULTS/unexpected.txt" "$RESULTS/results.tsv"
    fi
    if [ -s "$RESULTS/fixed.txt" ]; then
        echo
        echo "## Listed in expected.tsv but passing"
        echo
        awk -F'\t' '{ printf "- %s/%s %s\n", $1, $2, $3 }' "$RESULTS/fixed.txt"
    fi
    echo
    echo "## Expected failures"
    echo
    grep -v '^#' clients/expected.tsv | awk -F'\t' -v cs=" ${COLUMNS[*]} " \
        'index(cs, " " $1 "/" $2 " ") { printf "- %s/%s %s — %s\n", $1, $2, $3, $4 }'
    echo
    echo "## Requests sent"
    for column in "${COLUMNS[@]}"; do
        dir=$RESULTS/${column#*/}/${column%/*}
        ls "$dir"/*.requests.jsonl > /dev/null 2>&1 || continue
        echo
        echo "### $column"
        echo
        # the helpers' own curl requests are not the client's
        cat "$dir"/*.requests.jsonl | jq -c 'select(.user_agent | startswith("curl/") | not)' > "$dir/client-requests.jsonl"
        # one a line: a client may put the command or the features in use
        # in it, and send a dozen different ones
        echo "User-Agent, with the requests that carried it:"
        echo
        jq -r .user_agent "$dir/client-requests.jsonl" | sort | uniq -c \
            | sed -E 's/^ *([0-9]+) (.*)$/- `\2`: \1/'
        echo
        echo "Operations:"
        echo
        jq -r '.operation // "(none)"' "$dir/client-requests.jsonl" \
            | sort | uniq -c | awk '{ printf "- %s: %s\n", $2, $1 }'
    done
} > "$RESULTS/report.md"

[ -s "$RESULTS/unexpected.txt" ] || [ -s "$RESULTS/fixed.txt" ] && status=1
cat "$RESULTS/report.md"
echo "report: $RESULTS/report.md"
exit "$status"
