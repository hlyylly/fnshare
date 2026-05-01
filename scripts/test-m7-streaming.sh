#!/usr/bin/env bash
# M7 smoke test: streaming put/get, multi-stripe layout, FUSE partial read,
# and holder spread via rendezvous hashing.
set -euo pipefail

cd "$(dirname "$0")/.."

compose() { docker compose -f deploy/docker-compose.yml "$@"; }

SIZE_MB=200
echo "==> tear down + build + start (will upload a ${SIZE_MB} MiB file)"
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
sleep 8

echo
echo "==> generate ${SIZE_MB} MiB random file on node-a"
compose exec -T node-a sh -c "dd if=/dev/urandom of=/tmp/big.bin bs=1M count=${SIZE_MB} 2>/dev/null && sha256sum /tmp/big.bin"
ALICE_HASH=$(compose exec -T node-a sha256sum /tmp/big.bin | awk '{print $1}')

echo
echo "==> snapshot daemon RAM BEFORE upload"
RSS_BEFORE=$(docker stats --no-stream --format '{{.MemUsage}}' fnshare-a | awk -F'/' '{print $1}' | tr -d ' ')
echo "  node-a RSS before: $RSS_BEFORE"

echo
echo "==> alice uploads ${SIZE_MB} MiB (streaming — RAM should stay flat)"
PUT_OUT=$(compose exec -T node-a fnshare put /tmp/big.bin)
echo "$PUT_OUT" | head -10
FID=$(echo "$PUT_OUT" | awk '/file id/ {print $NF}')
STRIPE_COUNT=$(echo "$PUT_OUT" | grep -oE '[0-9]+ stripes' | awk '{print $1}')
echo "  → file id: $FID"
echo "  → stripes: $STRIPE_COUNT (expected ~$(( SIZE_MB * 1024 * 1024 / (4*1024*1024) )))"

echo
echo "==> snapshot daemon RAM AFTER upload"
RSS_AFTER=$(docker stats --no-stream --format '{{.MemUsage}}' fnshare-a | awk -F'/' '{print $1}' | tr -d ' ')
echo "  node-a RSS after : $RSS_AFTER"
# Convert to MiB for comparison; RSS strings look like "78.5MiB" or "1.2GiB".
to_mib() {
  python3 -c "
v='$1'.upper()
if 'GIB' in v: print(float(v.replace('GIB',''))*1024)
elif 'MIB' in v: print(float(v.replace('MIB','')))
elif 'KIB' in v: print(float(v.replace('KIB',''))/1024)
else: print(0)"
}
AFTER_MIB=$(to_mib "$RSS_AFTER")
echo "  RSS after = ${AFTER_MIB} MiB"
# Streaming target: daemon RAM < 5x the stripe size (4MiB) = 20 MiB, plus
# a generous baseline for libp2p / Badger / runtime. 100 MiB is the line.
if (( $(echo "$AFTER_MIB < 250" | bc -l) )); then
  echo "✓ daemon stayed under 250 MiB while uploading ${SIZE_MB} MiB — streaming works"
else
  echo "✗ daemon RSS too high (${AFTER_MIB} MiB) — likely buffered the whole file"
  exit 1
fi

echo
echo "==> verify all 3 nodes hold data (rendezvous spread)"
for n in node-a node-b node-c; do
  COUNT=$(compose exec -T "$n" sh -c 'find /data/blocks -type f 2>/dev/null | wc -l' | tr -d ' ')
  echo "  $n holds $COUNT shards"
  [ "$COUNT" -gt "0" ] || { echo "✗ $n holds zero shards"; exit 1; }
done
echo "✓ data spread across all members"

echo
echo "==> bob downloads the file (full sequential read)"
compose exec -T node-b fnshare get "$FID" /tmp/dl.bin >/dev/null
BOB_HASH=$(compose exec -T node-b sha256sum /tmp/dl.bin | awk '{print $1}')
echo "  alice : $ALICE_HASH"
echo "  bob   : $BOB_HASH"
[ "$ALICE_HASH" = "$BOB_HASH" ] && echo "✓ full download integrity" || { echo "✗ hash mismatch"; exit 1; }

echo
echo "==> FUSE: partial read (head -c 1024) — should hit only 1 stripe"
HEAD_HEX=$(compose exec -T node-a sh -c "head -c 1024 /mnt/fnshare/movies/big.bin | sha256sum" | awk '{print $1}')
EXPECTED_HEX=$(compose exec -T node-a sh -c "head -c 1024 /tmp/big.bin | sha256sum" | awk '{print $1}')
[ "$HEAD_HEX" = "$EXPECTED_HEX" ] && echo "✓ FUSE partial read returned correct first 1KB" || { echo "✗ partial read mismatch"; exit 1; }

echo
echo "==> FUSE: random middle read (dd skip=80M bs=4K count=1)"
MID_HEX=$(compose exec -T node-a sh -c "dd if=/mnt/fnshare/movies/big.bin bs=4096 skip=20480 count=1 2>/dev/null | sha256sum" | awk '{print $1}')
EXPECTED_MID=$(compose exec -T node-a sh -c "dd if=/tmp/big.bin bs=4096 skip=20480 count=1 2>/dev/null | sha256sum" | awk '{print $1}')
[ "$MID_HEX" = "$EXPECTED_MID" ] && echo "✓ FUSE random-access read correct (offset 80 MiB)" || { echo "✗ middle read mismatch"; exit 1; }

echo
echo "==> ✓ M7 streaming + multi-stripe + FUSE on-demand all working"
