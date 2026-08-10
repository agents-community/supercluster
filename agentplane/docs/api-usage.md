# Using the API

A task-oriented walkthrough of the AgentPlane control-plane API, with copy-paste
examples. For the exhaustive endpoint/error/auth reference, see
[Control-plane API](api.md).

The model is small: you create (or reuse) an **agent**, open a **session** on it,
send **messages**, and read the reply — as a log or a live stream. Sessions are
durable: step away, come back, and the mind resumes from its last checkpoint.

## Base URL and authentication

Every call goes to your deployment's base URL and (except `POST /v1/access`)
carries a bearer token:

```bash
export AGENTPLANE_URL="https://api.your-deployment.example"
export AGENTPLANE_TOKEN="apl_…"
```

Get a token with your allow-listed email (self-service; rate-limited):

```bash
curl -s -X POST "$AGENTPLANE_URL/v1/access" \
  -H 'content-type: application/json' \
  -d '{"email":"you@corp.com"}'
# → {"token":"apl_…","user":"you@corp.com"}
```

The token maps to **your** identity: you can only see and act on your own
sessions (a session you don't own returns `404`, never `403`, so ids can't be
enumerated).

!!! tip "andromeda"
    The `andromeda` CLI wraps all of this — `andromeda login`, then
    `andromeda --agent starter`. This page is for building against the API
    directly.

## Quickstart: run one command end to end

```bash
# 1. open a session on the built-in `starter` agent
SID=$(curl -s -X POST "$AGENTPLANE_URL/v1/sessions" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN" -H 'content-type: application/json' \
  -d '{"agent":"starter"}' | jq -r .id)

# 2. send a message (returns 202 immediately; the turn runs async)
curl -s -X POST "$AGENTPLANE_URL/v1/sessions/$SID/message" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN" -H 'content-type: application/json' \
  -d '{"message":"Create hello.txt containing the word world, then read it back."}'

# 3. read the reply (poll the log, or stream it — see below)
curl -s "$AGENTPLANE_URL/v1/sessions/$SID/message" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN" | jq '.events[].type'
```

`POST /message` returns `202` and wakes the session if it was asleep — you do not
manage lifecycle yourself.

## Reading a turn

Two ways to consume a turn's output.

**Poll the log** — idempotent, resumable with a cursor:

```bash
curl -s "$AGENTPLANE_URL/v1/sessions/$SID/message?since=evt_000007" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN"
```

**Stream it (SSE)** — live, with typing deltas; honors `Last-Event-ID` on
reconnect:

```bash
curl -N "$AGENTPLANE_URL/v1/sessions/$SID/message/stream" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN"
```

Event types you'll see: `user.message`, `agent.tool_use`, `agent.message`
(and `agent.message_delta` on the stream), `tool.approval_requested`,
`session.status_running` / `session.status_idle`. The `status_idle` event carries
`stop_reason` and `usage`.

## Approvals (human-in-the-loop)

If the agent's spec marks a tool `ask:`, the harness pauses before running it and
emits `tool.approval_requested` instead of executing. The call does **not** run
until you approve.

```bash
# what is this session waiting on?
curl -s "$AGENTPLANE_URL/v1/sessions/$SID/approvals" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN"
# → {"approvals":[{"request":"apr_…","tool":"Bash","input":{"command":"…"}}]}

# approve it — this wakes the session and the agent retries the call
curl -s -X POST "$AGENTPLANE_URL/v1/sessions/$SID/approvals/apr_…" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN" -H 'content-type: application/json' \
  -d '{"decision":"approve"}'
```

`decision` is `approve` or `deny`. The request id is bound to the exact call
(tool + arguments), so approving `echo hi` does not also approve `rm -rf /`.

## Agents

An agent is a reusable definition — system prompt, allowed tools, model. List
what's available:

```bash
curl -s "$AGENTPLANE_URL/v1/agents" -H "authorization: Bearer $AGENTPLANE_TOKEN"
# → [{"name":"starter","harness":"claude-code","phase":"Ready","sessions":2}]
```

Create or re-version one by POSTing an [AgentSpec](agents.md) (YAML or JSON).
Reusing a name mints a **new version**; existing sessions keep their version.

```bash
curl -s -X POST "$AGENTPLANE_URL/v1/agents" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN" -H 'content-type: application/yaml' \
  --data-binary @my-agent.yaml
# → 202; poll GET /v1/agents until phase is "Ready" (~30s golden bake)
```

## Credentials

Store third-party credentials (e.g. a GitHub token) in the per-user vault; values
are write-only (never returned) and injected **outside** the sandbox — the token
never enters actor memory.

```bash
# both "value" and "type" are required; type is one of git | header | env
curl -s -X PUT "$AGENTPLANE_URL/v1/credentials/gh-token" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN" -H 'content-type: application/json' \
  -d '{"value":"ghp_…","type":"git"}'

curl -s "$AGENTPLANE_URL/v1/credentials" -H "authorization: Bearer $AGENTPLANE_TOKEN"
# → {"credentials":["gh-token"]}   (names only)
```

An agent references a credential by name (see [AgentSpec](agents.md)); serve
resolves it per session for that agent.

## Session lifecycle

Sessions are durable and self-managing, but you can control them:

```bash
# suspend in place (checkpoint); 409 mid-turn unless ?force=true
curl -s -X POST "$AGENTPLANE_URL/v1/sessions/$SID/suspend" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN"

# delete: escrow the transcript, delete the actor, remove snapshots
curl -s -X DELETE "$AGENTPLANE_URL/v1/sessions/$SID" \
  -H "authorization: Bearer $AGENTPLANE_TOKEN"
```

You rarely need `suspend` — an idle session checkpoints and releases its worker on
its own, and the next message wakes it in ~1s.

## Errors

Errors are JSON: `{"error":{"code":"…","message":"…"}}`. Common codes:

| HTTP | code | meaning |
|---|---|---|
| 401 | `unauthorized` | missing/invalid bearer token |
| 404 | `not_found` | unknown route, or a session you don't own |
| 409 | `conflict` | delete-with-live-sessions (`?cascade=true`), or suspend mid-turn (`?force=true`) |
| 502 | `backend_error` | the control plane or actor was unreachable — safe to retry |
| 504 | `timeout` | the operation didn't complete in time |

`POST /message` already retries transient wake races internally, so a `202`
means the message was accepted, not that the turn has finished.
