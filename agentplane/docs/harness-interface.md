# The Harness interface

A **harness** adapts one agent CLI/SDK (Claude Code, Codex, …) to the AgentPlane
runtime. The design is a **Strategy pattern**: the runtime owns everything
durability-critical and harness-agnostic; a harness owns only the vendor
specifics. This is what lets a second or third harness inherit the watchdog,
the poison-message guard, checkpoint-at-turn-boundary, and the event log
**for free** — none of that is re-implemented per vendor.

## Who owns what

| Concern | Owner |
|---|---|
| Input queue + the turn clock (stamped at pull time) | runtime (`runtime.mjs`) |
| Turn watchdog (deadline → abort → restart) | runtime |
| Poison-message guard (re-queue in-flight once) | runtime |
| Event log (`events.jsonl`) + SSE fan-out | runtime |
| Vendor session-id persistence (resume-by-id) | runtime |
| HTTP (session-events dialect) | `server.mjs` |
| AgentSpec → vendor options | **harness** |
| One turn: vendor stream → normalized events | **harness** |

## The contract

A harness is an object with three members (`brain/harness/index.mjs` documents
the typedefs):

```js
export const myHarness = {
  name: "my-harness",

  // AgentSpec → vendor options. Pure; unit-test it in isolation.
  optionsFromSpec(spec, /* … */) { return { /* vendor options */ }; },

  // Long-lived. Pull messages from `inputs` (each pull stamps the turn clock —
  // do NOT buffer ahead), yield normalized events, persist the vendor id.
  async *run(inputs, ctx) { /* … */ },
};
```

**`ctx`** gives you: `ctx.spec`, `ctx.workdir`, `ctx.sessionId` (for resume),
`ctx.setSessionId(id)` (persist a new one), and `ctx.signal` — an `AbortSignal`
that fires when the watchdog tears down an over-deadline turn. If your SDK can
actively interrupt, listen on `ctx.signal` and do so; otherwise check
`ctx.signal.aborted` between events.

**Normalized events** you `yield` (the runtime writes them to the wire):

```js
{ type: "agent.message",       content: [{ type: "text", text }] }
{ type: "agent.tool_use",      name, input }
{ type: "session.status_idle", stop_reason: { type }, usage }
```

**Throwing** from `run()` ends the turn: the runtime emits `session.error`,
re-queues the in-flight message **once** (so work behind a wedged turn survives,
but a poison message can't loop forever), and restarts `run()` with
resume-by-id.

## Three shapes, same interface

The shipped harnesses show the range the interface covers:

- **claude-code** holds a single **persistent** SDK stream across turns (the
  conversation lives in process memory — this is what gives ~1s warm recall
  across a checkpoint). Its `run()` starts one `query()` and iterates it; the
  watchdog interrupts via `q.interrupt()` on `ctx.signal`.
- **codex** spawns the CLI **per turn**; continuity is the thread id +
  `~/.codex` rollout files on the actor fs. Its `run()` is a
  `for await (const inp of inputs)` loop that resumes the thread each turn.
- **pi** (model-agnostic — `model: "provider/model-id"` picks ANY vendor; the
  compiler wires the matching key env from the provider prefix) drives a
  persistent `AgentSession`; continuity is the session JSONL whose file path
  is the resume token. `session.abort()` gives a real watchdog interrupt.

Each is well under 100 lines. None contains a watchdog, a queue, or an event
log.

## One image per harness

Each harness ships in its own image (`brain/Dockerfile`, `Dockerfile.codex`,
`Dockerfile.pi`) installing only its SDK — the registry loads harnesses
lazily, so absent SDKs are never imported. This is a hard requirement, not
taste: gVisor pays a per-file cost at workload creation, and a combined
image's node_modules pushed restores past Substrate's RPC deadline. Check the
SDK's `engines` field for the Node base (pi needs ≥22).

## Adding one — checklist

1. **`brain/harness/<name>.mjs`** — export the `Harness` object.
2. **`brain/harness/index.mjs`** — add it to `HARNESSES`.
3. **`internal/agentspec/agentspec.go`** — add the vendor key to `harnessKey`
   (env var + default Secret name; for multi-provider harnesses like pi, see
   `providerKey`/`keyForSpec`). This wires `optionsFromSpec`'s key and the
   `AGENTPLANE_HARNESS` env at compile time.
3b. **`brain/Dockerfile.<name>` + `cloudbuild-<name>.yaml`** — the harness's
   own image variant (see "One image per harness" above).
4. **`deploy/brain.yaml.tmpl`** — add the Secret name to the
   `ate-api-server-env-sources` Role's `resourceNames`. **If you skip this, the
   golden bake stalls in `ResumeGoldenActor`** with a "secrets … is forbidden"
   error in the `ate-api-server` logs.
5. **`brain/package.json`** — add the SDK dependency.
6. **`examples/<name>.yaml`**, a compiler unit test (`agentspec_test.go`), and a
   live drill in the runbook.
7. Build the image (Cloud Build), pin the digest, `agentplane agent create`.

## Tool-policy translation

Tool policy is **user-controlled** via the AgentSpec `allow`/`deny` lists — the
platform imposes none (gVisor is the safety boundary). Each harness translates
that to its own model:

- claude-code: `allow`/`deny` map straight to the SDK's allowed/disallowed tools.
- codex: has sandbox *modes*, not allow-lists — empty `allow` → `read-only`,
  any entry → `danger-full-access`.

Keep that translation inside `optionsFromSpec` so the AgentSpec stays
vendor-neutral.
