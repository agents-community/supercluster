#!/usr/bin/env bash
# Orchestrates the local v1 proof end-to-end with Docker:
#   - starts the egress proxy (mitmproxy + inject.py) on a private network
#   - waits for its CA to be generated
#   - runs a hand-like alpine container that trusts the CA and routes egress
#     through the proxy, then executes hand-test.sh
#
# No secret ever enters the hand container — it lives only in policy.json,
# which is mounted into the PROXY.
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
NET=egress-proof-net
CERTS="$(mktemp -d)"

cleanup() {
  docker rm -f egress-proxy >/dev/null 2>&1 || true
  docker network rm "$NET" >/dev/null 2>&1 || true
  rm -rf "$CERTS"
}
trap cleanup EXIT

docker network create "$NET" >/dev/null 2>&1 || true

echo "== start egress proxy =="
docker run -d --name egress-proxy --network "$NET" \
  -v "$HERE/inject.py:/inject.py:ro" \
  -v "$HERE/policy.json:/policy.json:ro" \
  -v "$CERTS:/root/.mitmproxy" \
  mitmproxy/mitmproxy \
  mitmdump -s /inject.py --set confdir=/root/.mitmproxy >/dev/null

echo "== wait for proxy CA =="
for _ in $(seq 1 30); do
  [ -f "$CERTS/mitmproxy-ca-cert.pem" ] && break
  sleep 1
done
[ -f "$CERTS/mitmproxy-ca-cert.pem" ] || { echo "CA never appeared"; docker logs egress-proxy; exit 1; }

echo "== run hand (holds no secret) =="
docker run --rm --network "$NET" \
  -v "$CERTS:/certs:ro" \
  -v "$HERE/hand-test.sh:/hand-test.sh:ro" \
  alpine:3.20 sh /hand-test.sh

echo
echo "== proxy log =="
docker logs egress-proxy 2>&1 | grep -E 'INJECT|DENY|ALLOW' || true
