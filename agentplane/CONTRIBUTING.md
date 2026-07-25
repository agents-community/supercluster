# Contributing to AgentPlane

AgentPlane is a control plane for **durable agent minds** on Agent Substrate:
each session is a persistent agent harness (Claude Code, Codex, …) running in a
gVisor sandbox that checkpoints and restores with full memory. This guide is for
people changing the code.

## The shape of the repo

This is a **monorepo**: the Go control plane, the Node brain image, and
standalone client apps live side by side, each with its own toolchain. The
contract between them is the HTTP API (the session-events dialect) — modules
never share code across language boundaries. Planned next: per-provider
infrastructure modules (Terraform) and CI/CD per module.

```
cmd/agentplane/         the Go control-plane CLI + HTTP API (`serve`)
  main.go               subcommand dispatch
  session.go            session lifecycle (mint/list/send/tail/suspend/delete)
  agent.go              agent lifecycle (AgentSpec → ActorTemplate, cascade rules)
  serve.go              the HTTP control-plane API (hardened; OTel trace root)
  configure.go          bring-your-own vendor key (Secret upsert)
  dispatcher.go         idle-mind auto-suspend loop
internal/
  agentspec/            the AgentSpec format + compiler (→ ActorTemplate JSON)
  naming/               the sess-<id> ⇄ actor-name convention (+ name validation)
  identity/             ES256 signer (dev-only)
  backend/substrate/    the Substrate sandbox backend (async-exec, HTTP hardening)
pkg/sandbox/            backend-neutral Sandbox/Provider seam (SDK-free)
brain/                  the in-actor server image (Node)
  server.mjs            thin HTTP layer (session-events dialect)
  runtime.mjs           the durable engine (queue, watchdog, event log) — shared
  harness/              per-vendor Harness strategies (claude-code, codex)
  identity.mjs          lazy actor identity + hand pairing
deploy/                 base install manifests (ns, RBAC, WorkerPool, brain template)
examples/               sample AgentSpecs (tutor, coder, codex)
test/smoke.sh           the live integration suite
andromeda/          ✦ the distributable TUI (TypeScript-less Ink, zero-build ESM)
                        — talks ONLY to serve's HTTP API; `npx`-runnable
docs/                   the published docs (MkDocs Material)
```

Two languages, one boundary: the **Go control plane** manages actors/templates
and speaks to the **Node brain** (in the actor) over the session-events HTTP
dialect. They share nothing but that wire format.

## Design principles (read before you add code)

1. **Minimal, but never at the cost of correctness.** The smallest change that
   is fully correct and verified — not the smallest diff.
2. **Foundations before scale.** Don't add multi-tenancy, autoscaling, or
   caching until a feature needs it. The control plane is deliberately
   **stateless**: sessions and agents are derived from Substrate + GCS, never
   from a local database. Don't introduce a second source of truth.
3. **Durability lives in one place.** Every reliability guarantee (the turn
   watchdog, poison-message guard, checkpoint-at-turn-boundary, escrow-before-
   delete) is written once — in `brain/runtime.mjs` and the session cascade —
   so all harnesses/agents inherit it. A new harness must not re-implement any
   of it (see below).
4. **The AgentSpec is ours; adapters translate.** Never leak a vendor's config
   surface into the AgentSpec. Add a field to `agentspec` and translate it in
   each harness adapter.
5. **Verify on the cluster.** Reliability claims are backed by a live drill in
   `docs/runbook-brain-reliability.md`, not by assertion.

## Building & testing

```bash
# Go: build, vet, unit tests (fast, no cluster)
go build ./... && go vet ./... && go test ./...

# Brain: syntax-check the Node modules
cd brain && for f in server.mjs runtime.mjs identity.mjs harness/*.mjs; do node --check "$f"; done

# Live integration suite (needs a cluster + port-forwards + AGENTPLANE_BUCKET)
export AGENTPLANE_BUCKET=<your-bucket>
./test/smoke.sh                          # full suite, per-test PASS/FAIL/SKIP
./test/smoke.sh t_memory_across_checkpoint   # one case

# Destructive reliability drills (manual) — docs/runbook-brain-reliability.md
```

Prereqs for the live paths: a Substrate cluster, port-forwards to `ateapi`
(`:8080`) and `atenet` (`:8000`), the brain template `Ready`, and a configured
vendor key (`agentplane configure`).

## Building the brain images

**One image per harness.** gVisor's gofer pays a per-file cost when a workload
is created, and a combined node_modules for every harness pushed restores past
Substrate's RPC deadline (observed live: 745 MiB multi-harness image failed to
restore; the 122 MiB pi-only image restores instantly). Each variant installs
only its own SDK; the lazy harness registry (`brain/harness/index.mjs`) makes
that safe.

| Variant | Dockerfile | Build config |
|---|---|---|
| claude-code | `brain/Dockerfile` | `cloudbuild-cc.yaml` → `agentplane-brain-cc` |
| codex | `brain/Dockerfile.codex` | `cloudbuild-codex.yaml` → `agentplane-brain-codex` |
| pi (node ≥22) | `brain/Dockerfile.pi` | `cloudbuild-pi.yaml` → `agentplane-brain-pi` |

Local `docker push` is unreliable on some networks (TLS resets); the project
builds through **Cloud Build**:

```bash
cd brain
gcloud builds submit --config cloudbuild-<variant>.yaml .
gcloud container images describe <image>:<tag> --format='value(image_summary.digest)'
```

Then pin the **digest** (never a tag — Substrate invalidates snapshots on image
change, so tags are rejected at spec validation) into the AgentSpec / template
and recreate the agent. Existing suspended sessions are unaffected (they restore
from their own snapshots); only new sessions get the new image.

## Adding a harness (the common contribution)

A harness adapts one agent CLI/SDK to the runtime. The runtime owns the queue,
watchdog, poison-guard, event log, and HTTP — a harness owns only vendor logic.
Full contract and a worked example: **docs/harness-interface.md**. In short:

1. Add `brain/harness/<name>.mjs` exporting a `Harness` object (`name`,
   `optionsFromSpec`, `async *run`).
2. Register it in `brain/harness/index.mjs`.
3. Teach the compiler its vendor key in `internal/agentspec` (`harnessKey`) and
   add the secret name to `deploy/brain.yaml.tmpl`'s
   `ate-api-server-env-sources` Role, or golden bakes stall in
   `ResumeGoldenActor`.
4. Add an `examples/<name>.yaml`, a compiler unit test, and a live drill.

## Coding conventions

- **Go**: standard `gofmt`; wrap errors with `%w`; every `exec.Command` to
  kubectl/gcloud uses `CommandContext` with a bounded timeout and puts
  user-derived values after `--`. Never send raw internal errors to HTTP
  clients — use `s.fail(...)` (logs detail server-side, returns a sanitized
  `{"error":{"code","message"}}`).
- **Node**: plain ESM `.mjs`, no build step; document non-obvious invariants
  (especially anything tied to a reliability finding) in a comment.
- **Comments** state constraints the code can't, not narration. Match the
  surrounding density.
- **Names** that reach kubectl (agents) must pass `naming.IsAgentName` — this is
  a security boundary, not cosmetics.

## Submitting

- Keep PRs scoped to one change; note which drills/tests you ran.
- If you touched the watchdog, checkpoint path, or cascade, re-run the relevant
  runbook drill and update its STATUS line.
- New reliability behavior ⇒ a new drill; new API surface ⇒ a docs update.

## Known deferred items (don't be surprised)

- Control-plane gRPC dials use `InsecureSkipVerify` (dev self-signed certs) —
  needs a real CA knob before production.
- List/get are O(N) over the actor set — fine at current scale.
- Egress from actors is currently unrestricted (allowlist is a Substrate-side
  roadmap item). See the strategy notes.
