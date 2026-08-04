# Agents — AgentSpec & lifecycle

An **agent** is a named, immutable definition: harness + image + behavior
config. A **session** is a durable mind minted *from* an agent. Underneath,
agent = Substrate `ActorTemplate`, session = actor — but you only ever touch
the AgentSpec.

## AgentSpec

```yaml
name: tutor
harness: claude-code          # or codex, pi (opencode: planned)
image: gcr.io/…/agentplane-brain@sha256:…    # digest-pinned (required)
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

Validation is strict where it protects you: the **image must be digest-pinned**
(mutable tags would silently invalidate snapshots), names must be DNS-safe,
harnesses must be known. The spec is translated to each harness's native
config by the in-image adapter — the file above is vendor-neutral.

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

The platform imposes **no** tool policy — `allow`/`deny` in the spec is the
entire control. Only allow-listed tools are auto-approved; headless, anything
else is denied at the permission layer, so an empty `allow` means a chat-only
agent. The safety boundary is the gVisor sandbox, not tool lists.

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
| `mcp:` | `mcpServers` | federated **through the hand** when there is one (the normal case) — see below |
| — | `permissionMode: "dontAsk"` | always set |

`allowedTools` is, in the SDK's own words, a list of tools "auto-allowed
**without prompting**" rather than a restriction — it directs you to the `tools`
option to restrict. It nevertheless behaves as an allow-list here, because
`permissionMode: "dontAsk"` in a headless session means an unlisted tool has
nobody to approve it and is denied rather than queued. Worth knowing if you ever
change the permission mode: `allow` would stop being a boundary the moment
something could answer a prompt.

> **Subagent caveat.** `deny` is not reliably inherited by subagents — an
> escrowed transcript shows `Bash` running under an agent that denied it
> ([threat model F13](threat-model/README.md), upstream
> [#172](https://github.com/anthropics/claude-agent-sdk-typescript/issues/172)).
> Harness versions are pinned and `test/smoke-live.sh` asserts the boundary, but
> do not treat per-subagent tool policy as enforced. gVisor is the boundary that
> does not depend on it.

### Where your `mcp:` servers actually connect

They always work — but with `hand: true` (the normal case) they are **not**
connected to the reasoning layer. The brain connects to exactly one MCP server,
the hand, and your servers are federated *through* it:

```
you declare        mcp: {github: {url, headersFrom: {...}}}
serve, at setup    resolves the credential for THIS user, POSTs the upstream
                   to the hand's /admin/upstreams
the hand           connects to github and re-exposes its tools next to its own
the brain          sees ONE server — the hand
```

That is the point of the split: **the brain never holds an upstream URL or a
credential.** Only the hand does, and only for the session it belongs to.

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
- **You cannot allow-list federated tools individually today.** Restrict by
  *which servers you connect*, not by which of their tools you permit.

Without a hand (`hand: false`), your servers connect straight to the brain and
the names are `mcp__github__*` — but then the reasoning layer holds the
credentials, which is what the split exists to avoid.

## Split agents: brain + hand

```yaml
hand: true                    # split the agent: reasoning ≠ execution
credentials: [gh-token]       # vault credentials granted to the session's hand
```

With `hand: true` the agent is minted as **two** paired actors: the **brain**
(the harness, which reasons but executes nothing) and the **[hand](../../hand/)**
(an MCP tool gateway that owns bash/file tools and federates the user's MCP
servers). The brain's tool traffic all flows through the hand's one MCP door —
so tool execution is observable, credential use is attributable, and the
reasoning process never holds secrets. `credentials:` names entries from the
user's vault (`PUT /v1/credentials/{name}`) that serve grants to the hand at
session setup. Hand-role agents are hidden from `GET /v1/agents`.

Curated starting points live in [`examples/`](../examples/): `starter.yaml`
(claude-code, split-agent, git-capable), `codex.yaml`, `pi.yaml`.

## Egress policy & credential injection

> **Declarative today, not yet enforced.** These fields validate and compile
> onto the template so serve can render a gateway policy per session, but the
> egress gateway is not deployed ([`egress/`](../../../egress/), threat-model
> F9) — actor egress is still unrestricted in practice. Declaring a policy
> documents intent; it does not yet constrain anything.

```yaml
egress:
  mode: limited                 # unrestricted (default) | limited
  allowedHosts: [github.com, api.github.com]
credentials:
  - name: gh-token
    inject:
      hosts: [github.com]       # which destinations receive the secret
      location: {header: true}  # header and/or body — never the URL path
```

`credentials: [gh-token]` (a bare name) still works and means "no injection" —
the legacy path where the hand pulls the value into actor memory.

**Reachability and credential scope are separate.** `egress.allowedHosts` says
which destinations are reachable; `inject.hosts` says which receive the
secret. A host must be in **both** — being reachable never implies being
trusted with a credential, and injecting into a host outside the allowlist is
rejected at create rather than silently doing nothing.

Three limits worth knowing before you write a policy:

- **The URL path is never injected.** Path-secret endpoints (Slack incoming
  webhooks) can't be used this way — prefer header auth.
- **`location` should stay header-only unless you need otherwise.** Request
  bodies are assembled from content the agent is working with, so body
  injection is the wider exposure surface.
- **Clients that validate key format locally will break** once injection is
  live: they see an opaque placeholder, not a real key, and can fail before
  making any network call.

Hosts are plain hostnames — a scheme, port, or path is rejected, since those
are the usual ways an allowlist entry looks right and matches nothing.

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

## Deletion semantics (verified, not aspirational)

Deleting a template while its sessions live makes them **unresumable** — so:

1. `agent delete` **refuses** when sessions exist, listing them.
2. `-cascade` destroys sessions first, escrow-first: each transcript is written
   to `gs://$AGENTPLANE_BUCKET/transcripts/<sid>/` **before** anything dies.
3. Only then does the template (and its golden) go.

Re-creating an agent with the same name later is safe — even with a different
image, previously-suspended sessions restore from their own snapshots with
memory intact.
