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
| **[`agentplane/`](agentplane/)** | The control plane + runtime — agents, sessions, the durable-mind engine, credential vault, self-service access, and the HTTP API. | Go control plane · Node brain image · [Agent Substrate](https://github.com/agent-substrate/substrate) |
| **[`andromeda/`](andromeda/)** | The terminal you actually live in — a rich TUI (markdown, streaming, syntax highlighting) for chatting with durable minds. | Ink, zero-build — `npx @agentsupercluster/andromeda` |
| **[`hand/`](hand/)** | The agent's hand — an MCP tool gateway in the sandbox: bash/file tools + federation of the user's own MCP servers. The brain talks to one door. | Node · MCP streamable HTTP |
| **[`gitproxy/`](gitproxy/)** | Attaches your git credential **outside** the sandbox, so a repository token never enters an actor — and so a checkpoint cannot capture it. | Go · distroless |
| **[`infra/`](infra/)** | Deploy manifests, network policy, and versioned [Substrate patches](infra/substrate-patches/). Every account-specific value lives in one `config.env`; [`BOOTSTRAP.md`](infra/BOOTSTRAP.md) stands the whole thing up in a fresh GCP project. | k8s · GCP |

Galaxies never share code across language boundaries — the contract between
them is the **HTTP API** ([`agentplane/docs/api.md`](agentplane/docs/api.md)).
Testing spans three layers — see [`test/`](test/).

## 🚀 Quickstart

**As a user** (someone is already hosting; your email is allowlisted):
```bash
npx @agentsupercluster/andromeda login        # email → personal token, saved
npx @agentsupercluster/andromeda --agent starter
#   … chat … /sleep to sleep the mind … /usage for cost … Esc to detach …
npx @agentsupercluster/andromeda              # tomorrow: pick the session up — it remembers
```
Full walkthrough: [`andromeda/ONBOARDING.md`](andromeda/ONBOARDING.md).

**As an operator**, deploying into your own GCP project — full runbook in
[`infra/BOOTSTRAP.md`](infra/BOOTSTRAP.md), which front-loads the cluster
settings that are not optional and are invisible until they bite:

```bash
cp infra/config.env my-account.env    # project, cluster, bucket, image tags
source my-account.env
./infra/build.sh all                  # build every image into your registry
./infra/render.sh | kubectl apply -f -
kubectl apply -f infra/serve/networkpolicy.yaml
```

Then create agents — the spec pins an image digest, which is per-account, so
`agent.sh` resolves it for you:

```bash
./infra/agent.sh agentplane/examples/starter.yaml --create   # or codex.yaml / pi.yaml
./test/smoke-live.sh                  # asserts a hand tool actually ran
```

Bring your own key (nothing stored):
```bash
ANDROMEDA_API_KEY=sk-… npx @agentsupercluster/andromeda --agent starter
```

## 🧭 Concepts in one breath

- **agent** = a **versioned** definition (harness + model + tools + prompt + the repos its sandbox should contain). Each version compiles to its own immutable Substrate `ActorTemplate`; re-posting a name mints the next version and **running sessions keep the one they were minted from**.
- **session** = a durable mind minted from an agent — a checkpointable gVisor actor.
- **harness** = the brain vendor (claude-code / codex / **pi**); adding one is a single strategy module.
- **the API is the boundary** — every client (TUI, web, scripts) speaks the same HTTP dialect; nothing needs cluster access.

## 🔭 Status & roadmap

Alpha, and everything below is **live-verified** rather than designed:

- the runtime, three harnesses, durable workspace, hand tool gateway, streaming TUI, end-to-end tracing
- **agent versioning** — updating an agent no longer destroys its sessions
- **declarative repositories** — an agent's repos are checked out before its first message
- **git credentials outside the sandbox** ([`gitproxy/`](gitproxy/)) — a repo token never enters an actor, so a checkpoint cannot capture it
- **per-user MCP auth** — credentials are vault references, never literals in a spec
- **actor egress constrained** — the GCP metadata server and private ranges are unreachable from a sandbox
- **session metadata in Firestore** — usage and activity readable while a mind sleeps, and cost survives deletion

Known gaps are tracked honestly in the
[threat model](agentplane/docs/threat-model/README.md), which is written against
the **cluster** rather than against `main` — the two differ, and the difference
is the point. The largest open items: the public endpoint is still plain HTTP,
and subagent tool policy is not enforced by the harness
([upstream #172](https://github.com/anthropics/claude-agent-sdk-typescript/issues/172)).

Next: HTTPS, then extending the gitproxy pattern to non-git credentials, then
per-user credential selection via Substrate `ActorIdentity`.

## 🤝 Contributing

Read [`CONTRIBUTING.md`](CONTRIBUTING.md) first — the workflow is
**issue-first, one feature per PR**. Then
[`agentplane/CONTRIBUTING.md`](agentplane/CONTRIBUTING.md) for repo layout,
the durable-mind design principles, and how to add a harness.

<div align="center">
<sub>built on <a href="https://github.com/agent-substrate/substrate">Agent Substrate</a> · minds that persist</sub>
</div>
