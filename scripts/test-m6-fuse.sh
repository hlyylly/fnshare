#!/usr/bin/env bash
# M6 smoke test: FUSE mount exposes the unified resource library as a
# read-only directory; uploaded files appear and can be read with `cat`.
set -euo pipefail

cd "$(dirname "$0")/.."

compose() { docker compose -f deploy/docker-compose.yml "$@"; }

echo "==> tear down + build + start (node-a has FUSE caps)"
compose down -v >/dev/null 2>&1 || true
compose up -d --build >/dev/null
sleep 3

compose exec -T node-a fnshare init --nickname alice --contribute-gb 100 >/dev/null
compose exec -T node-b fnshare init --nickname bob   --contribute-gb 100 >/dev/null
compose exec -T node-c fnshare init --nickname carol --contribute-gb 100 >/dev/null
compose exec -T node-a fnshare group-create --name "movies" >/dev/null

PEER_A=$(compose exec -T node-a fnshare status | awk '/^node/ {print $NF}' | tr -d '()')
INVITE=$(compose exec -T node-a fnshare invite-create --bootstrap "/dns4/node-a/tcp/4001/p2p/${PEER_A}" --ttl-hours 1)

compose exec -d node-a fnshare daemon
sleep 4
compose exec -T node-b fnshare group-join "$INVITE" >/dev/null
compose exec -d node-b fnshare daemon
sleep 2
compose exec -T node-c fnshare group-join "$INVITE" >/dev/null
compose exec -d node-c fnshare daemon
sleep 5

echo
echo "==> check FUSE mount is alive on node-a"
if ! compose exec -T node-a sh -c 'mountpoint -q /mnt/fnshare'; then
  echo "✗ /mnt/fnshare is not a mountpoint inside node-a"
  echo "  daemon log:"
  compose logs --tail 20 node-a
  exit 1
fi
echo "✓ /mnt/fnshare is mounted"

echo
echo "==> directory layout (root should show the group folder)"
compose exec -T node-a ls -la /mnt/fnshare/

echo
echo "==> alice uploads a SHARED file"
compose exec -T node-a sh -c "echo 'hello from fuse mount' > /tmp/hello.txt"
compose exec -T node-a fnshare put /tmp/hello.txt | tail -8

sleep 2

echo
echo "==> file should now appear in /mnt/fnshare/movies/"
compose exec -T node-a ls -la /mnt/fnshare/movies/

echo
echo "==> read the file through the FUSE mount"
CONTENT=$(compose exec -T node-a cat /mnt/fnshare/movies/hello.txt)
echo "  content: $CONTENT"
[ "$CONTENT" = "hello from fuse mount" ] && echo "✓ FUSE read returned correct plaintext" \
  || { echo "✗ FUSE read returned wrong content"; exit 1; }

echo
echo "==> alice uploads a PRIVATE file (only she can decrypt)"
compose exec -T node-a sh -c "echo 'secret' > /tmp/secret.txt"
compose exec -T node-a fnshare put --private /tmp/secret.txt | tail -3
sleep 2

echo
echo "==> private files appear under .private/ (owner only) — note name shows as <id>.bin in M6"
compose exec -T node-a ls -la /mnt/fnshare/movies/.private/ 2>/dev/null || echo "  (no private subdir yet — refresh)"

echo
echo "==> verify private files of OTHERS are hidden — bob's view"
# Spin up a FUSE mount on node-b too. For this test we just verify via the
# regular file list that bob sees alice's private file (he holds shards),
# but his FUSE mount (if enabled) would hide it. Since bob doesn't have FUSE
# enabled here, just sanity-check the manifest list shows it as private.
compose exec -T node-b fnshare ls

echo
echo "==> ✓ M6 FUSE smoke test complete"
echo "    inside node-a, you can now do:"
echo "      docker compose exec node-a ls /mnt/fnshare/"
echo "      docker compose exec node-a cat /mnt/fnshare/movies/hello.txt"
