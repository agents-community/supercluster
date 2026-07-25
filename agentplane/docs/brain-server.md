# Brain server — persistent Claude Code harness in a Substrate actor (Arch A, spike design)

A brain actor hosts ONE persistent conversational agent. Turns arrive over HTTP;
the mind survives suspend/resume (Substrate checkpoint) and process death
(on-disk transcript). Facts below verified against current Claude Code / Agent
SDK docs (2026-07-19).

## Verified stack decision

- **Harness: Claude Agent SDK** (`@anthropic-ai/claude-agent-sdk`, TypeScript —
  the brain image is node-based already). Docs recommend the SDK over raw
  `--input-format stream-json` CLI for exactly this "long-running session"
  pattern: `query({ prompt: asyncIterable })` keeps ONE session open and each
  HTTP turn is yielded into the iterable; events stream out.
- **Docs bless our persistence model verbatim:** *"If the container's memory AND
  filesystem are checkpointed/restored, the session resumes seamlessly."* That
  sentence is Substrate's value proposition, in Anthropic's own hosting docs.

## Turn server spec

```
POST /turn {message} → {turn_id}          async: atenet route timeout ~10s
GET  /turn/{id}      → pending | {result} (poll; SSE streaming = gate 2)
GET  /healthz        → dispatcher liveness probe (detects zombie harness)
```

- Feeds turns into the SDK's streaming-input iterable; ONE session per actor.
- **Session identity:** SDK session ids are UUIDs (deterministic ids NOT
  supported) → capture the id on first turn and write it to a file in the actor
  fs (`/workspace/.session-id`). The fs is checkpointed, so the id survives
  everything; on process restart the entrypoint passes `resume: <id>` and the
  SDK rebuilds the conversation from `~/.claude/projects/…/<id>.jsonl`
  (also in the checkpointed fs). Two recovery paths, one snapshot.
- **Hand wiring:** entrypoint computes the partner hand's atenet URL from
  `/run/ate/actor-id` + naming convention (`b-X` ↔ `h-X`) and passes it
  programmatically via SDK `options.mcpServers = { hand: {type:"http", url} }`.
- **Enforced brain/hand split (verified flags):**
  `permissionMode: "dontAsk"`, `allowedTools: ["mcp__hand__*"]`,
  `disallowedTools: ["Bash(*)","Edit(*)","Write(*)"]` — local execution
  impossible; every side effect routes to the hand actor.

## Operational rules (verified constraints)

- **Checkpoint only at turn boundaries** — an in-flight streaming API request
  aborted by freeze is NOT retried by the SDK. Turn-server exposes the boundary;
  dispatcher order: turn done → SuspendActor → ack queue.
- **Context:** auto-compacts around ~100K tokens (summary replaces history);
  `maxTurns` available as a hard cap; session files grow — `/compact` or archive
  policy for very long lives.
- **Memory:** ~1 GiB RSS / 1 CPU per active session (docs' starting point) —
  density math: a c3-standard-4 pod hosts ONE active mind at a time (Substrate
  model), and unlimited suspended minds cost only GCS. Recycle the harness
  process periodically (resume-by-id makes this free).
- **Observability:** `CLAUDE_CODE_ENABLE_TELEMETRY=1` + OTEL_* exporters → GKE
  managed OTel from day one.
- Unverified (flagged by docs check): prompt-caching default behavior in the
  SDK; cost aggregation across long sessions. Measure at gate 4.
- Optional belt-and-braces: SDK `SessionStore` adapter can mirror transcripts to
  GCS independent of snapshots (transcript durability even if a snapshot is lost).

## Spike gates

1. ✅ PASSED (2026-07-19). Brain answers over the session-events dialect w/ SSE:
   user.message → status_running → agent.message → status_idle{cost}. First turn
   9s (harness spawn). FIX FOUND: atenet forwards to actor port 80 → brain must
   listen on PORT=80 (set via template env; ActorTemplate spec is immutable —
   port changes mean template recreate).
2a. ✅ Cursor resume (2026-07-19, image :g2): stream + list accept
   `?since=evt_…` / SSE `Last-Event-ID` — replay only newer events, sourced from
   the persisted events.jsonl (cursors valid across process lives). Verified:
   Last-Event-ID evt_000006 → replayed only 7,8. NOTE: image upgrades only reach
   NEW actors (existing minds resume their snapshot with old code) — template
   recycle + fresh actor is the upgrade path; migrating a live mind = future work.
2. ✅ PASSED (2026-07-19). Codeword taught → SuspendActor (memory → GCS) → new
   message auto-resumed the actor → "MOONRIVER-7" recalled from RESTORED PROCESS
   MEMORY in 1.2s (vs 9s cold spawn). Event log (events.jsonl) continued
   seamlessly across the checkpoint. SSE shipped in gate 1.
2b. ✅ Reliability sweep (2026-07-21, image :g4): watchdog v2 (deadline at
   processing-start, turn_timeout classification, re-queue once) — long-gap T5
   PASS; dispatcher v0 (auto-suspend idle, escrow-first); derived session
   status; T6 pod-death PASS; template cascade rules verified (delete = dead
   sessions; recreate = full recovery).
2e. ✅ pi adapter — FIRST fully-live third harness (2026-07-23, image
   agentplane-brain-pi:p3): mitsuhiko's model-agnostic pi; `model:
   "provider/model-id"` selects ANY vendor, compiler wires the key from the
   provider prefix (anthropic/openai/google). Live-verified end-to-end: real
   turn + memory-across-checkpoint on anthropic/claude-haiku-4-5.
   session.abort() = real watchdog interrupt. ALSO: per-harness images now
   MANDATORY (finding #5 — multi-harness image restores time out); session
   API objects now carry `harness`.
2d. ✅ Codex adapter (2026-07-22, image :g7 — adds @openai/codex-sdk):
   `harness: codex` in the AgentSpec — same server, second loop. Per-turn CLI
   spawn; continuity = thread-id in .session-id + rollout under ~/.codex
   (actor fs, checkpointed). Tool-policy translation: empty allow → read-only
   sandbox, any allow → danger-full-access (gVisor is the boundary).
   Compiler wires OPENAI_API_KEY ← openai-api-key secret + AGENTPLANE_HARNESS;
   `configure -provider openai`. VERIFIED live to the vendor-auth boundary:
   401 with placeholder key surfaces as session.error, poison-guard retries
   once, settles idle (codex CLI backoff ≈50s per auth failure). Real-key
   turn pending an OpenAI key. RBAC gotcha: each vendor secret must be added
   to the ate-api-server-env-sources Role resourceNames or golden bake stalls
   in ResumeGoldenActor ("secrets … is forbidden").
2c. ✅ Product surfaces (2026-07-22, image :g6 — adds git): AgentSpec compiler
   (`agent create` / `POST /v1/agents` — see agents.md), `serve` HTTP control
   plane with end-to-end OTel tracing (serve → atenet Envoy → ResumeActor in
   one waterfall — see api.md), tool policy user-controlled via spec
   allow/deny (T8: brain+hand in one actor), durable workspace via Substrate
   DurableDir (`workspace.durable: true`, T9: repo survives checkpoint as fs
   data). Builds go through Cloud Build (`gcloud builds submit` in brain/) —
   local registry pushes are unreliable on the dev LAN.
3. [DEFERRED] Brain's MCP call wakes the hand actor (`mcp__hand__exec uname`).
4. Real task (clone/fix/PR) with built-ins denied; spike-grade PAT in hand.
5. Swap PAT for Warden (identity JWT → credential injection, proven in Phase 3).
6. Dispatcher v1: Cloud Tasks front door, checkpoint-then-ack, liveness probe
   (contract in ../substrate-agents-api/docs/RELIABILITY.md).

## Deletion lifecycle (requirement, 2026-07-20)

Deletion must CASCADE: session delete = escrow transcript → suspend → delete
actor → remove its snapshot prefix. Agent (template) delete = cascade all its
sessions first (linkage = actor.template), then golden actor + golden
snapshots + template + agent-scoped Secrets. Everything runtime goes away with
the agent; the ONE deliberate survivor is the escrowed transcript (audit
record, GCS lifecycle-ruled) — true erasure is an explicit --purge.
Open verification: can a session actor resume after its template is deleted?
(resume path resolves env via template → likely NO → cascade order is
correctness, not hygiene).
