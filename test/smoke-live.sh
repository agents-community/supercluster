#!/usr/bin/env bash
# Live end-to-end smoke against a deployed agentplane. Scripts the exact flow
# a tester walks: auth -> create session -> send a message -> assert the agent
# replied and its reply came through the durable event log.
#
#   AGENTPLANE_URL=https://... AGENTPLANE_TOKEN=apl_... ./test/smoke-live.sh
#   AGENT=starter TURN_TIMEOUT=180 ./test/smoke-live.sh     # optional knobs
#
# Costs one real model turn. Exits non-zero on the first failure and always
# deletes the session it created (brain + hand + workspace).
set -euo pipefail

: "${AGENTPLANE_URL:?set AGENTPLANE_URL}"
: "${AGENTPLANE_TOKEN:?set AGENTPLANE_TOKEN}"
AGENT="${AGENT:-starter}"
TURN_TIMEOUT="${TURN_TIMEOUT:-180}"

auth=(-H "Authorization: Bearer $AGENTPLANE_TOKEN")
fail() { echo "FAIL  $*" >&2; exit 1; }
pass() { echo "PASS  $*"; }

sid=""
cleanup() {
  if [ -n "$sid" ]; then
    curl -fsS -X DELETE "${auth[@]}" "$AGENTPLANE_URL/v1/sessions/$sid" >/dev/null 2>&1 || true
    echo "cleaned up session $sid"
  fi
}
trap cleanup EXIT

echo "== 1. control plane is up =="
curl -fsS "$AGENTPLANE_URL/healthz" >/dev/null || fail "healthz"
pass "healthz"

echo "== 2. token is valid =="
curl -fsS "${auth[@]}" "$AGENTPLANE_URL/v1/agents" >/dev/null || fail "token rejected"
pass "authenticated agent list"

echo "== 3. agent template exists =="
curl -fsS "${auth[@]}" "$AGENTPLANE_URL/v1/agents" | grep -q "\"$AGENT\"" || fail "agent '$AGENT' not found"
pass "agent '$AGENT' present"

echo "== 4. create session =="
sid=$(curl -fsS -X POST "${auth[@]}" -H 'Content-Type: application/json' \
  -d "{\"agent\":\"$AGENT\"}" "$AGENTPLANE_URL/v1/sessions" | sed -n 's/.*"id":"\([^"]*\)".*/\1/p')
[ -n "$sid" ] || fail "no session id returned"
pass "session $sid"

echo "== 5. send a turn =="
curl -fsS -X POST "${auth[@]}" -H 'Content-Type: application/json' \
  -d '{"text":"Run exactly this and tell me the output: echo agentplane-smoke-$((6*7))"}' \
  "$AGENTPLANE_URL/v1/sessions/$sid/message" >/dev/null || fail "send"
pass "message accepted"

echo "== 6. agent replies (tool exec through the hand) =="
deadline=$(( $(date +%s) + TURN_TIMEOUT ))
while :; do
  events=$(curl -fsS "${auth[@]}" "$AGENTPLANE_URL/v1/sessions/$sid/message" || true)
  if echo "$events" | grep -q 'agentplane-smoke-42'; then
    pass "agent executed the command and reported the output"
    break
  fi
  [ "$(date +%s)" -lt "$deadline" ] || { echo "--- last events ---"; echo "$events" | tail -c 2000; fail "no reply within ${TURN_TIMEOUT}s"; }
  sleep 5
done

echo "== 7. event log is durable (replay returns the same turn) =="
curl -fsS "${auth[@]}" "$AGENTPLANE_URL/v1/sessions/$sid/message" | grep -q 'agentplane-smoke-42' \
  && pass "replay consistent" || fail "replay lost the turn"

echo
echo "smoke-live: ALL PASS"
