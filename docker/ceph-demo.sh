#!/bin/bash
# Single-container Ceph for development and CI: one mon, one mgr, one OSD
# (bluestore on a plain directory) and one RGW, plus an RGW admin user.
#
# This is a trimmed port of /opt/ceph-container/bin/demo from
# quay.io/ceph/demo, which stopped being published after squid (2024-12).
# It runs on the plain quay.io/ceph/ceph release image, so we can follow
# current Ceph releases (tentacle and later). Only the pieces s3rp needs are
# kept: no mds, nfs, rbd-mirror, restful or crash daemons.
#
# Environment (same names as the demo image):
#   MON_IP, CEPH_PUBLIC_NETWORK   mon address and public network
#   CEPH_DEMO_UID/ACCESS_KEY/SECRET_KEY   RGW admin user (caps buckets/users/usage/metadata)
#   RGW_FRONTEND_PORT             beast port (default 7480)
#
# State lives under /var/lib/ceph and /etc/ceph. A restart with the same
# volumes skips bootstrapping and only starts the daemons.
set -euo pipefail

: "${CLUSTER:=ceph}"
: "${MON_NAME:=demo}"
: "${MGR_NAME:=demo}"
: "${RGW_NAME:=demo}"
: "${MON_IP:?MON_IP must be set}"
: "${CEPH_PUBLIC_NETWORK:?CEPH_PUBLIC_NETWORK must be set}"
: "${MON_PORT:=3300}"
: "${CEPH_DEMO_UID:=demo}"
: "${CEPH_DEMO_ACCESS_KEY:?CEPH_DEMO_ACCESS_KEY must be set}"
: "${CEPH_DEMO_SECRET_KEY:?CEPH_DEMO_SECRET_KEY must be set}"
: "${RGW_FRONTEND_IP:=0.0.0.0}"
: "${RGW_FRONTEND_PORT:=7480}"
: "${RGW_ENABLE_USAGE_LOG:=true}"
: "${RGW_USAGE_MAX_USER_SHARDS:=1}"
: "${RGW_USAGE_MAX_SHARDS:=32}"
: "${RGW_USAGE_LOG_FLUSH_THRESHOLD:=1}"
: "${RGW_USAGE_LOG_TICK_INTERVAL:=1}"

CONF=/etc/ceph/${CLUSTER}.conf
ADMIN_KEYRING=/etc/ceph/${CLUSTER}.client.admin.keyring
MON_KEYRING=/etc/ceph/${CLUSTER}.mon.keyring
MONMAP=/etc/ceph/monmap-${CLUSTER}
MON_DATA_DIR=/var/lib/ceph/mon/${CLUSTER}-${MON_NAME}
MGR_PATH=/var/lib/ceph/mgr/${CLUSTER}-${MGR_NAME}
OSD_ID=0
OSD_PATH=/var/lib/ceph/osd/${CLUSTER}-${OSD_ID}
RGW_PATH=/var/lib/ceph/radosgw/${CLUSTER}-rgw.${RGW_NAME}
RGW_KEYRING=${RGW_PATH}/keyring
MARKER=/var/lib/ceph/I_AM_A_DEMO

# Daemons detach themselves (no -f); logs go to stderr, so `docker logs`
# shows everything.
DAEMON_OPTS=(--cluster "$CLUSTER" --setuser ceph --setgroup ceph
  --default-log-to-stderr=true --err-to-stderr=true --default-log-to-file=false)
CLI_OPTS=(--cluster "$CLUSTER")

log() { echo "$(date '+%F %T')  ceph-demo: $*"; }

write_conf() {
  local fsid
  fsid=$(uuidgen)
  cat >"$CONF" <<EOF
[global]
fsid = $fsid
mon initial members = ${MON_NAME}
mon host = v2:${MON_IP}:${MON_PORT}/0
public network = ${CEPH_PUBLIC_NETWORK}
cluster network = ${CEPH_PUBLIC_NETWORK}
auth_allow_insecure_global_id_reclaim = false
osd crush chooseleaf type = 0
osd pool default size = 1
osd pool default min size = 1
osd objectstore = bluestore
# bluestore on a directory inside the container, not a block device
bluestore block create = true
bluestore block size = 10737418240
osd max object name len = 256
osd max object namespace len = 64
mon allow pool size one = true
mon warn on pool no redundancy = false
mon data avail warn = 5

[osd.${OSD_ID}]
osd data = ${OSD_PATH}

[client.rgw.${RGW_NAME}]
rgw dns name = ${RGW_NAME}
rgw enable usage log = ${RGW_ENABLE_USAGE_LOG}
rgw usage log tick interval = ${RGW_USAGE_LOG_TICK_INTERVAL}
rgw usage log flush threshold = ${RGW_USAGE_LOG_FLUSH_THRESHOLD}
rgw usage max shards = ${RGW_USAGE_MAX_SHARDS}
rgw usage max user shards = ${RGW_USAGE_MAX_USER_SHARDS}
rgw frontends = beast endpoint=${RGW_FRONTEND_IP}:${RGW_FRONTEND_PORT}
EOF
}

bootstrap_mon() {
  write_conf
  local fsid
  fsid=$(awk '/^fsid/ {print $NF}' "$CONF")
  ceph-authtool "$ADMIN_KEYRING" --create-keyring --gen-key -n client.admin \
    --cap mon 'allow *' --cap osd 'allow *' --cap mds 'allow *' --cap mgr 'allow *'
  ceph-authtool "$MON_KEYRING" --create-keyring --gen-key -n mon. --cap mon 'allow *'
  ceph-authtool "$MON_KEYRING" --import-keyring "$ADMIN_KEYRING"
  monmaptool --create --add "$MON_NAME" "${MON_IP}:${MON_PORT}" --fsid "$fsid" "$MONMAP"
  mkdir -p "$MON_DATA_DIR"
  chown -R ceph:ceph /etc/ceph "$MON_DATA_DIR"
  ceph-mon --setuser ceph --setgroup ceph --cluster "$CLUSTER" --mkfs -i "$MON_NAME" \
    --inject-monmap "$MONMAP" --keyring "$MON_KEYRING" --mon-data "$MON_DATA_DIR"
  rm -f "$MONMAP"
}

start_mon() {
  ceph-mon "${DAEMON_OPTS[@]}" -i "$MON_NAME" --mon-data "$MON_DATA_DIR" --public-addr "$MON_IP"
  local i
  for i in $(seq 1 60); do
    if ceph "${CLI_OPTS[@]}" -s >/dev/null 2>&1; then return; fi
    sleep 1
  done
  log "ERROR: mon did not come up"
  exit 1
}

bootstrap_mgr() {
  mkdir -p "$MGR_PATH"
  ceph "${CLI_OPTS[@]}" auth get-or-create mgr."$MGR_NAME" \
    mon 'allow profile mgr' mds 'allow *' osd 'allow *' -o "$MGR_PATH"/keyring
  chown -R ceph:ceph "$MGR_PATH"
}

start_mgr() {
  ceph-mgr "${DAEMON_OPTS[@]}" -i "$MGR_NAME"
}

bootstrap_osd() {
  mkdir -p "$OSD_PATH"
  ceph "${CLI_OPTS[@]}" auth get-or-create osd."$OSD_ID" \
    mon 'allow profile osd' osd 'allow *' mgr 'allow profile osd' -o "$OSD_PATH"/keyring
  chown -R ceph:ceph "$OSD_PATH"
  ceph-osd --cluster "$CLUSTER" --setuser ceph --setgroup ceph --osd-data "$OSD_PATH" --mkfs -i "$OSD_ID"
  chown -R ceph:ceph "$OSD_PATH"
}

start_osd() {
  ceph-osd "${DAEMON_OPTS[@]}" -i "$OSD_ID"
  local i
  for i in $(seq 1 120); do
    if ceph "${CLI_OPTS[@]}" osd stat 2>/dev/null | grep -q '1 up'; then return; fi
    sleep 1
  done
  log "ERROR: osd.${OSD_ID} did not come up"
  exit 1
}

bootstrap_rgw() {
  mkdir -p "$RGW_PATH"
  ceph "${CLI_OPTS[@]}" auth get-or-create client.rgw."$RGW_NAME" \
    osd 'allow rwx' mon 'allow rw' -o "$RGW_KEYRING"
  chown -R ceph:ceph "$RGW_PATH"
}

start_rgw() {
  radosgw "${DAEMON_OPTS[@]}" -n client.rgw."$RGW_NAME" -k "$RGW_KEYRING"
  local i
  for i in $(seq 1 120); do
    if curl -sf "http://127.0.0.1:${RGW_FRONTEND_PORT}/" >/dev/null 2>&1; then return; fi
    sleep 1
  done
  log "ERROR: rgw did not answer on port ${RGW_FRONTEND_PORT}"
  exit 1
}

bootstrap_demo_user() {
  radosgw-admin "${CLI_OPTS[@]}" user create --uid="$CEPH_DEMO_UID" \
    --display-name="Ceph demo user" \
    --access-key="$CEPH_DEMO_ACCESS_KEY" --secret-key="$CEPH_DEMO_SECRET_KEY" >/dev/null
  radosgw-admin "${CLI_OPTS[@]}" caps add --uid="$CEPH_DEMO_UID" \
    --caps="buckets=*;users=*;usage=*;metadata=*" >/dev/null
}

if [ -e "$MARKER" ]; then
  log "existing demo cluster found, starting daemons"
  start_mon; start_mgr; start_osd; start_rgw
else
  log "bootstrapping a new demo cluster"
  bootstrap_mon; start_mon
  bootstrap_mgr; start_mgr
  bootstrap_osd; start_osd
  bootstrap_rgw; start_rgw
  bootstrap_demo_user
  touch "$MARKER"
fi
log "SUCCESS: RGW on ${RGW_FRONTEND_IP}:${RGW_FRONTEND_PORT}, admin user ${CEPH_DEMO_UID}"

# Stay in the foreground and forward the stop signal to the daemons.
stop() { local d; for d in radosgw ceph-osd ceph-mgr ceph-mon; do pkill -TERM -x "$d" 2>/dev/null || true; done; exit 0; }
trap stop TERM INT
while true; do
  sleep 3600 &
  wait $!
done
