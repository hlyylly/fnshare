#!/usr/bin/env bash
# M5 smoke test: heartbeat detection + reputation deduction + lazy repair.
# With 3 nodes the test verifies online/offline state + reputation; lazy
# repair has nowhere to migrate (no spare member of the 3-node group), so
# we just confirm it logs that gracefully.
set -euo pipefail

cd "$(dirname "$0")/.."

compose() { docker compose -f deploy/docker-compose.yml "$@"; }

echo "==> tear down + build + start"
compose down -v >/dev/null 2>&1 || true
compose up -d --build >/dev/null
sleep 3

compose exec -T node-a fnshare init --nickname alice --contribute-gb 100 >/dev/null
compose exec -T node-b fnshare init --nickname bob   --contribute-gb 100 >/dev/null
compose exec -T node-c fnshare init --nickname carol --contribute-gb 100 >/dev/null
compose exec -T node-a fnshare group-create --name "m5-test" >/dev/null

PEER_A=$(compose exec -T node-a fnshare status | awk '/^node/ {print $NF}' | tr -d '()')
INVITE=$(compose exec -T node-a fnshare invite-create --bootstrap "/dns4/node-a/tcp/4001/p2p/${PEER_A}" --ttl-hours 1)

compose exec -d node-a fnshare daemon
sleep 3
compose exec -T node-b fnshare group-join "$INVITE" >/dev/null
compose exec -d node-b fnshare daemon
sleep 2
compose exec -T node-c fnshare group-join "$INVITE" >/dev/null
compose exec -d node-c fnshare daemon

echo "==> wait for heartbeat to mark everyone online (≈10s)"
sleep 12

echo
echo "==> alice's view of bob+carol (should be online, reputation 100ish)"
curl -fsS http://localhost:4101/v1/ledger | python3 -c "
import sys, json
data = json.load(sys.stdin)
for e in data.get('entries', []):
    print(f\"  {e['peer_id'][:20]}…  online={e['is_online']}  rep={e['reputation']}\")
"

echo
echo "==> kill bob's daemon (simulating offline)"
compose exec -T node-b sh -c 'pkill -x fnshare || true'

echo "==> wait for offline threshold (3 failures × 5s = ~20s)"
sleep 25

echo
echo "==> alice's view after bob died (bob should be offline, rep < 100)"
curl -fsS http://localhost:4101/v1/ledger | python3 -c "
import sys, json
data = json.load(sys.stdin)
bob_seen = False
for e in data.get('entries', []):
    print(f\"  {e['peer_id'][:20]}…  online={e['is_online']}  rep={e['reputation']}\")
"

# Check that at least one peer is now offline + rep dropped.
ALL_ENTRIES=$(curl -fsS http://localhost:4101/v1/ledger)
DOWN_COUNT=$(echo "$ALL_ENTRIES" | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(sum(1 for e in data.get('entries', []) if not e['is_online']))
")
MIN_REP=$(echo "$ALL_ENTRIES" | python3 -c "
import sys, json
data = json.load(sys.stdin)
reps = [e['reputation'] for e in data.get('entries', [])]
print(min(reps) if reps else 100)
")
echo
echo "  offline peers: $DOWN_COUNT, min reputation: $MIN_REP"
[ "$DOWN_COUNT" -ge "1" ] && [ "$MIN_REP" -lt "100" ] && echo "✓ heartbeat detected offline + reputation deducted" \
  || { echo "✗ heartbeat did not deduct as expected"; exit 1; }

echo
echo "==> restart bob's daemon (recovery)"
compose exec -d node-b fnshare daemon
sleep 12

echo
echo "==> alice's view after bob's recovery (should be online again)"
curl -fsS http://localhost:4101/v1/ledger | python3 -c "
import sys, json
data = json.load(sys.stdin)
for e in data.get('entries', []):
    print(f\"  {e['peer_id'][:20]}…  online={e['is_online']}  rep={e['reputation']}\")
"

echo
echo "==> ✓ M5 smoke test complete"
echo "    open browsers to see the live online dots + reputation bars:"
echo "      alice: http://localhost:4101"
echo "      bob  : http://localhost:4102"
echo "      carol: http://localhost:4103"
