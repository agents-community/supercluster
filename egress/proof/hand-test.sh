#!/bin/sh
# Runs INSIDE the hand-like container. Proves the secretless-egress claim:
#   1. the sandbox holds NO secret (env + cred files are empty)
#   2. a call to a credential-protected endpoint succeeds WITHOUT the hand
#      sending any auth — the proxy injected it
#   3. a non-allowlisted host is denied (controlled egress)
#   4. git works through the proxy (TLS-terminated + trusted)
set -e

echo "== install client tooling =="
apk add --no-cache git curl ca-certificates >/dev/null

echo "== trust the proxy CA (internal CA the hand trusts) =="
cp /certs/mitmproxy-ca-cert.pem /usr/local/share/ca-certificates/egress-proxy.crt
update-ca-certificates >/dev/null

# route ALL egress through the proxy
export http_proxy="http://egress-proxy:8080"
export https_proxy="http://egress-proxy:8080"

echo "== proof 1: the sandbox holds no secret =="
echo "env secrets: $(env | grep -iE 'token|secret|password' || echo none)"
echo "cred files: $( (ls -a "$HOME" 2>/dev/null | grep -E '\.git-credentials|\.netrc') || echo none)"

echo "== proof 2: injection — we send NO auth, proxy injects it =="
curl -s https://httpbin.org/basic-auth/x-access-token/s3cr3t-from-vault
echo "-- what the destination actually received --"
curl -s https://httpbin.org/headers | grep -i authorization

echo "== proof 3: deny-by-default =="
curl -s -o /dev/null -w "example.com -> HTTP %{http_code}\n" https://example.com || true

echo "== proof 4: git clone through the proxy =="
git clone --depth 1 https://github.com/octocat/Hello-World /tmp/hw
ls /tmp/hw/README* && echo "clone OK"
