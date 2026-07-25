# ✦ andromeda

A beautiful terminal for **durable agent minds** on AgentPlane. Chat like any
CLI agent — except detaching never loses the conversation: the mind
checkpoints, sleeps for ~free, and wakes with full memory when you return.

```bash
npx @agentplane/andromeda --agent buddy        # new mind, start chatting
npx @agentplane/andromeda --session sess-…     # come back tomorrow — it remembers
npx @agentplane/andromeda                      # see what agents/sessions exist
```

## Connecting

andromeda talks only to the AgentPlane control-plane API — a URL and a token
are the whole setup. Get both from whoever runs the cluster:

```bash
export ANDROMEDA_URL=https://agentplane.example.com     # or http://localhost:7433
export ANDROMEDA_TOKEN=…
```

## Keys while chatting

| Key | Action |
|---|---|
| Enter | send |
| Ctrl+S | suspend — the mind checkpoints in place (any message wakes it) |
| Esc | detach — the mind keeps living; re-attach any time |

## Development

Zero-build ESM: `node src/cli.mjs --help`. The backend contract is the
AgentPlane session-events dialect (JSON + SSE) — see the repo's
`docs/api.md`. This module never touches kubectl, gRPC, or the cluster.
