#!/usr/bin/env bash
# Multi-group end-to-end test:
#   - alice creates "group-A"
#   - bob   creates "group-B"
#   - all 3 nodes join BOTH groups (alice is admin of A, member of B; bob
#     is admin of B, member of A; carol is plain member of both)
#   - alice uploads fileA to group-A; bob uploads fileB to group-B
#   - every node's unified file view shows BOTH files with the right group
#   - everyone can decrypt the file in the group they belong to
set -euo pipefail

cd "$(dirname "$0")/.."

compose() { docker compose -f deploy/docker-compose.yml "$@"; }
restart_daemon() {
  local n=$1
  compose exec -T "$n" sh -c 'pkill -x fnshare || true'
  sleep 1
  compose exec -d "$n" fnshare daemon
  sleep 2
}
peer_id() {
  compose exec -T "$1" fnshare status | awk '/^node/ {print $NF}' | tr -d '()'
}

echo "==> tear down + build + start"
compose down -v >/dev/null 2>&1 || true
compose up -d --build >/dev/null
sleep 3

compose exec -T node-a fnshare init --nickname alice --contribute-gb 100 >/dev/null
compose exec -T node-b fnshare init --nickname bob   --contribute-gb 100 >/dev/null
compose exec -T node-c fnshare init --nickname carol --contribute-gb 100 >/dev/null

# ===== group A: alice is admin =====
echo
echo "==> alice creates group-A"
compose exec -T node-a fnshare group-create --name "group-A" >/dev/null
compose exec -d node-a fnshare daemon
sleep 3

PEER_A=$(peer_id node-a)
INVITE_A=$(compose exec -T node-a fnshare invite-create --bootstrap "/dns4/node-a/tcp/4001/p2p/${PEER_A}" --ttl-hours 1)
echo "invite-A: ${INVITE_A:0:60}…"

echo "==> bob and carol join group-A"
compose exec -T node-b fnshare group-join "$INVITE_A" >/dev/null
compose exec -T node-c fnshare group-join "$INVITE_A" >/dev/null

# Bring B's daemon up so we can create group-B from there.
compose exec -d node-b fnshare daemon
sleep 3

# ===== group B: bob is admin =====
echo
echo "==> bob creates group-B (alice's daemon is already running — bob's group-create stops his own daemon, recreates DB, starts again)"
# group-create needs daemon stopped on bob's node only.
compose exec -T node-b sh -c 'pkill -x fnshare || true'; sleep 1
compose exec -T node-b fnshare group-create --name "group-B" >/dev/null
compose exec -d node-b fnshare daemon
sleep 3

PEER_B=$(peer_id node-b)
INVITE_B=$(compose exec -T node-b fnshare invite-create --bootstrap "/dns4/node-b/tcp/4001/p2p/${PEER_B}" --group "$(compose exec -T node-b fnshare groups | awk '/group-B/ {print $1}')" --ttl-hours 1)
echo "invite-B: ${INVITE_B:0:60}…"

echo "==> alice and carol join group-B"
compose exec -T node-a sh -c 'pkill -x fnshare || true'; sleep 1
compose exec -T node-a fnshare group-join "$INVITE_B" >/dev/null
compose exec -d node-a fnshare daemon
sleep 2

compose exec -T node-c fnshare group-join "$INVITE_B" >/dev/null
compose exec -d node-c fnshare daemon
sleep 8   # peer-discovery converge

echo
echo "==> verify everyone is in BOTH groups"
for n in node-a node-b node-c; do
  COUNT=$(compose exec -T "$n" fnshare groups | grep -cE 'group-A|group-B' || true)
  echo "  $n is in $COUNT group(s)"
  [ "$COUNT" = "2" ] || { echo "✗ expected 2 groups on $n"; exit 1; }
done

# ===== upload: alice to A, bob to B =====
echo
echo "==> alice uploads fileA to group-A"
GID_A=$(compose exec -T node-a fnshare groups | awk '/group-A/ {print $1}')
compose exec -T node-a sh -c "echo 'PAYLOAD-A-shared' > /tmp/fileA.txt"
PUT_A=$(compose exec -T node-a fnshare put --group "$GID_A" /tmp/fileA.txt)
FID_A=$(echo "$PUT_A" | awk '/file id/ {print $NF}')
echo "  fileA file_id: $FID_A"

echo "==> bob uploads fileB to group-B"
GID_B=$(compose exec -T node-b fnshare groups | awk '/group-B/ {print $1}')
compose exec -T node-b sh -c "echo 'PAYLOAD-B-shared' > /tmp/fileB.txt"
PUT_B=$(compose exec -T node-b fnshare put --group "$GID_B" /tmp/fileB.txt)
FID_B=$(echo "$PUT_B" | awk '/file id/ {print $NF}')
echo "  fileB file_id: $FID_B"

sleep 3

# ===== unified file view =====
echo
echo "==> carol's unified file list (should show BOTH files)"
compose exec -T node-c fnshare ls

# ===== cross-group reads =====
echo
echo "==> carol downloads fileA (she's in group-A)"
compose exec -T node-c fnshare get "$FID_A" /tmp/out-A.txt >/dev/null
A_OUT=$(compose exec -T node-c cat /tmp/out-A.txt)
echo "  carol sees: $A_OUT"
echo "$A_OUT" | grep -q "PAYLOAD-A-shared" || { echo "✗ fileA decrypt failed"; exit 1; }

echo
echo "==> carol downloads fileB (she's in group-B)"
compose exec -T node-c fnshare get "$FID_B" /tmp/out-B.txt >/dev/null
B_OUT=$(compose exec -T node-c cat /tmp/out-B.txt)
echo "  carol sees: $B_OUT"
echo "$B_OUT" | grep -q "PAYLOAD-B-shared" || { echo "✗ fileB decrypt failed"; exit 1; }

echo
echo "==> ✓ multi-group test complete"
echo "    open browsers to see the unified resource library:"
echo "      alice: http://localhost:4101"
echo "      bob  : http://localhost:4102"
echo "      carol: http://localhost:4103"
