#!/usr/bin/env bash
# End-to-end smoke test for M1: 3 nodes form a group via invite link.
#
# M1 limitation: BadgerDB holds an exclusive directory lock, so CLI commands
# that touch the DB cannot run while `fnshare daemon` is up. We work around
# this by minting all invites BEFORE starting the admin daemon. M2 will add
# a daemon-side HTTP API so `status`/`invite-create` work concurrently.
set -euo pipefail

cd "$(dirname "$0")/.."

compose() { docker compose -f deploy/docker-compose.yml "$@"; }

echo "==> build & start 3-node cluster"
compose up -d --build >/dev/null

# Containers boot the daemon entrypoint, which fails (no group yet) and
# falls through to `sleep infinity`. Give them a sec to settle into that.
sleep 2

echo "==> init each node"
compose exec -T node-a fnshare init --nickname alice --contribute-gb 100
compose exec -T node-b fnshare init --nickname bob   --contribute-gb 100
compose exec -T node-c fnshare init --nickname carol --contribute-gb 100

echo "==> create group on node-a"
compose exec -T node-a fnshare group-create --name "test-circle"

PEER_A=$(compose exec -T node-a fnshare status | awk '/^node/ {print $NF}' | tr -d '()')
echo "==> node-a peer id: $PEER_A"

BOOTSTRAP="/dns4/node-a/tcp/4001/p2p/${PEER_A}"
INVITE=$(compose exec -T node-a fnshare invite-create --bootstrap "$BOOTSTRAP" --ttl-hours 1)
echo "==> invite link minted (len=${#INVITE})"

echo "==> start node-a daemon (admin, in background)"
compose exec -d node-a fnshare daemon
sleep 3   # let libp2p bind ports

echo "==> node-b joins via node-a"
compose exec -T node-b fnshare group-join "$INVITE"

echo "==> node-c joins via node-a"
compose exec -T node-c fnshare group-join "$INVITE"

# Stop the admin daemon so we can read status (lock conflict — see header).
# Use `pkill -x fnshare` (exact name match) so we don't accidentally match
# the entrypoint shell whose argv contains "fnshare daemon".
echo "==> stop node-a daemon to read status"
compose exec -T node-a sh -c 'pkill -x fnshare || true'
sleep 2

echo
echo "==> final status on each node"
for n in node-a node-b node-c; do
  echo "--- $n ---"
  compose exec -T "$n" fnshare status
done

echo
echo "==> ✓ smoke test complete"
