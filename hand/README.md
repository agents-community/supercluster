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
