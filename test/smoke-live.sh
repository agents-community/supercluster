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
  -d '{"message":"Run the shell command `hostname` and report its exact output. Do not guess."}' \
  "$AGENTPLANE_URL/v1/sessions/$sid/message" >/dev/null || fail "send"
pass "message accepted"

echo "== 6. agent replies (tool exec through the hand) =="
# The assertion must be something ONLY execution can produce, and it must prove
# the HAND executed it — not the brain.
#
# The previous check sent `echo agentplane-smoke-$((6*7))` and grepped the event
# log for `agentplane-smoke-42`. The shell expanded that to 42 in the MESSAGE, so
# the string was present the moment the message was accepted: step 6 passed
# without a single tool ever running, and hid a total hand outage for two days.
# An arithmetic answer is no better — the model computes it in its head.
#
# `hostname` inside the gVisor sandbox returns `runsc`, which the model cannot
# derive; and mcp__hand__ in a tool_use event proves it went through the hand
# rather than the brain's own denied builtins.
deadline=$(( $(date +%s) + TURN_TIMEOUT ))
while :; do
  events=$(curl -fsS "${auth[@]}" "$AGENTPLANE_URL/v1/sessions/$sid/message" || true)
  if echo "$events" | grep -q 'mcp__hand__'; then
    echo "$events" | grep -q 'runsc' \
      || fail "hand tool ran but the sandbox hostname is missing — did it execute?"
    pass "agent executed the command ON THE HAND and reported the output"
    break
  fi
  if echo "$events" | grep -qE '"name": *"Bash"'; then
    fail "the BRAIN executed locally (builtin Bash) — the hand was bypassed"
  fi
  [ "$(date +%s)" -lt "$deadline" ] || { echo "--- last events ---"; echo "$events" | tail -c 2000; fail "no reply within ${TURN_TIMEOUT}s"; }
  sleep 5
done

echo "== 7. event log is durable (replay returns the same turn) =="
# Replay must return the same durable turn: the hand tool call AND its output.
curl -fsS "${auth[@]}" "$AGENTPLANE_URL/v1/sessions/$sid/message" | grep -q 'mcp__hand__' \
  && pass "replay consistent" || fail "replay lost the turn"

echo
echo "smoke-live: ALL PASS"
