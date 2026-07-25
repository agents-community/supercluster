# Contributing to Andromeda

Thanks for helping build Andromeda — the terminal client for **durable agent
minds** on AgentPlane. It's a small, dependency-light [Ink](https://github.com/vadimdemedes/ink)
app, so it's an easy codebase to jump into.

## Ground rules

- **Be kind and constructive.** Assume good intent; keep reviews about the code.
- **Andromeda is a *client*.** It talks to the AgentPlane control plane only over
  its HTTP API. Don't add cluster-, cloud-, or provider-specific logic here — that
  belongs on the server side.
- **No secrets, ever.** Never commit tokens, API keys, or a specific host's
  endpoint/URL. Connection details come from the user's environment at runtime.

## Getting set up

You need **Node 18.17+** and access to a running AgentPlane control plane (ask a
maintainer for a URL + token, or point at your own).

```bash
git clone https://github.com/agents-community/<this-repo>.git
cd <this-repo>
npm install

export ANDROMEDA_URL=https://your-host        # your control-plane endpoint
export ANDROMEDA_TOKEN=apl_…                   # your access token
node src/cli.mjs                               # list agents/sessions
node src/cli.mjs --agent <name>               # start a conversation
```

## Project layout

| File | What it does |
|------|--------------|
| `src/cli.mjs` | entry point — parses flags/env, resolves the session, mounts the UI |
| `src/api.mjs` | the HTTP client for the control-plane API (sessions, messages, streaming) |
| `src/app.mjs` | the Ink UI — the chat view, banner, and streaming render loop |
| `src/banner.txt` | the ASCII wordmark shown on start |

## How we build

- **Zero build step.** Plain ESM (`.mjs`), run directly by Node — no bundler, no
  transpile. Keep it that way.
- **No JSX / no TypeScript.** UI is built with `React.createElement`, aliased to
  `h` at the top of each file. Match that style.
- **Stay dependency-light.** Ink + React are the core; think hard before adding a
  new runtime dependency.
- **Match the surrounding code.** Small focused modules, clear names, comments
  that explain *why* not *what*.

## Testing your change

There's no automated suite yet (a good first contribution!). For now, test
manually against a control plane and cover the core loop:

- list mode (`node src/cli.mjs` with no args)
- start a new session (`--agent <name>`) and send a few messages
- detach (`esc`) and re-attach (`--session sess-…`) — history should replay
- suspend (`ctrl+s`) and confirm the next message wakes the mind

Because this is a TUI, include a short **terminal recording or screenshot** in
your PR so reviewers can see the result.

## Submitting a pull request

1. **Fork** and create a branch off `main`: `feat/…`, `fix/…`, or `docs/…`.
2. **Keep it focused** — one logical change per PR. Small PRs get merged faster.
3. **Commit clearly** — imperative subject line ("Add reconnect backoff"), a body
   explaining the why when it's not obvious.
4. **Fill in the PR template** — what changed, why, and how you tested it.
5. **Open the PR against `main`** and link any related issue.

See [PR guidance](#pull-request-guidance) below for what a good PR looks like.

## Reporting bugs & proposing features

Open an issue. For a bug, include: what you ran, what you expected, what happened,
and your Node version (`node -v`). For a feature, describe the user problem first,
then your proposed solution.

---

## Pull request guidance

A PR is easiest to review — and quickest to merge — when it:

- **Does one thing.** Refactors, features, and formatting churn in separate PRs.
- **Explains the why.** The diff shows *what*; the description should say *why* and
  what you considered.
- **Shows the result.** For any visible change, a terminal recording/screenshot.
- **Keeps the client a client.** No server assumptions, no hardcoded endpoints, no
  secrets.
- **Passes the manual checklist** (see the template) — it runs, matches the style,
  and updates docs if behavior changed.
- **Leaves the tree green.** No stray `console.log`, no committed `node_modules`,
  no `.env`.

Reviews aim to be fast and friendly. Expect questions — they're about making the
change solid, not blocking it. Maintainers may push small follow-up commits to
get a PR over the line; say so in the PR if you'd rather they didn't.
