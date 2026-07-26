# Welcome to Andromeda 🌌

**Andromeda** is a beautiful terminal for talking to **durable agent minds**. You
open a conversation, the mind does real work (runs code, edits files, calls
tools — all in an isolated sandbox), and when you leave it *keeps its memory*.
Come back tomorrow, re-attach, and it picks up exactly where you left off.

You bring two things: an **access token** (from whoever invited you) and your
own **model API key** (used only for your session, never stored).

---

## 60-second quickstart

```bash
# 1. Log in once — it asks for your host's URL and your email, then saves your
#    token to ~/.andromeda (you won't need to paste it again).
npx @agentplane/andromeda login

# 2. Bring your own model key (only the one your agent uses)
export ANTHROPIC_API_KEY="sk-ant-…"             # or OPENAI_API_KEY / GEMINI_API_KEY

# 3. Talk to a mind — your token is remembered, nothing to paste
npx @agentplane/andromeda --agent migrator      # start a new conversation
npx @agentplane/andromeda                        # or list agents & live sessions
```

That's it — no clone, no `npm install`, no token juggling. `login` fetches your
token from your email and stores it; every later run just works.

> `andromeda whoami` shows who you're logged in as · `andromeda logout` clears it.
> Prefer no install? `npm i -g @agentplane/andromeda`, then just run `andromeda`.

---

## What you need from your host

| Thing | Looks like | What it's for |
|-------|-----------|---------------|
| **Endpoint URL** | `https://YOUR-HOST` | where the control plane lives — you enter it at `login` |
| **On the allowlist** | your email | so `login` can issue your token — ask your host to add it |
| **An agent name** | e.g. `migrator` | which mind to talk to (run with no args to list) |

`login` handles the token for you and saves it to `~/.andromeda`. Advanced: any
run can be overridden with `--url` / `--token` flags or `ANDROMEDA_URL` /
`ANDROMEDA_TOKEN` env vars — they win over the saved config.

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

## Using it

**Start or resume a conversation**

```bash
andromeda --agent migrator        # new mind
andromeda --session sess-abc123     # re-attach to one you started before
andromeda                           # list everything, then pick
```

**While chatting**

| Key | Does |
|-----|------|
| **enter** | send your message |
| **ctrl+s** | suspend the mind in place (frees resources; wakes on your next message) |
| **esc** | detach — the mind keeps its full memory; re-attach any time |

When you detach, Andromeda prints the exact command to come back to that mind.

---

## See why "durable" matters (try this)

The `migrator` agent shows the whole point — a task too big for one sitting:

1. **Start it:** `npx @agentplane/andromeda --agent migrator`
2. **Give it real work:**
   > *"Clone `github.com/<some-repo>`, then upgrade it from `<old>` to `<new>`. Write a plan first, then start working through it — I'll check back."*
3. Watch it clone the repo into its sandbox, write a migration plan, and begin —
   editing files and running tests on the hand.
4. **Walk away:** hit `esc` to detach (close your laptop, go to a meeting).
5. **Come back later:** `andromeda --session sess-…` — it's still mid-migration,
   remembers the plan, the repo state, what's already converted and what's next.
   Just say *"continue."*

A stateless bot would start from zero every time. This one never lost the thread
— that's the durable mind.

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

---

## One command to remember

```bash
npx @agentplane/andromeda login                          # once — saves your token

ANTHROPIC_API_KEY=sk-ant-your_key \
npx @agentplane/andromeda --agent migrator               # anytime after
```

Welcome aboard. Talk to a mind, leave, come back — it remembers. 🌌
