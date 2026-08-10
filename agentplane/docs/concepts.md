# Concepts

The building blocks of AgentPlane, and how they fit together. Each links to the
detailed page for that piece. For running the API, see [Using the API](api-usage.md).

## Durable sessions

**A session is an agent mind that survives being put to sleep.**

A session runs as a gVisor actor on Agent Substrate. When it goes idle it is
checkpointed to a snapshot and its worker is released; the next message resumes
it from that snapshot in about a second, memory and workspace intact. You don't
manage the lifecycle — sending a message wakes a sleeping session.

```bash
curl -s -X POST "$AGENTPLANE_URL/v1/sessions" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN" -d '{"agent":"starter"}'
# … days later, the same session resumes from its checkpoint
```

Use it when work spans time — a task you step away from and come back to.

## Agents and AgentSpec

**An agent is a reusable, versioned definition of how a mind behaves.**

An [AgentSpec](agents.md) sets the system prompt, model, harness, the tools the
agent may use (`allow`/`deny`), which tools require approval (`ask`), declared
repositories, and credentials. Applying a spec compiles it to a Substrate
ActorTemplate; reusing a name mints a new **version**, and running sessions keep
the version they started on.

Use it to make a behavior repeatable and shareable, not re-specified per session.

## Harnesses

**The harness is the agent loop that turns messages into tool calls.**

AgentPlane supports the **claude-code**, **pi**, and **codex** harnesses; a
session's agent picks one. The harness holds the conversation and decides which
tools to call — it is trusted code and runs in the brain, never the sandbox.

## Brain and hand

**The brain reasons; the hand is the sandbox where commands run.**

The brain runs the harness and holds the model key. The hand is a gVisor
sandbox — a bare "zone" holding only the durable `/workspace` and a toolchain.
The model's commands run *inside* the hand; nothing the model can reach holds a
credential or any of our code.

## The broker

**A command runs in the sandbox, launched from outside it.**

The broker is a plain pod between brain and hand. When the model calls a tool,
the broker runs it in the hand via the control plane's `ExecActor` (`runsc
exec`) — so the runtime lives *outside* gVisor and the hand carries none of it.
The broker also applies credentials before forwarding. See the
[architecture](architecture.md) for how the pieces fit together.

## Tools

**The built-in tools operate on the durable workspace.**

Every agent gets `bash`, `read`, `write`, `list`, `edit`, `glob`, and `grep`,
executed in `/workspace` inside the hand. An agent's spec narrows them with
`allow`/`deny`.

**External MCP servers extend the tool set.** An agent's `mcp:` servers are
federated by the broker — it connects to them as an MCP client and exposes their
tools alongside the built-ins (as `mcp__hand__<server>__<tool>`). The brain never
connects to an MCP server directly; all external MCP is mediated by the broker.

## Approvals

**Sensitive tools can pause for a human yes.**

A spec's `ask:` list gates those tools: the harness stops before running them and
emits an approval request instead of executing. A human approves (or denies)
through the API or `andromeda`, and the agent retries the exact call.

```bash
curl -s "$AGENTPLANE_URL/v1/sessions/$SID/approvals" -H "authorization: Bearer $AGENTPLANE_TOKEN"
curl -s -X POST "$AGENTPLANE_URL/v1/sessions/$SID/approvals/$REQ" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN" -d '{"decision":"approve"}'
```

Use it for tools whose effects you want to see before they happen.

## Credentials

**Secrets are injected outside the sandbox, never checkpointed.**

Store third-party credentials in the per-user vault (`PUT /v1/credentials`);
values are write-only and returned only as names. An agent references one by
name, and serve resolves it per session for that agent.

## Observability

**Every turn is traceable end to end.**

serve, the brain, the broker, and the hand export OpenTelemetry traces to a
collector, so one trace shows request → routing → turn → tool. See
[observability](observability.md).

## Access and ownership

**A token is a user identity, and you only see your own sessions.**

Except `POST /v1/access` (self-service token issuance), every `/v1/*` route needs
`Authorization: Bearer <token>`. Each token maps to a user; a session another
user owns returns `404`, not `403`, so ids can't be enumerated.
