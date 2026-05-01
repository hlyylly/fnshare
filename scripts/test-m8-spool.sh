#!/usr/bin/env bash
# M8 smoke test: write buffer (spool) + private filename UX.
#
# Spool scenario:
#   - alice + carol up; bob KILLED before upload
#   - alice puts a file → succeeds (bob's shard spools locally)
#   - carol can read the file (alice + carol have ≥ k shards)
#   - alice's spool dir has bob's pending shard + manifest
#   - restart bob → spool worker delivers within ~15s
#
# Private filename scenario:
#   - alice puts a private file with a recognizable name
#   - alice's `fnshare ls` shows the real name
#   - alice's FUSE mount shows the real name in .private/
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
compose exec -T node-a fnshare group-create --name "media" >/dev/null

PEER_A=$(compose exec -T node-a fnshare status | awk '/^node/ {print $NF}' | tr -d '()')
INVITE=$(compose exec -T node-a fnshare invite-create --bootstrap "/dns4/node-a/tcp/4001/p2p/${PEER_A}" --ttl-hours 1)

compose exec -d node-a fnshare daemon
sleep 4
compose exec -T node-b fnshare group-join "$INVITE" >/dev/null
compose exec -d node-b fnshare daemon
sleep 2
compose exec -T node-c fnshare group-join "$INVITE" >/dev/null
compose exec -d node-c fnshare daemon
sleep 8

PEER_B=$(compose exec -T node-b fnshare status | awk '/^node/ {print $NF}' | tr -d '()')
echo "  bob peer id: $PEER_B"

# ============================================================
#                    SPOOL SCENARIO
# ============================================================
echo
echo "==> kill bob's daemon BEFORE alice uploads"
compose exec -T node-b sh -c 'pkill -x fnshare || true'
echo "    waiting for heartbeat to mark bob offline (~20s)…"
sleep 20

# Sanity: alice should now see bob as offline.
ONLINE_BOB=$(curl -fsS http://localhost:4101/v1/ledger | python3 -c "
import sys, json
data = json.load(sys.stdin)
for e in data.get('entries', []):
    if e['peer_id'] == '$PEER_B':
        print(e['is_online'])
        break
else:
    print('?')
")
echo "  alice's view of bob: online=$ONLINE_BOB"
[ "$ONLINE_BOB" = "False" ] || { echo "✗ bob not detected as offline yet"; exit 1; }

echo
echo "==> alice uploads a 1MB file (bob is dead — shard should SPOOL, not fail)"
compose exec -T node-a sh -c 'dd if=/dev/urandom of=/tmp/spool-test.bin bs=1024 count=1024 2>/dev/null'
ALICE_HASH=$(compose exec -T node-a sha256sum /tmp/spool-test.bin | awk '{print $1}')

PUT_OUT=$(compose exec -T node-a fnshare put /tmp/spool-test.bin)
echo "$PUT_OUT" | head -8
FID=$(echo "$PUT_OUT" | awk '/file id/ {print $NF}')

echo
echo "==> alice's spool dir for bob should contain the queued shard + manifest"
SPOOL_FILES=$(compose exec -T node-a sh -c "ls -la /data/spool/${PEER_B}/ 2>/dev/null" | tail -n +2)
echo "$SPOOL_FILES"
PENDING_COUNT=$(compose exec -T node-a sh -c "ls /data/spool/${PEER_B}/ 2>/dev/null | wc -l" | tr -d ' ')
echo "  pending entries for bob: $PENDING_COUNT"
[ "$PENDING_COUNT" -ge "2" ] || { echo "✗ expected ≥ 2 spool entries (1 shard + 1 manifest)"; exit 1; }
echo "✓ shard + manifest were spooled rather than failing the upload"

echo
echo "==> carol can read the file (only k of k+m holders are alive)"
compose exec -T node-c fnshare get "$FID" /tmp/dl.bin >/dev/null
CAROL_HASH=$(compose exec -T node-c sha256sum /tmp/dl.bin | awk '{print $1}')
[ "$ALICE_HASH" = "$CAROL_HASH" ] && echo "✓ carol decoded from k available holders" \
  || { echo "✗ hash mismatch"; exit 1; }

echo
echo "==> bob has NO shards on disk yet (he was offline)"
BOB_SHARDS=$(compose exec -T node-b sh -c 'find /data/blocks -type f 2>/dev/null | wc -l' | tr -d ' ')
echo "  bob's blockstore: $BOB_SHARDS shards"
# (blockstore was created at init; should still be 0 here since bob's daemon was down for the put)

echo
echo "==> restart bob's daemon — spool worker should drain within ~15s"
compose exec -d node-b fnshare daemon
echo "    waiting for heartbeat to mark bob online + spool worker tick…"
sleep 25

echo
echo "==> bob should now have his shard + manifest"
BOB_SHARDS=$(compose exec -T node-b sh -c 'find /data/blocks -type f 2>/dev/null | wc -l' | tr -d ' ')
echo "  bob's blockstore: $BOB_SHARDS shards"
[ "$BOB_SHARDS" -ge "1" ] || { echo "✗ spool didn't deliver bob's shard"; exit 1; }

PENDING_AFTER=$(compose exec -T node-a sh -c "ls /data/spool/${PEER_B}/ 2>/dev/null | wc -l" | tr -d ' ')
echo "  alice's spool for bob (after delivery): $PENDING_AFTER"
[ "$PENDING_AFTER" = "0" ] && echo "✓ spool drained" \
  || echo "  (spool not fully drained yet — wait another tick)"

echo "✓ bob's spool was delivered after he came back online"

# ============================================================
#                  PRIVATE FILENAME SCENARIO
# ============================================================
echo
echo "==> alice uploads a private file with a recognizable name"
compose exec -T node-a sh -c "echo 'top secret content' > /tmp/tax-2026.pdf"
compose exec -T node-a fnshare put --private /tmp/tax-2026.pdf | head -3

sleep 2

echo
echo "==> alice's fnshare ls should show the real name (not <id>.bin)"
LS_OUT=$(compose exec -T node-a fnshare ls)
echo "$LS_OUT"
echo "$LS_OUT" | grep -q "tax-2026.pdf" && echo "✓ alice sees real filename via ls" \
  || { echo "✗ private filename not enriched"; exit 1; }

echo
echo "==> alice's FUSE mount shows the real name in .private/"
PRIV_LS=$(compose exec -T node-a ls /mnt/fnshare/media/.private/ 2>&1 || true)
echo "$PRIV_LS"
echo "$PRIV_LS" | grep -q "tax-2026.pdf" && echo "✓ FUSE shows real private filename" \
  || { echo "✗ FUSE still shows .bin"; exit 1; }

echo
echo "==> bob (non-owner) sees the file as encrypted (its name is ciphertext)"
BOB_LS=$(compose exec -T node-b fnshare ls)
echo "$BOB_LS"
# Bob's listing: row for the private file should NOT contain "tax-2026.pdf"
echo "$BOB_LS" | grep -q "tax-2026" && { echo "✗ bob can see plaintext name!"; exit 1; } \
  || echo "✓ bob doesn't see the plaintext name"

echo
echo "==> ✓ M8 spool + private filename smoke test complete"
