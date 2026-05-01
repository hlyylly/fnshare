#!/usr/bin/env bash
# M4 smoke test: shared + private encryption end-to-end.
# Verifies: shards on holders' disks contain NO plaintext markers.
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

compose exec -T node-a fnshare group-create --name "m4-test" >/dev/null
PEER_A=$(compose exec -T node-a fnshare status | awk '/^node/ {print $NF}' | tr -d '()')
INVITE=$(compose exec -T node-a fnshare invite-create --bootstrap "/dns4/node-a/tcp/4001/p2p/${PEER_A}" --ttl-hours 1)

compose exec -d node-a fnshare daemon
sleep 3
compose exec -T node-b fnshare group-join "$INVITE" >/dev/null
compose exec -d node-b fnshare daemon
sleep 2
compose exec -T node-c fnshare group-join "$INVITE" >/dev/null
compose exec -d node-c fnshare daemon
sleep 8  # let peer-discovery converge

# A distinctive plaintext marker we will grep for in raw shards.
MARKER="FNSHARE-PLAINTEXT-MARKER-$(date +%s)"

# ---- shared mode ----
echo
echo "==> SHARED upload from alice (plaintext contains '$MARKER')"
compose exec -T node-a sh -c "echo '$MARKER content for shared mode' > /tmp/shared.txt && fnshare put /tmp/shared.txt" \
  | tee /tmp/shared-out.txt
SHARED_FID=$(awk '/file id/ {print $NF}' /tmp/shared-out.txt)

sleep 2
echo
echo "==> bob downloads shared file → expect plaintext marker"
compose exec -T node-b fnshare get "$SHARED_FID" /tmp/shared-bob.txt >/dev/null
DECRYPTED=$(compose exec -T node-b cat /tmp/shared-bob.txt)
echo "  bob sees: $DECRYPTED"
echo "$DECRYPTED" | grep -q "$MARKER" && echo "✓ shared mode: bob decrypted correctly" || { echo "✗ marker missing"; exit 1; }

# ---- private mode ----
echo
echo "==> PRIVATE upload from alice (with --private)"
compose exec -T node-a sh -c "echo '$MARKER content PRIVATE' > /tmp/priv.txt && fnshare put --private /tmp/priv.txt" \
  | tee /tmp/priv-out.txt
PRIV_FID=$(awk '/file id/ {print $NF}' /tmp/priv-out.txt)

sleep 2
echo
echo "==> alice downloads her own private file → should succeed"
compose exec -T node-a fnshare get "$PRIV_FID" /tmp/priv-alice.txt >/dev/null
DECRYPTED=$(compose exec -T node-a cat /tmp/priv-alice.txt)
echo "  alice sees: $DECRYPTED"
echo "$DECRYPTED" | grep -q "$MARKER" || { echo "✗ alice can't read her own private file"; exit 1; }
echo "✓ private mode: owner can decrypt"

echo
echo "==> bob tries to download alice's private file → should FAIL"
# Capture output separately so pipefail doesn't mask the grep result.
BOB_OUT=$(compose exec -T node-b fnshare get "$PRIV_FID" /tmp/priv-bob.txt 2>&1 || true)
if echo "$BOB_OUT" | grep -q "private file: only the owner"; then
  echo "✓ private mode: non-owner refused"
else
  echo "✗ bob was NOT refused — privacy broken!"
  echo "  output was: $BOB_OUT"
  exit 1
fi

# ---- holder cannot read raw shards ----
echo
echo "==> grep all on-disk shards across all 3 nodes for the plaintext marker"
FOUND=0
for n in node-a node-b node-c; do
  if compose exec -T "$n" sh -c "grep -ral '$MARKER' /data/blocks 2>/dev/null" | grep -q .; then
    echo "  ✗ found plaintext on $n!"
    FOUND=$((FOUND+1))
  else
    echo "  ✓ $n: no plaintext leak"
  fi
done
[ "$FOUND" = "0" ] || { echo "✗ encryption leaked plaintext to disk"; exit 1; }

echo
echo "==> ✓ M4 smoke test complete — shards are end-to-end encrypted"
