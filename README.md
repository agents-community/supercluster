<div align="center">

# 🌌 supercluster

### durable agent minds that never forget

*A supercluster is the largest known structure in the universe — a web of galaxies bound by gravity.*
*This one binds **galaxies** of agent infrastructure around a single idea:*
**an AI agent should be a living mind you can put to sleep and wake up, not a script you re-run.**

[![status](https://img.shields.io/badge/status-alpha-8b5cf6)](#)
[![harnesses](https://img.shields.io/badge/harnesses-claude--code_·_codex_·_pi-22d3ee)](#)
[![runtime](https://img.shields.io/badge/runtime-Agent_Substrate_·_gVisor-a78bfa)](#)
[![license](https://img.shields.io/badge/license-TBD-6b7280)](#)

</div>

---

## ✨ What is this?

Most "AI agents" are stateless: every run starts from zero. **supercluster** runs
each agent as a **durable mind** — a real process, in a checkpointable gVisor
sandbox, that:

- 🧠 **remembers** — conversation, tool state, and its filesystem all survive
- 💤 **sleeps for ~free** — checkpointed to storage; wakes in ~1–2s with full memory
- ♻️ **self-heals** — a turn watchdog and poison-message guard survive wedges, crashes, and pod deaths
- 🔌 **runs any brain** — Claude Code, Codex, or **pi** (any model, one harness) — same runtime
- 🔑 **is yours to key** — bring your own API key per session; it's never stored
- 🛰️ **is portable** — every capability is a plain HTTP API; hand someone a URL + token and they're in

You can close your terminal tonight and re-attach tomorrow to the *same mind*,
mid-thought. That's the whole product.

## 🌠 The galaxies

This is a **monorepo**. Each product is a **galaxy**; the durable minds it runs
are its stars.

| Galaxy | What it is | Stack |
|---|---|---|
| **[`agentplane/`](agentplane/)** | The control plane + runtime — agents, sessions, the durable-mind engine, and the HTTP API. | Go control plane · Node brain image · [Agent Substrate](https://github.com/agent-substrate/substrate) |
| **[`andromeda/`](andromeda/)** | The terminal you actually live in — a beautiful TUI for chatting with durable minds. | TypeScript-less Ink (zero-build, `npx`-runnable) |

More galaxies will join (infra-per-provider, a web console, …). They never
share code across language boundaries — the contract between them is the
**session-events HTTP dialect** ([`agentplane/docs/api.md`](agentplane/docs/api.md)).

## 🚀 Quickstart

```bash
# 1. control plane (needs an Agent Substrate cluster — see agentplane/docs)
cd agentplane && go build -o /tmp/agentplane ./cmd/agentplane
/tmp/agentplane serve                       # the HTTP control plane

# 2. define a durable agent
/tmp/agentplane agent create -f examples/tutor.yaml

# 3. live in it
cd ../andromeda && npm install
ANDROMEDA_URL=http://localhost:7433 ANDROMEDA_TOKEN=… node src/cli.mjs --agent tutor
#   … chat … Ctrl+S to sleep the mind … Esc to detach …
node src/cli.mjs --session sess-…           # tomorrow: it remembers
```

Bring your own key (nothing stored):
```bash
ANDROMEDA_API_KEY=sk-… node src/cli.mjs --agent tutor
```

## 🧭 Concepts in one breath

- **agent** = an immutable definition (harness + model + tools + system prompt), compiled to a Substrate `ActorTemplate`.
- **session** = a durable mind minted from an agent — a checkpointable gVisor actor.
- **harness** = the brain vendor (claude-code / codex / **pi**); adding one is a single strategy module.
- **the API is the boundary** — every client (TUI, web, scripts) speaks the same HTTP dialect; nothing needs cluster access.

## 🔭 Status & roadmap

Alpha — the runtime, three harnesses, durable-workspace, tracing, and the HTTP
API are live-verified; see [`agentplane/docs/`](agentplane/docs/) and the
reliability runbook. Planned: per-provider infrastructure (Terraform) and CI/CD
per galaxy, a hosted access point, and a web console.

## 🤝 Contributing

Start with [`agentplane/CONTRIBUTING.md`](agentplane/CONTRIBUTING.md) — repo
layout, the durable-mind design principles, and how to add a harness.

<div align="center">
<sub>built on <a href="https://github.com/agent-substrate/substrate">Agent Substrate</a> · minds that persist</sub>
</div>
