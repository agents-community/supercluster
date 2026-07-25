#!/usr/bin/env bash
# Integration suite — named cases, per-test PASS/FAIL/SKIP, summary table.
#
#   ./test/smoke.sh                 run the full suite
#   ./test/smoke.sh t_memory …     run only the named case(s) (deps auto-skip)
#
# Needs: live cluster, brain template Ready, port-forwards, AGENTPLANE_BUCKET.
# Destructive drills (wedge/pod-death) are NOT here — see the runbook.
set -uo pipefail
AP=${AP:-/tmp/agentplane}
: "${AGENTPLANE_BUCKET:?set AGENTPLANE_BUCKET}"

SID=""            # shared session, created by t_create
RESULTS=()
ANY_FAIL=0

# ---------- helpers ----------
wait_idle() { # $1=expected idle-count $2=max-tries — turns close on status_idle
  for _ in $(seq 1 "$2"); do
    sleep 4
    n=$($AP session events -id "$SID" 2>/dev/null | grep -c "session.status_idle" || true)
    [ "$n" -ge "$1" ] && return 0
  done
  return 1
}

need_session() { [ -n "$SID" ] || { echo "  (no session — dependency failed)"; return 1; }; }

# ---------- cases ----------
t_create() {
  SID=$($AP session new | awk '/^session:/{print $2}')
  [ -n "$SID" ] || return 1
  echo "  session: $SID"
}

t_turn_completes() {
  need_session || return 2
  $AP session send -id "$SID" -m "Remember the codeword LANTERN-9. Reply only: stored." >/dev/null || return 1
  wait_idle 1 15
}

t_event_log() {
  need_session || return 2
  out=$($AP session events -id "$SID")
  echo "$out" | grep -q "user.message" && echo "$out" | grep -q "stored"
}

t_memory_across_checkpoint() {
  need_session || return 2
  $AP session suspend -id "$SID" >/dev/null || return 1
  $AP session send -id "$SID" -m "Codeword? Reply only the codeword." >/dev/null || return 1
  wait_idle 2 15 || return 1
  $AP session events -id "$SID" | grep -q "LANTERN-9"
}

t_status_derived() {
  need_session || return 2
  $AP session list | awk -v s="$SID" '$1==s' | grep -qE "sleeping|idle|running"  # any known derived state
}

t_escrow_on_delete() {
  need_session || return 2
  $AP session delete -id "$SID" >/dev/null || return 1
  gcloud storage ls "gs://$AGENTPLANE_BUCKET/transcripts/$SID/" >/dev/null 2>&1
}

t_cascade_snapshots_removed() {
  need_session || return 2
  ! gcloud storage ls "gs://$AGENTPLANE_BUCKET/agentplane/b-$SID/" >/dev/null 2>&1
}

# serve API round-trip against the shared session (no extra session cost).
# Boots a private serve on :7455; killed in teardown.
t_serve_api() {
  need_session || return 2
  AGENTPLANE_TOKEN=smoke-token "$AP" serve -addr :7455 >/dev/null 2>&1 & SERVE_PID=$!
  sleep 2
  local a="Authorization: Bearer smoke-token"
  curl -sfS http://localhost:7455/healthz >/dev/null || return 1
  curl -sS http://localhost:7455/v1/agents | grep -q '"error"' || return 1   # 401 without token
  curl -sfS -H "$a" http://localhost:7455/v1/agents | grep -q '"name"' || return 1
  curl -sfS -H "$a" "http://localhost:7455/v1/sessions/$SID" | grep -q '"status"' || return 1
  curl -sfS -H "$a" "http://localhost:7455/v1/sessions/$SID/events" | grep -q user.message
}

ALL_TESTS=(t_create t_turn_completes t_event_log t_memory_across_checkpoint \
           t_status_derived t_serve_api t_escrow_on_delete t_cascade_snapshots_removed)

# ---------- runner ----------
run_one() {
  local name=$1 rc
  echo "── $name"
  "$name"; rc=$?
  case $rc in
    0) RESULTS+=("PASS  $name") ;;
    2) RESULTS+=("SKIP  $name") ;;
    *) RESULTS+=("FAIL  $name"); ANY_FAIL=1 ;;
  esac
}

SERVE_PID=""
cleanup() {
  [ -n "$SERVE_PID" ] && kill "$SERVE_PID" >/dev/null 2>&1
  [ -n "$SID" ] && $AP session delete -id "$SID" -force >/dev/null 2>&1 || true
}
trap cleanup EXIT

TESTS=("${@:-}")
[ -z "${TESTS[0]:-}" ] && TESTS=("${ALL_TESTS[@]}")
# ad-hoc runs still need a session for dependent cases
if [ "${TESTS[0]}" != "t_create" ]; then TESTS=(t_create "${TESTS[@]}"); fi

for t in "${TESTS[@]}"; do run_one "$t"; done
# cleanup() is idempotent — double-delete after the cascade cases is a no-op.

echo ""
echo "════════ suite summary ════════"
printf '%s\n' "${RESULTS[@]}"
[ "$ANY_FAIL" = 0 ] && echo "SUITE: ALL PASS" || { echo "SUITE: FAILURES"; exit 1; }
