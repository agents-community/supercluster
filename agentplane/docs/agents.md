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
