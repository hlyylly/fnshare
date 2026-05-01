#!/usr/bin/env bash
# M2 smoke test: distribute a file via Reed-Solomon EC and read it back
# from a different node.
set -euo pipefail

cd "$(dirname "$0")/.."

compose() { docker compose -f deploy/docker-compose.yml "$@"; }

echo "==> tear down + build + start"
compose down -v >/dev/null 2>&1 || true
compose up -d --build >/dev/null
sleep 3

echo "==> init each node"
compose exec -T node-a fnshare init --nickname alice --contribute-gb 100 >/dev/null
compose exec -T node-b fnshare init --nickname bob   --contribute-gb 100 >/dev/null
compose exec -T node-c fnshare init --nickname carol --contribute-gb 100 >/dev/null

echo "==> create group on alice (admin)"
compose exec -T node-a fnshare group-create --name "m2-test" >/dev/null

PEER_A=$(compose exec -T node-a fnshare status | awk '/^node/ {print $NF}' | tr -d '()')
INVITE=$(compose exec -T node-a fnshare invite-create --bootstrap "/dns4/node-a/tcp/4001/p2p/${PEER_A}" --ttl-hours 1)

echo "==> start alice daemon"
compose exec -d node-a fnshare daemon
sleep 3

echo "==> bob joins, then starts daemon"
compose exec -T node-b fnshare group-join "$INVITE" >/dev/null
compose exec -d node-b fnshare daemon
sleep 2

echo "==> carol joins, then starts daemon"
compose exec -T node-c fnshare group-join "$INVITE" >/dev/null
compose exec -d node-c fnshare daemon
sleep 2

echo "==> generate 1 MiB random test file"
compose exec -T node-a sh -c 'dd if=/dev/urandom of=/tmp/test.bin bs=1024 count=1024 2>/dev/null && sha256sum /tmp/test.bin'

echo
echo "==> alice: fnshare put /tmp/test.bin"
PUT_OUTPUT=$(compose exec -T node-a fnshare put /tmp/test.bin)
echo "$PUT_OUTPUT"
FILE_ID=$(echo "$PUT_OUTPUT" | awk '/file id/ {print $NF}')
echo "FILE_ID=$FILE_ID"

# Give alice a moment to finish replicating manifest to all holders.
sleep 2

echo
echo "==> bob: fnshare ls"
compose exec -T node-b fnshare ls || true

echo
echo "==> bob: fnshare get $FILE_ID /tmp/out-bob.bin"
compose exec -T node-b fnshare get "$FILE_ID" /tmp/out-bob.bin

echo
echo "==> verify bob's copy matches"
ALICE_HASH=$(compose exec -T node-a sha256sum /tmp/test.bin | awk '{print $1}')
BOB_HASH=$(compose exec -T node-b sha256sum /tmp/out-bob.bin | awk '{print $1}')
echo "alice : $ALICE_HASH"
echo "bob   : $BOB_HASH"
if [ "$ALICE_HASH" = "$BOB_HASH" ]; then
  echo "✓ hashes match — file reconstructed correctly"
else
  echo "✗ HASH MISMATCH"
  exit 1
fi

echo
echo "==> carol: fnshare get $FILE_ID /tmp/out-carol.bin"
compose exec -T node-c fnshare get "$FILE_ID" /tmp/out-carol.bin
CAROL_HASH=$(compose exec -T node-c sha256sum /tmp/out-carol.bin | awk '{print $1}')
echo "carol : $CAROL_HASH"
if [ "$ALICE_HASH" = "$CAROL_HASH" ]; then
  echo "✓ carol's copy also matches"
else
  echo "✗ carol mismatch"
  exit 1
fi

echo
echo "==> wait for peer-discovery ticker to converge (everyone knows everyone)"
sleep 10

echo "==> kill alice and verify bob+carol can still reconstruct (EC tolerance)"
compose exec -T node-a sh -c 'pkill -x fnshare || true'
sleep 5   # let dead-peer detection settle

# Bob already has the file cached; clean it so we force a real refetch.
compose exec -T node-b rm -f /tmp/out-bob.bin

echo "==> bob: fnshare get $FILE_ID /tmp/out-bob2.bin (alice is dead)"
if compose exec -T node-b fnshare get "$FILE_ID" /tmp/out-bob2.bin; then
  BOB2_HASH=$(compose exec -T node-b sha256sum /tmp/out-bob2.bin | awk '{print $1}')
  if [ "$ALICE_HASH" = "$BOB2_HASH" ]; then
    echo "✓ bob recovered file from carol (alice down) — EC tolerance works"
  else
    echo "✗ post-kill hash mismatch"; exit 1
  fi
else
  echo "✗ bob could not recover after alice died — peer discovery may not have fired"
  echo "  (this is M2's known limitation if BootstrapGroupPeers had no time to run)"
  exit 1
fi

echo
echo "==> ✓ M2 smoke test complete"
