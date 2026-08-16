# Agents — AgentSpec & lifecycle

An **agent** is a named, immutable definition: harness + behavior config. A
**session** is a durable mind minted *from* an agent. Underneath, agent =
Substrate `ActorTemplate`, session = actor — but you only ever touch the
AgentSpec. The brain image is resolved by the platform from the harness, so it
never appears in a spec.

## AgentSpec

```yaml
name: tutor
harness: claude-code          # or codex, pi
systemPrompt: >-
  You are a patient math tutor. Keep answers short.
turnDeadlineSeconds: 90       # watchdog: max wall-clock per turn
# model: claude-sonnet-5
# apiKeySecret: my-key-secret # per-agent BYO key (defaults to shared secret)
# allow: ["Bash(ls*)"]
# deny:  ["WebSearch"]
# mcp:
#   github: {url: https://…/mcp, headers: {Authorization: "Bearer …"}}
```

Validation is strict where it protects you: names must be DNS-safe and harnesses
must be known. The spec is translated to each harness's native config by the
in-image adapter — the file above is vendor-neutral.

## Any model, one harness: pi

The `pi` harness is model-agnostic — `model: "provider/model-id"` selects the
vendor per agent, and the compiler wires the matching key automatically:

| `model:` prefix | Key env | Default secret |
|---|---|---|
| `anthropic/…` (default) | `ANTHROPIC_API_KEY` | `anthropic-api-key` |
| `openai/…` | `OPENAI_API_KEY` | `openai-api-key` |
| `google/…` | `GEMINI_API_KEY` | `gemini-api-key` |

```yaml
harness: pi
model: google/gemini-2.5-pro    # switch vendors by editing ONE line
```

Configure keys with `agentplane configure -provider anthropic|openai|google`.
Other providers: set `apiKeySecret` explicitly (pi itself supports many more).

## Tool policy is yours

The platform imposes **no** tool policy — `allow`/`deny`/`ask` in the spec is
the entire control. Only allow-listed tools are auto-approved; headless, anything
else is denied at the permission layer, so an empty `allow` means a chat-only
agent. The safety boundary is the gVisor sandbox, not tool lists.

### `ask:` — tools that need a human yes

```yaml
allow: [WebFetch]
ask:   [Bash, github/create_issue]   # same vocabulary as allow/deny
deny:  [Write, Edit]
```

`ask` **gates** a tool; it does not grant one. A gated call is denied with a
reason the model reads, a `tool.approval_requested` event is emitted, and the
turn **ends cleanly** — the actor suspends, and waiting costs nothing.

```
andromeda    →  /approvals, then /approve <id> or /deny <id>
```

Approving queues an input, so the agent retries the call it was blocked on
rather than sitting idle until you happen to send a message.

Three properties worth knowing:

- **Per call, not per tool.** The request id hashes the tool *and its input*, so
  approving `rm -rf build` does not approve `rm -rf /`.
- **Single use.** A grant is spent when the tool runs, and the spend is
  recorded, so a restart cannot resurrect it.
- **Durable.** Requests and answers are events, so they survive suspend, harness
  restart and a cold rebuild — the runtime folds the log at startup.

Implemented as a `PreToolUse` hook rather than `canUseTool`. Awaiting a human
inside `canUseTool` would hold the turn open until the watchdog killed it, and
under `permissionMode: "dontAsk"` the SDK short-circuits denials — the SDK
documents that PreToolUse denies bypass `canUseTool`, so it is the hook that
reliably runs in a headless session.

### `WebFetch` and `WebSearch` are not sandboxed by the hand

Both are reasoning-layer tools, so they are the exception to "the mind decides,
the hand executes":

| Tool | Runs | Consequence |
|---|---|---|
| `WebSearch` | at the model provider | results arrive in the response; if the provider or region does not offer it, allowing it does nothing |
| `WebFetch` | in the brain actor | fetched content enters the model's context without crossing the sandbox that runs commands |

Actors have unrestricted outbound network access (`0.0.0.0/0` minus RFC1918 and
the metadata server), so `WebFetch` can reach any host.

### How the spec reaches the harness

The AgentSpec is vendor-neutral; each in-image adapter translates it. For
`claude-code` the mapping is direct
(`agentplane/brain/harness/claude-code.mjs`):

| Spec | Agent SDK option | Effect |
|---|---|---|
| `systemPrompt` | `systemPrompt` | as written |
| `model` | `model` | as written |
| `allow: [X]` | `allowedTools` | X runs without asking |
| `deny: [X]` | `disallowedTools` | X is removed |
| `mcp:` | `mcpServers` | federated **through the broker** (the brain never dials an upstream directly) — see below |
| — | `permissionMode: "dontAsk"` | always set |

`allowedTools` is, in the SDK's own words, a list of tools "auto-allowed
**without prompting**" rather than a restriction — it directs you to the `tools`
option to restrict. It nevertheless behaves as an allow-list here, because
`permissionMode: "dontAsk"` in a headless session means an unlisted tool has
nobody to approve it and is denied rather than queued. Worth knowing if you ever
change the permission mode: `allow` would stop being a boundary the moment
something could answer a prompt.

> **Subagent caveat.** `deny` is not reliably inherited by subagents — a
> subagent can run a tool its parent denied (upstream
> [claude-agent-sdk #172](https://github.com/anthropics/claude-agent-sdk-typescript/issues/172)).
> Harness versions are pinned, but do not treat per-subagent tool policy as
> enforced. The gVisor sandbox is the boundary that does not depend on it.

### Where your `mcp:` servers connect

With `hand: true` (the normal case) the brain connects to exactly **one** MCP
server — the **broker** — and your `mcp:` servers are federated *through* it. The
brain never opens a connection to an upstream server itself: it holds the model
key, so all external MCP is mediated by the broker.

```
you declare    mcp: {deepwiki: {url: "https://mcp.deepwiki.com/mcp"}}
the brain      passes the config to the broker; it never dials the server
the broker     connects to the server as an MCP client and re-exposes its tools
               alongside the hand's — the brain sees one server, the broker
```

Scope federated tools with `server/tool` in `allow`/`deny`/`ask`
(e.g. `allow: [deepwiki/*]`); they reach the model as `mcp__hand__<server>__<tool>`.

The visible consequence is naming. A `create_issue` tool on a `github` upstream
reaches the model as:

```
mcp__hand__github__create_issue      ← what you get
mcp__github__create_issue            ← what you might expect
```

So two things follow:

- **`allow: [mcp__github__*]` matches nothing.** The hand's own entry
  (`mcp__hand__*`) is added to `allowedTools` automatically, which covers every
  federated tool too.
- **Scope federated tools with `server/tool`, not the runtime name.** The spec is
  compiled before any upstream is dialled, so it cannot know that a `github`
  server exposes `create_issue`. serve learns the real names at session setup and
  translates them:

  ```yaml
  allow: [github/list_issues]     # permitted; github's other tools are denied
  allow: [github/*]               # the whole server
  ```

  Naming any tool of a server turns that server into an allow-list. A server
  nobody names is untouched, so this is inert for specs that do not use it.
  Resolution emits **denials only** — it can never grant a tool the operator
  disabled.

  An entry matching nothing **fails session creation**, naming the bad entries
  and the servers that connected — so a misspelled server (`gihub/list_issues`)
  is caught at create rather than leaving the real `github` unscoped.

Without a hand (`hand: false`), your servers connect straight to the brain and
the names are `mcp__github__*` — but then the reasoning layer holds the
credentials, which is what the split exists to avoid.

## Split agents: brain + hand

```yaml
hand: true                    # split the agent: reasoning ≠ execution
credentials: [gh-token]       # vault credentials granted to the session's hand
```

With `hand: true` the agent is minted as **two** paired actors: the **brain**
(the harness, which reasons but executes nothing) and the **hand** (a gVisor
sandbox holding the durable `/workspace` and a toolchain). The model's tools run
in the hand, launched from outside the sandbox by the broker — so tool execution
is observable, credential use is attributable, and the reasoning process never
holds secrets. `credentials:` names entries from the user's vault
(`PUT /v1/credentials/{name}`) that serve grants to the hand at session setup.
Hand-role agents are hidden from `GET /v1/agents`.

Curated starting points live in
[`examples/`](https://github.com/agents-community/supercluster/tree/main/agentplane/examples):
`starter.yaml` (claude-code, split-agent, git-capable), `codex.yaml`, `pi.yaml`.

## Versions: updating an agent never touches its sessions

`ActorTemplate.spec` is immutable in Substrate, so a changed agent is a **new
version compiled to a new template** — and the old templates stay put:

```bash
agentplane agent create -f tutor.yaml     # v1
# edit the system prompt…
agentplane agent create -f tutor.yaml     # v2 — sessions on v1 keep running
```

```
POST /v1/agents            re-posting an existing name mints the next version
GET  /v1/agents/{n}/versions   history, newest first, with each spec
POST /v1/sessions {"agent":"tutor"}              → latest version
POST /v1/sessions {"agent":"tutor","version":1}  → that exact definition
```

Sessions **pin** the version they were minted from, so a running mind keeps
answering on the template it started with. Before this, the only way to change
an agent was `DELETE ?cascade=true` + recreate, which destroyed every session
it owned — editing a prompt killed every durable mind using it.

Versions are append-only: a version document is written once and never
rewritten, so **rollback is a new version** whose spec is copied from an older
one, and the history stays auditable. Deleting the agent removes every version
(and refuses, without `?cascade=true`, while any version still has sessions).

Version 1 keeps the bare agent name; later versions are `agent-vN`. An agent
may therefore not be *named* like a versioned template — `foo-v2` is rejected,
since it would collide with version 2 of `foo`.

Requires `AGENTPLANE_PROJECT` (Firestore). Without it, agent creation keeps its
original create-once behavior and re-posting a name still returns `409`.

## Durable workspace

```yaml
workspace:
  durable: true
```

Mounts `/workspace` as a Substrate **DurableDir** volume: repos and build
artifacts persist across suspend/resume as *filesystem* data, kept out of the
memory image. Use it for agents that hold checkouts or build state — the
memory snapshot stays lean while the workspace survives independently.

## Play with it: `andromeda` (or `chat`)

The distributable TUI — hand someone a URL + token and they're chatting with
a durable mind on your cluster:

```bash
cd andromeda && npm install        # (until it's published to npm)
ANDROMEDA_URL=http://localhost:7433 ANDROMEDA_TOKEN=… node src/cli.mjs --agent buddy
```

Enter sends, **Ctrl+S suspends the mind in place**, Esc detaches — and
re-attaching (`--session sess-…`) replays history and continues where you
left off, even across checkpoints.

## Scripting it: `chat`

The interactive front door — create an agent, then chat like any CLI agent,
except **detaching never loses the mind**:

```bash
agentplane agent create -f my-agent.yaml
agentplane chat -agent my-agent          # mints a session, drops into a REPL
# … talk, watch tool activity stream, Ctrl-D to detach …
agentplane chat -id sess-…               # tomorrow: same mind, full history
```

In the REPL: `/suspend` freezes the mind in place, `/quit` detaches. A send to
a sleeping mind auto-wakes it (~1–2s) — chat shows "(waking the mind…)" while
the checkpoint thaws.

## Commands (CLI or API)

Everything below is also available over HTTP (`agentplane serve`):
`POST /v1/agents` with the spec as the body, `DELETE /v1/agents/{id}[?cascade=true]`
— same validation, same cascade guard. See [Control-plane API](api.md).

```bash
agentplane agent create -f tutor.yaml   # compile → apply → bake golden (~30s)
agentplane agent create -f tutor.yaml -print   # inspect the compiled template
agentplane agent list                   # AGENT / HARNESS / PHASE / SESSIONS
agentplane session new -agent tutor     # mint a mind from this agent
agentplane agent delete -name tutor     # refuses while sessions live
agentplane agent delete -name tutor -cascade   # escrow + delete sessions first
```

## Deletion semantics

Deleting a template while its sessions live makes them **unresumable** — so:

1. `agent delete` **refuses** when sessions exist, listing them.
2. `-cascade` destroys sessions first, escrow-first: each transcript is written
   to `gs://$AGENTPLANE_BUCKET/transcripts/<sid>/` **before** anything dies.
3. Only then does the template (and its golden) go.

Re-creating an agent with the same name later is safe — even with a different
image, previously-suspended sessions restore from their own snapshots with
memory intact.
