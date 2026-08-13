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
#   MON_IP             fixed mon address on a dummy interface (default 10.89.89.1)
#
# Idempotent: every step is skipped when already done, so it can be rerun
# after a partial failure. Tear everything down with:
#   sudo snap remove --purge microceph
set -euo pipefail

MICROCEPH_CHANNEL=${MICROCEPH_CHANNEL:-tentacle/stable}
RGW_PORT=${RGW_PORT:-7490}
MON_IP=${MON_IP:-10.89.89.1}
DUMMY_IF=ceph0
POOL_SIZE=${POOL_SIZE:-1}
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

# The mon address is sticky: a default bootstrap binds the mon to the
# current DHCP address, and when the host's address changes later, every
# client (and the RGW itself) hangs trying to reach it. MicroCeph refuses
# loopback for --mon-ip, so pin the mon to a fixed address on a dummy
# interface instead — independent of DHCP, docker bridges and tailscale.
if ip -4 addr show "$DUMMY_IF" 2>/dev/null | grep -q "inet $MON_IP/"; then
    echo "==> dummy interface $DUMMY_IF already has $MON_IP"
else
    echo "==> creating dummy interface $DUMMY_IF ($MON_IP)"
    ip link add "$DUMMY_IF" type dummy 2>/dev/null || true
    ip addr replace "$MON_IP/24" dev "$DUMMY_IF"
    ip link set "$DUMMY_IF" up
fi
# persist the interface across reboots (the running one was set up above,
# so netplan only needs to recreate it on boot — no `netplan apply` here)
if [ -d /etc/netplan ] && [ ! -e /etc/netplan/99-microceph-dummy.yaml ]; then
    echo "==> persisting $DUMMY_IF via netplan"
    cat >/etc/netplan/99-microceph-dummy.yaml <<EOF
network:
  version: 2
  dummy-devices:
    $DUMMY_IF:
      addresses: ["$MON_IP/24"]
EOF
    chmod 600 /etc/netplan/99-microceph-dummy.yaml
    netplan generate # validate only
fi

if microceph status >/dev/null 2>&1; then
    echo "==> cluster already bootstrapped"
else
    echo "==> bootstrapping cluster (mon pinned to $MON_IP)"
    microceph cluster bootstrap \
        --mon-ip "$MON_IP" --microceph-ip "$MON_IP" \
        --public-network "${MON_IP%.*}.0/24"
fi

# Replica 1: every OSD is a loop file on the same disk, so replication
# adds no durability — it just divides the usable capacity by 3 and burns
# CPU. Set the default before any data pool exists (rgw pools are created
# by `enable rgw` below), and shrink pools that already exist.
if [ "$(microceph.ceph config get osd osd_pool_default_size)" = "$POOL_SIZE" ]; then
    echo "==> osd_pool_default_size already $POOL_SIZE"
else
    echo "==> setting pool size to $POOL_SIZE"
    microceph.ceph config set global osd_pool_default_size "$POOL_SIZE"
    for p in $(microceph.ceph osd pool ls); do
        microceph.ceph osd pool set "$p" size "$POOL_SIZE" --yes-i-really-mean-it
    done
fi

# New pools start at pg_num 1 under the autoscaler, which cannot react
# within a benchmark burst — with size 1 that concentrates ALL rgw data
# on a single PG on a single OSD, which then fills and wedges the whole
# cluster read-only (observed: OSD_FULL with the other two OSDs near
# empty). Fix the PG count up front instead; must be set before
# `enable rgw` creates its pools.
if [ "$(microceph.ceph config get osd osd_pool_default_pg_autoscale_mode)" = "off" ]; then
    echo "==> pg autoscaler already off"
else
    echo "==> disabling pg autoscaler, defaulting new pools to 32 PGs"
    microceph.ceph config set global osd_pool_default_pg_autoscale_mode off
    microceph.ceph config set global osd_pool_default_pg_num 32
    microceph.ceph config set global osd_pool_default_pgp_num 32
fi

if [ "$(microceph.ceph osd stat -f json | sed 's/.*"num_osds":\([0-9]*\).*/\1/')" -gt 0 ]; then
    echo "==> OSDs already present"
else
    echo "==> adding loop-file OSDs"
    microceph disk add loop,12G,3
fi

if microceph status | grep -q rgw; then
    echo "==> rgw already enabled"
else
    echo "==> enabling rgw on port $RGW_PORT"
    microceph enable rgw --port "$RGW_PORT"
fi

wait_for_rgw() {
    for _ in $(seq 1 60); do
        curl -sf -o /dev/null "http://127.0.0.1:$RGW_PORT/" && return 0
        sleep 2
    done
    echo "error: rgw did not come up on port $RGW_PORT" >&2
    exit 1
}

# RGW options. MicroCeph's RGW runs as client.radosgw.gateway (not
# client.rgw.*), so they must target that name to apply.
# - SSE over plain http with the built-in "testing" KMS backend, mirroring
#   the compose ceph service so the SSE-KMS tests work without a real KMS.
# - Aggressive garbage collection: RGW deletes only queue space for
#   reclaim, and the defaults (2h object wait, 1h gc cycle) mean deleted
#   data never frees up within a test or benchmark run — which fills the
#   small loop OSDs and wedges the cluster read-only.
RGW_NAME=client.radosgw.gateway
rgw_conf_changed=0
set_rgw_conf() {
    if [ "$(microceph.ceph config get $RGW_NAME "$1" 2>/dev/null)" != "$2" ]; then
        microceph.ceph config set $RGW_NAME "$1" "$2"
        rgw_conf_changed=1
    fi
}
echo "==> configuring rgw (SSE testing KMS over plain http, fast gc)"
set_rgw_conf rgw_crypt_require_ssl false
set_rgw_conf rgw_crypt_s3_kms_backend testing
set_rgw_conf rgw_crypt_s3_kms_encryption_keys \
    testkey-1=YmluCmJvb3N0CmJvb3N0LWJ1aWxkCmNlcGguY29uZgo=
set_rgw_conf rgw_gc_obj_min_wait 10
set_rgw_conf rgw_gc_processor_period 60
# tcp_nodelay: without it, beast leaves Nagle on and every small-object
# GET response stalls ~40ms against the client's delayed ACK (measured:
# 16KiB GET 43ms -> 1.1ms). Loopback's huge MSS makes every small
# response a "small segment", so this bites hardest exactly here. The
# generated conf overrides the config db for rgw_frontends, so patch the
# file (it is not regenerated by snap restart).
RGW_CONF=/var/snap/microceph/current/conf/radosgw.conf
if grep -q 'tcp_nodelay=1' "$RGW_CONF"; then
    echo "==> beast tcp_nodelay already set"
else
    echo "==> enabling tcp_nodelay on the beast frontend"
    sed -i 's/^rgw frontends = beast /rgw frontends = beast tcp_nodelay=1 /' "$RGW_CONF"
    rgw_conf_changed=1
fi
if [ "$rgw_conf_changed" = 1 ]; then
    snap restart microceph.rgw
fi

echo "==> waiting for rgw to answer"
wait_for_rgw

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
