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
chmod 777 "$CERTS"   # the mitmproxy image runs as a non-root user

echo "== start egress proxy =="
docker run -d --name egress-proxy --network "$NET" \
  -v "$HERE/inject.py:/inject.py:ro" \
  -v "$HERE/policy.json:/policy.json:ro" \
  -v "$CERTS:/home/mitmproxy/.mitmproxy" \
  mitmproxy/mitmproxy \
  mitmdump -s /inject.py >/dev/null

echo "== wait for proxy CA =="
for _ in $(seq 1 30); do
  [ -f "$CERTS/mitmproxy-ca-cert.pem" ] && break
  sleep 1
done
[ -f "$CERTS/mitmproxy-ca-cert.pem" ] || { echo "CA never appeared"; docker logs egress-proxy; exit 1; }

echo "== run hand (holds no secret) =="
OUT="$CERTS/hand-out.log"
docker run --rm --network "$NET" \
  -v "$CERTS:/certs:ro" \
  -v "$HERE/hand-test.sh:/hand-test.sh:ro" \
  alpine:3.20 sh /hand-test.sh | tee "$OUT"

echo
echo "== proxy log =="
docker logs egress-proxy 2>&1 | grep -E 'INJECT|DENY|ALLOW' | tee "$CERTS/proxy.log" || true

echo
echo "== assertions =="
failures=0
assert() { # name, file, pattern
  if grep -q "$3" "$2"; then echo "PASS  $1"; else echo "FAIL  $1"; failures=$((failures+1)); fi
}
assert "hand env holds no secret"        "$OUT" 'env secrets: none'
assert "hand has no credential files"    "$OUT" 'cred files: none'
assert "proxy injected the credential"   "$OUT" '"authenticated": true'
assert "upstream saw the injected auth"  "$OUT" 'Authorization.*Basic'
assert "non-allowlisted host is denied"  "$OUT" 'example.com -> HTTP 403'
assert "git clone works through proxy"   "$OUT" 'clone OK'
assert "proxy log shows the injection"   "$CERTS/proxy.log" 'INJECT basic -> httpbin.org'
assert "proxy log shows the deny"        "$CERTS/proxy.log" 'DENY example.com'

if [ "$failures" -gt 0 ]; then echo; echo "egress proof: $failures FAILURE(S)"; exit 1; fi
echo
echo "egress proof: ALL PASS"
