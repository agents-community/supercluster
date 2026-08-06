# Welcome to Andromeda 🌌

**Andromeda** is a beautiful terminal for talking to **durable agent minds**. You
open a conversation, the mind does real work (runs code, edits files, calls
tools — all in an isolated sandbox), and when you leave it *keeps its memory*.
Come back tomorrow, re-attach, and it picks up exactly where you left off.

You bring your **email** (your host adds it to an allowlist) and, optionally,
your own **model API key**. `login` turns your email into a saved token; the key
is used only for your session, held in memory, never stored.

---

## Before you start

**On your machine:** Node.js **18.17 or newer** (`node -v`). That is the whole
list. No clone, no Docker, no `kubectl`, no cluster access — andromeda talks to
the control plane over HTTPS with a URL and a token.

**From your host:**

| Thing | Looks like | What it's for |
|-------|-----------|---------------|
| **Endpoint URL** | `https://YOUR-HOST` | where the control plane lives — you enter it at `login` |
| **On the allowlist** | your email | so `login` can issue your token — ask your host to add it |

**Optional:** your own model API key. Without one, your agent falls back to the
host's shared key if there is one.

---

## Step 1 — Install and log in

```bash
npm install -g @agentsupercluster/andromeda
andromeda login          # asks for the URL and your email, saves your token
```

`login` writes your token to `~/.andromeda`, so nothing needs pasting again.
Prefer not to install? Every command works as `npx @agentsupercluster/andromeda …`.

```bash
andromeda whoami         # who you are and which cluster you're pointed at
andromeda agent ls       # what already exists here
```

> Any run can be overridden with `--url` / `--token`, or `ANDROMEDA_URL` /
> `ANDROMEDA_TOKEN` — they win over the saved config.

---

## Step 2 — Define your agent

An agent is a YAML spec. The two fields that make it *yours* are
**`systemPrompt`** (what it is for) and its **tool policy** (what it may do).

Start from one that already runs here rather than writing from scratch:

```bash
andromeda agent get starter > my-agent.yaml
```

```yaml
name: reviewer                    # your own name — reusing one makes a new VERSION
harness: claude-code              # or codex, pi
model: claude-sonnet-5
image: gcr.io/…@sha256:…          # keep whatever `agent get` gave you (see below)
hand: true                        # tools run in the sandbox, not in the reasoning layer

systemPrompt: >-
  You review pull requests. Be specific and cite line numbers. Ask before
  changing anything outside the diff.

allow: [WebFetch]                 # auto-approved (see the tool list below)
deny:  [Bash, Write, Edit, Read, Grep, Glob, NotebookEdit]
turnDeadlineSeconds: 600
```

Then apply it and talk to it:

```bash
andromeda agent create -f my-agent.yaml
andromeda agent ls                       # wait for phase: Ready (a snapshot bakes)
andromeda --agent reviewer
```

> **Keep the `image:` digest you were given.** It pins the brain image, and the
> digest differs per platform — one copied from anywhere else is wrong here.

### The tools you can name

Use these exact names in `allow` / `deny`:

| Tool | Does |
|---|---|
| `Bash` | run shell commands |
| `Read` · `Write` · `Edit` | read and change files |
| `Glob` · `Grep` | find files by name · search their contents |
| `NotebookEdit` | edit Jupyter cells |
| `WebFetch` · `WebSearch` | fetch a URL · search the web |
| `TodoWrite` | keep a task list across a long job |
| `Agent` | spawn a subagent |

`allow` lets a tool run; `deny` removes it; `ask` runs it only after you say yes;
anything in none of them will not run. So list what your agent needs. The hand's
own tools are on automatically.

```yaml
allow: [WebFetch]
ask:   [Bash]                     # you approve each call before it runs
deny:  [Write, Edit]
```

A gated call stops the agent and shows up in your terminal. `/approve <id>` and
it carries on from where it stopped — you can be away when it asks; the request
is still waiting when you come back.

To add an MCP server, declare it and scope its tools with `server/tool`:

```yaml
mcp:
  github:
    url: https://api.githubcopilot.com/mcp/
    headersFrom:
      Authorization: {credential: gh-token, format: "Bearer {}"}

allow: [github/list_issues]       # this one is permitted; github's others are not
```

Naming any tool of a server turns that server into an allow-list. `github/*`
takes the whole server.

Full reference, including how each field maps onto the harness:
[`agentplane/docs/agents.md`](../agentplane/docs/agents.md).

---

## Step 3 — Talk to it

```bash
andromeda --agent reviewer          # new mind
andromeda --session sess-abc123     # re-attach to one you started before
andromeda                           # list everything, then pick
```

**While chatting** — press **enter** to send. Everything else is a typed
command; anything starting with `/` goes to the terminal, never to the agent.

| Command | Does |
|-----|------|
| **/sleep** | suspend the mind in place (frees resources; wakes on your next message) |
| **/approvals** | tool calls waiting on your decision |
| **/approve** `<id>` | let a waiting call run (`/deny <id>` refuses it) |
| **/sessions** | list your sessions for this agent |
| **/usage** | tokens & cost this session |
| **/help** | list all commands |
| **/quit** | detach — the mind keeps its full memory; re-attach any time |

When you detach, Andromeda prints the exact command to come back to that mind.

---

## Bringing your own model key

Andromeda never asks you to hand your key to anyone. It reads it from your
environment and forwards it **only for your session, held in memory, never
written to disk or logs**. Set the one that matches your agent's model:

```bash
export ANTHROPIC_API_KEY="sk-ant-…"     # Claude agents
export OPENAI_API_KEY="sk-…"            # OpenAI agents
export GEMINI_API_KEY="…"               # Gemini agents
```

If you don't set a key, the agent falls back to the host's shared key (if one is
configured).

---

## See why "durable" matters (try this)

The `starter` agent shows the whole point — a task too big for one sitting:

1. **Start it:** `npx @agentsupercluster/andromeda --agent starter`
2. **Give it real work:**
   > *"Clone `github.com/<some-repo>`, then upgrade it from `<old>` to `<new>`. Write a plan first, then start working through it — I'll check back."*
3. Watch it clone the repo into its sandbox, write a migration plan, and begin —
   editing files and running tests on the hand.
4. **Walk away:** type `/quit` to detach (close your laptop, go to a meeting).
5. **Come back later:** `andromeda --session sess-…` — it's still mid-migration,
   remembers the plan, the repo state, what's already converted and what's next.
   Just say *"continue."*

A stateless bot would start from zero every time. This one never lost the thread
— that's the durable mind.

---

## Work with your own GitHub repo

`starter` can clone, edit, and **push your repos** — the work happens on its
sandboxed hand, and your token is pulled in *just for the task*, never in the
model's context or your terminal.

1. **Store a token once** — a fine-grained GitHub PAT scoped to the repo:
   ```bash
   npx @agentsupercluster/andromeda cred set gh-token
   # prompts: host (github.com), username (x-access-token), token (your PAT)
   ```
2. **Point `starter` at your repo:**
   > *"Clone github.com/me/myapp, add tests for the auth module, run them, then push a branch."*

Public repos need no token — just ask it to clone. `cred ls` shows what you've
stored; `cred rm gh-token` removes it.

---

## The agents you can talk to

| Agent | Engine | Good for |
|---|---|---|
| **`starter`** | Claude (claude-code) | the full experience — runs code, git, files on its hand; **start here** |
| `codex` | OpenAI Codex | same platform, a different agent engine |
| `pi` | model-agnostic (pi) | one harness, any model provider |

Run `andromeda` with no arguments any time to see the live list.

---

## Use it from your own code

Andromeda is a client, not the product — everything it does is the HTTP API, so
you can drive a durable mind from a script, a service, or CI. A URL and a bearer
token are the whole contract.

```bash
export ANDROMEDA_URL=https://YOUR-HOST
export ANDROMEDA_TOKEN=apl_…            # andromeda whoami shows yours
```

**Create a session and send a message**

```bash
SID=$(curl -sS -X POST "$ANDROMEDA_URL/v1/sessions" \
  -H "Authorization: Bearer $ANDROMEDA_TOKEN" -H 'Content-Type: application/json' \
  -d '{"agent":"starter"}' | jq -r .id)

curl -sS -X POST "$ANDROMEDA_URL/v1/sessions/$SID/events" \
  -H "Authorization: Bearer $ANDROMEDA_TOKEN" -H 'Content-Type: application/json' \
  -d '{"message":"Clone github.com/me/app and run the tests"}'
```

Sending returns **202** — the turn runs asynchronously. Read the result by
streaming, or by polling the log with a cursor.

**Stream the turn** (SSE; `Last-Event-ID` is honoured, so a dropped connection
resumes without replaying)

```bash
curl -sN "$ANDROMEDA_URL/v1/sessions/$SID/events/stream" \
  -H "Authorization: Bearer $ANDROMEDA_TOKEN"
```

**Or poll from a cursor** — every event has a monotonic id, and the log is
durable, so this works across suspend, restart and reconnect:

```bash
curl -sS "$ANDROMEDA_URL/v1/sessions/$SID/events?since=evt_000042" \
  -H "Authorization: Bearer $ANDROMEDA_TOKEN"
```

### Endpoints

| Method | Path | Does |
|---|---|---|
| `POST` | `/v1/access` | exchange an allowlisted email for a token (no auth) |
| `GET` | `/v1/agents` | list agents and the version each serves |
| `POST` | `/v1/agents` | create or re-version an agent (`Content-Type: application/yaml`) |
| `GET` | `/v1/agents/{name}/versions` | version history, each with its spec verbatim |
| `DELETE` | `/v1/agents/{name}` | delete (409 + session list unless `?cascade=true`) |
| `POST` | `/v1/sessions` | mint a session: `{"agent":"…"}` |
| `GET` | `/v1/sessions` | your sessions |
| `GET` | `/v1/sessions/{id}` | status, queue depth, usage |
| `POST` | `/v1/sessions/{id}/events` | send a message → `202` |
| `GET` | `/v1/sessions/{id}/events` | the durable log, `?since=evt_…` |
| `GET` | `/v1/sessions/{id}/events/stream` | SSE |
| `PUT` | `/v1/sessions/{id}/key` | set your own model key for this session (memory only) |
| `POST` | `/v1/sessions/{id}/suspend` | sleep it now (it wakes on the next message) |
| `DELETE` | `/v1/sessions/{id}` | end it |
| `GET` | `/v1/sessions/{id}/approvals` | calls waiting on a human |
| `POST` | `/v1/sessions/{id}/approvals/{req}` | `{"decision":"approve"\|"deny"}` |
| `PUT` | `/v1/credentials/{name}` | store a secret (write-only; values never returned) |
| `GET` | `/v1/credentials` | names only |

Errors are JSON: `{"error":{"code":"…","message":"…"}}`. A session you do not
own answers **404**, not 403 — ids cannot be probed.

Full reference: [`agentplane/docs/api.md`](../agentplane/docs/api.md).

---

## Changing an agent later

`agent create` with an existing `name:` adds a **version**. Sessions already
running stay on the version they started with, so nobody's mind breaks under
them; new sessions get the new one.

```bash
andromeda agent get reviewer --version 2 > v2.yaml   # fetch any earlier version
andromeda agent rm reviewer                          # refuses if live sessions would die
```

---

## What's actually happening (the good part)

- **Durable mind** — the agent's memory lives in a checkpointed process that
  wakes in about a second. Detaching or suspending doesn't lose the thread.
- **A separate "hand"** — every command, file write, and tool call runs in an
  isolated sandbox paired to your mind, **not** on your machine and not in the
  reasoning layer. Anything the agent builds lives in that sandbox's workspace.
- **Credentials that stay put** — if your agent needs, say, a GitHub token, your
  host stores it in a vault and the sandbox pulls it in *for the task only*. It
  never passes through the reasoning layer or your terminal.
- **Not just coding** — an agent is whatever its harness makes it: a coding
  assistant, a deterministic workflow, a research task. Same durable mind, same
  safe hand.

---

## Troubleshooting

| Symptom | Fix |
|---------|-----|
| `cannot reach the control plane` | check `ANDROMEDA_URL`; confirm the endpoint with your host |
| `missing or invalid bearer token` | run `andromeda login` again (or `andromeda whoami` to check) |
| `isn't on the allowlist` at login | ask your host to add your email, then retry `andromeda login` |
| agent replies but does nothing useful | make sure your model key env var is `export`ed (a plain `VAR=…` won't reach the process) |
| first message is slow | that's a sleeping mind waking from its checkpoint — it's quick after that |
| `node: bad option` / crashes | you need Node 18.17+ (`node -v` to check) |
| `allows tools that no connected MCP server exposes` | a `server/tool` entry in `allow` is misspelled — the error names the servers that did connect |
| a tool you expected never runs | add it to `allow` — unlisted tools don't run |
| `sh: 1: andromeda: not found` from `npx` | you're inside a checkout of andromeda itself. Run it from any other directory, or `npm install -g` |

---

## One command to remember

```bash
npm install -g @agentsupercluster/andromeda   # once
andromeda login                               # once — saves your token

andromeda agent get starter > my-agent.yaml   # define your own: prompt + tools
andromeda agent create -f my-agent.yaml
andromeda --agent my-agent                    # anytime after
```

Welcome aboard. Talk to a mind, leave, come back — it remembers. 🌌
