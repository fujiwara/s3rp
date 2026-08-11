#!/usr/bin/env bash
# Sets up a MicroCeph cluster with an RGW endpoint for the s3-tests suite.
#
# The compose.yml ceph service (quay.io/ceph/demo) is frozen at Ceph 19.2.0
# — that image is no longer built — so this is the way to test against a
# current Ceph: MicroCeph tracks upstream point releases per channel.
#
# Needs root:  sudo ./s3tests/setup-microceph.sh
#
#   MICROCEPH_CHANNEL  snap channel (default tentacle/stable; e.g. squid/stable)
#   RGW_PORT           RGW listen port (default 7490 — 7480 is the compose ceph)
#
# Idempotent: every step is skipped when already done, so it can be rerun
# after a partial failure. Tear everything down with:
#   sudo snap remove --purge microceph
set -euo pipefail

MICROCEPH_CHANNEL=${MICROCEPH_CHANNEL:-tentacle/stable}
RGW_PORT=${RGW_PORT:-7490}
# the backend credentials the harness and integration tests default to
ACCESS_KEY=${S3RP_TEST_BACKEND_ACCESS_KEY_ID:-backendkey}
SECRET_KEY=${S3RP_TEST_BACKEND_SECRET_ACCESS_KEY:-backendsecret}

if [ "$(id -u)" -ne 0 ]; then
    echo "error: needs root (run with sudo)" >&2
    exit 1
fi

if snap list microceph >/dev/null 2>&1; then
    echo "==> microceph already installed: $(snap list microceph | awk 'NR==2 {print $2, "("$4")"}')"
else
    echo "==> installing microceph ($MICROCEPH_CHANNEL)"
    snap install microceph --channel="$MICROCEPH_CHANNEL"
fi

if microceph status >/dev/null 2>&1; then
    echo "==> cluster already bootstrapped"
else
    echo "==> bootstrapping cluster"
    microceph cluster bootstrap
fi

if [ "$(microceph.ceph osd stat -f json | sed 's/.*"num_osds":\([0-9]*\).*/\1/')" -gt 0 ]; then
    echo "==> OSDs already present"
else
    echo "==> adding loop-file OSDs"
    microceph disk add loop,4G,3
fi

if microceph status | grep -q rgw; then
    echo "==> rgw already enabled"
else
    echo "==> enabling rgw on port $RGW_PORT"
    microceph enable rgw --port "$RGW_PORT"
fi

echo "==> waiting for rgw to answer"
for _ in $(seq 1 60); do
    curl -sf -o /dev/null "http://127.0.0.1:$RGW_PORT/" && break
    sleep 2
done
curl -sf -o /dev/null "http://127.0.0.1:$RGW_PORT/" || {
    echo "error: rgw did not come up on port $RGW_PORT" >&2
    exit 1
}

if microceph.radosgw-admin user info --uid=backend >/dev/null 2>&1; then
    echo "==> backend user already exists"
else
    echo "==> creating the backend S3 user"
    microceph.radosgw-admin user create --uid=backend --display-name=backend \
        --access-key="$ACCESS_KEY" --secret-key="$SECRET_KEY" >/dev/null
fi

echo
echo "RGW ready: http://127.0.0.1:$RGW_PORT ($(microceph.ceph version))"
echo "run the suite against it with:"
echo "  S3RP_TEST_BACKEND_ENDPOINT=http://127.0.0.1:$RGW_PORT ./s3tests/run.sh"
