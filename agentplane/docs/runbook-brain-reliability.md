# Runbook — brain durability & checkpointing tests

Scope (decided 2026-07-19): brain/harness reliability ONLY — hands deferred.
Run these drills after any brain-server or template change. Each drill has an
expected result; a deviation is a finding — record it in this file's log.

## Prereqs
```bash
export PATH="$HOME/.local/bin:$HOME/go/bin:$PATH"
# port-forwards (skip if already up):
kubectl port-forward -n ate-system svc/api 8080:443 &
kubectl port-forward -n ate-system svc/atenet-router 8000:80 &
# template Ready:
kubectl get actortemplate brain -n agentplane -o jsonpath='{.status.phase}'   # → Ready
# CLI:
cd ~/code_vault/agentplane && go build -o /tmp/agentplane ./cmd/agentplane
AP=/tmp/agentplane
```

## Automated: `test/smoke.sh` (run this first)
Named-case suite, ~2min, live cluster — 8 cases with per-test PASS/FAIL/SKIP
and a summary table: create, turn-completes, event-log, memory-across-
checkpoint, derived-status, serve-API round-trip, escrow-on-delete,
cascade-snapshots-removed (`AGENTPLANE_BUCKET` required). Run one case:
`./test/smoke.sh t_memory_across_checkpoint`. Unit tests: `go test ./...`.
The destructive drills below (T4–T6) stay manual by design.

## Observability
`serve` is the trace root (OTLP via `OTEL_EXPORTER_OTLP_ENDPOINT` — full
setup incl. the atenet collector repoint in docs/api.md). Quick look:
`kubectl port-forward -n otel-system svc/jaeger 16686:16686` → Jaeger UI.

## T1 — lifecycle smoke
```bash
SID=$($AP session new | awk '/^session:/{print $2}'); echo $SID
$AP session send -id $SID -m "Reply exactly: ALIVE"
$AP session tail -id $SID -for 25s          # expect: user.message → status_running → agent.message ALIVE → status_idle
$AP session suspend -id $SID                # expect: "suspended (mind checkpointed)"
$AP session list                            # expect: SID SUSPENDED
```
PASS = all four events, idle before suspend, list shows SUSPENDED.

## T2 — memory continuity across checkpoint (the core claim)
```bash
$AP session send -id $SID -m "Remember codeword ZEBRA-42. Reply only: stored."
$AP session tail -id $SID -for 25s | grep -q stored && echo taught
$AP session suspend -id $SID                                  # checkpoint
$AP session send -id $SID -m "Codeword? Reply only the codeword."   # auto-resume
$AP session tail -id $SID -for 20s | grep ZEBRA-42 && echo "PASS: memory survived"
```
PASS = recall after suspend; warm-turn latency ≈1–3s (vs ~9s first-ever turn).

## T3 — event-log cursors
```bash
curl -sS "http://localhost:8000/v1/sessions/b-$SID/events" -H "Host: b-$SID.agents.actors.resources.substrate.ate.dev" \
  | python3 -c 'import json,sys;print(len(json.load(sys.stdin)["events"]),"events")'
$AP session tail -id $SID -since evt_000004 -for 5s     # expect: only events > 4
```
PASS = cursor filters; SSE `Last-Event-ID` equivalent (browser reconnect) same.

## T4 — safe-suspend guard (turn-boundary enforcement)
```bash
$AP session send -id $SID -m "Count slowly from 1 to 20, one number per line."
$AP session suspend -id $SID       # IMMEDIATELY, while busy
# expect: "turn IN FLIGHT (busy=true) — refusing to checkpoint mid-turn"
sleep 30 && $AP session suspend -id $SID    # after idle → succeeds
```
PASS = refusal while busy, success at boundary.

## T5 — mid-turn wedge (KNOWN-FAIL repro of finding #1; destructive)
```bash
SID2=$($AP session new | awk '/^session:/{print $2}')
$AP session send -id $SID2 -m "Count slowly to 30."
$AP session suspend -id $SID2 -force        # checkpoint MID-TURN (the sin)
$AP session send -id $SID2 -m "hello?"      # auto-resume the frozen mind
curl -sS http://localhost:8000/healthz -H "Host: b-$SID2.agents.actors.resources.substrate.ate.dev"
# EXPECTED TODAY: restore OK (~220ms) but busy:true FOREVER — the in-flight API
# socket died in the freeze; the harness neither completes nor errors (zombie
# turn). Queued messages never process. Recovery: session delete (mind is lost
# past its last good turn).
$AP session delete -id $SID2 -force
```
**STATUS: PASS as of watchdog v2 (image :g4, 2026-07-21).** Long-gap repro
(freeze mid-turn → 85s dead window → wake): `session.error{turn_timeout}` →
queued message answered → idle. Correct classification, no lost messages,
self-healed with no human action. NOTE: a SHORT gap (≲10s) doesn't even wedge
(finding #1-refined) — the stream survives.

## T6 — pod-death recovery (Substrate case B: the well-handled one)
```bash
$AP session suspend -id $SID                    # ensure checkpointed
POD=$(kubectl get pods -n agentplane -o name | grep brain-pool | head -1)
kubectl delete $POD -n agentplane               # kill a worker pod
kubectl rollout status deploy/brain-pool-deployment -n agentplane
$AP session send -id $SID -m "Codeword again?"  # resume lands on a surviving/new pod
$AP session tail -id $SID -for 20s | grep ZEBRA-42 && echo "PASS: survived pod death"
```
PASS = same memory, different pod. (If the actor was RUNNING when its pod died:
syncer auto-demotes to SUSPENDED = rollback to last checkpoint — also PASS, but
anything since that checkpoint is expected loss.)
**STATUS: PASS (2026-07-21)** — pod deleted, rollout, resume on replacement pod,
essay topic recalled ("Operating systems").

## T8 — hand durability (brain+hand in one actor)
```bash
# agent from examples/coder.yaml (allow: Bash/Read/Write/… — USER-controlled policy)
# 1. "Using Bash, write HANDPROOF-7 into /workspace/note.txt, reply with cat output"
#    → expect agent.tool_use event + reply HANDPROOF-7
# 2. session suspend (checkpoint mind + WORKSPACE together)
# 3. "Reply with only the output of: cat /workspace/note.txt"
#    → expect HANDPROOF-7 — the file thawed with the memory
```
**STATUS: PASS (2026-07-22, image :g5).** Local tools execute in-actor under
spec-driven allow/deny (platform imposes no tool policy — decision 2026-07-22);
files written by tools survive checkpoint/restore because the fs is part of
the snapshot. gVisor is the safety boundary.

## T9 — durable workspace (DurableDir volume; Phase 1.5)
```bash
# agent: examples/coder.yaml with workspace.durable: true (compiles to a
# Substrate DurableDir volume mounted at /workspace — check the template has
# spec.volumes before trusting the drill!)
# 1. verify mount is real:   grep workspace /proc/mounts   → "none /workspace overlay rw"
# 2. git clone octocat/Hello-World into /workspace/repo + write marker
# 3. session suspend  →  send again (auto-resume)
# 4. cat marker + git log -1 + git status --short | wc -l   → marker, same HEAD, 0
```
**STATUS: PASS (2026-07-22, image :g6 — adds git).** Repo + marker survived
checkpoint/restore on the DurableDir; `git status` clean. Observations:
volume-bearing resume is SLOWER (~20-30s before sends are accepted; the 5xx
send-retry absorbs it — finding #3 machinery, no action needed). GOTCHA that
burned this drill's first run: `serve` compiles the template at CREATE time —
after changing the compiler, RESTART serve or agents get old-shape templates
(assert `spec.volumes` non-empty).

## T7 — zombie process (Substrate case C) — NOT YET TESTABLE
Killing only the node process inside the sandbox needs an admin/kill endpoint
(future). Expected per RELIABILITY.md: actor stays RUNNING, healthz dead →
dispatcher liveness probe must catch it (suspend + resume = one-step rollback;
disk transcript + resume-by-id is the recovery path).

## Cleanup + cost
```bash
$AP session list        # suspend anything RUNNING (suspended minds ≈ $0)
```
Cluster meter (~$0.50/hr) is independent of sessions.

## Findings log
- 2026-07-23 **#5 cold-image restore timeout — per-harness images are load-bearing**:
  a session resume on a worker that hasn't unpacked the template's image runs
  the pull+unpack inside atelet's restore RPC deadline. The 745MiB multi-harness
  image (:g9) failed EVERY restore ("creating workload from golden snapshot:
  DeadlineExceeded"), parking actors in a stuck RESUMING state; 696MiB (:g8) was
  borderline (failed after worker-pod restarts wiped the unpack cache — worker
  restarts make this WORSE). Fix: one image per harness (cc 270MiB / codex
  231MiB / pi 131MiB) — suite green on fully cold workers. Recovery for a stuck
  RESUMING actor: rollout-restart the worker pool (syncer demotes it to
  SUSPENDED), then delete with AGENTPLANE_BUCKET unset (the escrow fetch would
  re-wake it — that race is finding #3's cousin).
- 2026-07-23 **#6 key rotation requires a golden REBAKE — "env resolves at
  resume" was wrong**: a restored process carries the environment captured at
  golden-bake time (you cannot change a live process's environ; secretKeyRef
  re-resolution at restore does not reach the frozen process image). Proven:
  codex 401'd with a placeholder-era golden even after the real key was set
  AND a fresh session was minted. Rotation procedure: `configure` then
  recreate the agent (delete + create = rebake). configure's output now says
  this. ALSO: codex auth must be passed as CodexOptions.apiKey (SDK forwards
  it as CODEX_API_KEY to the CLI) — plain OPENAI_API_KEY env is ignored
  (image :x2). With that: codex FULLY live-verified ("CODEX-LIVE-OK").
- 2026-07-23 **pi harness verified live end-to-end** (image agentplane-brain-pi
  :p3): real turn ("stored.") + memory across checkpoint ("NEBULA-3") on
  anthropic/claude-haiku-4-5. Gotchas that burned the first attempts: pi needs
  node ≥22.19 (node:20 base → "webidl.util.markAsUncloneable is not a
  function"); DefaultResourceLoader REQUIRES {cwd, agentDir} (the SDK doc's
  minimal example omits them → crash in resolvePath).
- 2026-07-19 **#1 mid-turn checkpoint wedges the turn** (T5): restore fine,
  busy stuck forever, no error surfaced. Mitigated by the safe-suspend guard
  (`session suspend` probes /healthz busy; `-force` to override). Real fix:
  turn watchdog in server.mjs. Session lost: sess-yt3i9ebp9f (disposed).
- 2026-07-20 **#1 REFINED — short freezes are survivable**: a mid-turn
  checkpoint with a SHORT suspension (~8s frozen, fast auto-resume) did NOT
  wedge — the in-flight API stream survived restore and the turn completed
  normally (essay finished post-thaw; gVisor's in-sentry netstack + TCP
  tolerance). The wedge requires a LONG gap (peer/TLS timeout). Guard stays;
  severity downgraded for brief blips.
- 2026-07-20 **#2 watchdog v1 false positive** (image :g3): mechanism WORKS
  (interrupt → session.error → harness restart → healthy/busy cleared) but
  deadline is stamped at message ENQUEUE, so a message queued behind a long
  turn trips the watchdog and can kill the harness right at the boundary —
  and the queued message was consumed-and-lost by the dying stream. Also the
  interrupt error surfaces as reason=harness_error ("process aborted by
  user") instead of turn_timeout. v2 fixes: stamp turnStartedAt at YIELD
  (processing start), classify interrupt as turn_timeout, re-queue unprocessed
  inputs across restarts.
- 2026-07-20 **#3 send during first resume returns 500** and the message is
  lost — the CLI/dispatcher must retry sends on 5xx (idempotent: user.message
  append).
- 2026-07-21 **#4 template-deletion experiment (both parts)**:
  (a) template deleted → session resume FAILS ("error resuming actor") —
  **cascade order is correctness**: never delete an agent's template while its
  sessions live. (b) template RECREATED — even with a different image digest —
  old-snapshot sessions resume fine with memory intact (restore uses the
  actor's own snapshot content). So template recreation is a safe recovery
  path, and image bumps don't corrupt old minds.
- 2026-07-21 **v2 verification sweep**: T5 PASS (turn_timeout + no message
  loss), T6 PASS (pod death), dispatcher v0 live (escrow-first auto-suspend,
  warn-only on unreachable), derived status in `session list`
  (sleeping/idle/running/unreachable), send-retry observed rescuing wake races
  twice in production use.
