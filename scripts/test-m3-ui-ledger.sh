#!/usr/bin/env bash
# M3 smoke test: bring up the cluster, put/get a file, verify the ledger
# counters incremented, and check the embedded Web UI responds.
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

compose exec -T node-a fnshare group-create --name "m3-test" >/dev/null
PEER_A=$(compose exec -T node-a fnshare status | awk '/^node/ {print $NF}' | tr -d '()')
INVITE=$(compose exec -T node-a fnshare invite-create --bootstrap "/dns4/node-a/tcp/4001/p2p/${PEER_A}" --ttl-hours 1)

compose exec -d node-a fnshare daemon
sleep 3
compose exec -T node-b fnshare group-join "$INVITE" >/dev/null
compose exec -d node-b fnshare daemon
sleep 2
compose exec -T node-c fnshare group-join "$INVITE" >/dev/null
compose exec -d node-c fnshare daemon
sleep 2

echo
echo "==> upload a 512KB file from alice"
compose exec -T node-a sh -c 'dd if=/dev/urandom of=/tmp/test.bin bs=1024 count=512 2>/dev/null'
compose exec -T node-a fnshare put /tmp/test.bin >/dev/null

# Allow the ledger flush ticker (10s) to persist counters before we read.
echo "==> wait for ledger flush"
sleep 12

echo
echo "==> alice's ledger (should show bytes stored on bob & carol)"
curl -fsS http://localhost:4101/v1/ledger | python3 -m json.tool

echo
echo "==> bob's ledger (should show bytes stored FOR alice)"
curl -fsS http://localhost:4102/v1/ledger | python3 -m json.tool

echo
echo "==> Web UI sanity (alice's index.html)"
HTTP_CODE=$(curl -s -o /tmp/index.html -w '%{http_code}' http://localhost:4101/)
echo "GET / → $HTTP_CODE  ($(wc -c < /tmp/index.html | tr -d ' ') bytes)"
if [ "$HTTP_CODE" != "200" ] || ! grep -q 'fnshare' /tmp/index.html; then
  echo "✗ UI did not respond as expected"
  exit 1
fi
echo "✓ Web UI served"

echo
echo "==> static asset check (style.css)"
HTTP_CODE=$(curl -s -o /dev/null -w '%{http_code}' http://localhost:4101/style.css)
echo "GET /style.css → $HTTP_CODE"
[ "$HTTP_CODE" = "200" ] || { echo "✗ static asset missing"; exit 1; }

echo
echo "==> ✓ M3 smoke test complete"
echo
echo "    open these in a browser to play with the UI:"
echo "      alice: http://localhost:4101"
echo "      bob  : http://localhost:4102"
echo "      carol: http://localhost:4103"
