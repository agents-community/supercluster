# hand — the agent's tool gateway

The **hand** is the half of a split agent that touches the world. It runs as
its own Substrate actor (gVisor sandbox) next to the **brain** (the reasoning
harness), and exposes exactly one door: an **MCP server over streamable HTTP**
at `/mcp`. Whatever the model decides, execution happens here — never in the
brain.

## What it serves

- **Own tools** — `bash`, `read`, `write`, `edit`, `list`, `glob`, `grep`,
  executed inside the sandbox workdir (`/workspace`, durable with the session).
- **Federated tools** — the user's own MCP servers, registered at session
  setup; the hand connects out and re-exports their tools under one roof, so
  the brain always sees a single toolbox (`tools/list` is the union).

## Admin surface (serve-only)

`POST /admin/upstreams` (register MCP upstreams) and `POST /admin/grant`
(deliver a short-lived credential grant) — bearer-gated by
`HAND_ADMIN_TOKEN`. With a grant the hand pulls declared credentials from
serve into actor memory (git credentials / env / header form). The
[`egress/`](../egress/) gateway supersedes this pull path: goal state is the
hand holding **no** credentials at all.

## Execution journal & idempotency advisory

Every **mutating** tool call (`bash`, `write`, `edit`) is journaled — a start
line and a completion line (tool, args hash, command summary, outcome,
duration) — to two places at once: `<workdir>/.hand-journal.jsonl` (durable
with the session, visible to the model) and **stdout**, which Cloud Logging
ingests with the actor's labels and retains after the hand is gone. That
stdout copy is the operator audit trail: it records what was *actually
executed*, not just what the model intended.

Idempotency is **platform-enforced, keyed by the logical call id**: when a
tool call carries an id (MCP `_meta` tool-use id / progress token), a
re-arrival of the *same id* replays the stored result without re-executing —
exactly-once per model decision, guaranteed by the hand, no model cooperation
involved. Errors are remembered too: a same-id replay of a failed call gets
the recorded error, never a blind re-execution against unknown state.

A *new* id with identical content is a fresh model decision: it executes, and
the result carries an advisory ("an identical call completed Ns ago") so the
model can recognize a possible retry-after-lost-response. Pure tools
(`read`/`list`/`grep`/`glob`) skip all of this.

## Observability

With `OTEL_EXPORTER_OTLP_ENDPOINT` set, every tool execution is a span
(`hand.tool <name>`), joined to the request's trace via W3C `traceparent`
extracted from the brain's MCP calls.

## Test

```bash
docker run --rm -v "$PWD":/app -w /app node:22-slim node test/mcp-smoke.mjs
# or, with node >= 22:  node test/mcp-smoke.mjs
```

Boots the real server and drives it like the brain does: initialize →
`tools/list` (7 own tools) → write/read/bash round-trip → workdir confinement
→ admin auth gate.

## Config

| Env | Default | Meaning |
|---|---|---|
| `PORT` | `8080` | MCP + admin listener |
| `HAND_WORKDIR` | `/workspace` | tool execution root |
| `HAND_ADMIN_TOKEN` | *(empty = open, dev only)* | bearer for `/admin/*` |
| `OTEL_EXPORTER_OTLP_ENDPOINT` | *(unset = off)* | OTLP/HTTP trace export |
