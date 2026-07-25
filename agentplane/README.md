# agentplane

A control plane for vendor managed-agent APIs, with **Agent Substrate as the
primary sandbox backend**. This is the clean production-shaped seed; the proofs
live in `../substrate-agents-api` (Phases 1–3: multiplexing, session continuity,
skills, tool kinds, permission flows, identity + credential injection).

## Layering

```
Vendor managed-agent APIs        Anthropic (self_hosted — adapter built) · OpenAI BYO-sandbox (planned) · MCP
        │
agentplane (this repo)           internal/anthropicworker   vendor adapter (session→sandbox, skills, tools)
                                 internal/identity          ES256 identity JWTs (gateway/PEP validates + injects creds)
                                 cmd/agentplane             CLI: run adapters, manage sandboxes
        │
pkg/sandbox                      backend-neutral seam (Sandbox / Provider / ExecResult) — no vendor SDKs
        │
internal/backend/substrate       PRIMARY backend: Agent Substrate (ateapi + atenet)
```

Decisions (see `../substrate-agents-api/docs/DECISIONS.md`):
- **Substrate is primary.** `kubernetes-sigs/agent-sandbox` / GKE Agent Sandbox
  are references only — Substrate is the only backend today with *portable*
  in-memory checkpoint/restore multiplexing.
- The seam (`pkg/sandbox`) exists to keep upper layers backend-clean, not to
  invite backend sprawl.

## Run

Prereqs: a Substrate cluster with the sandbox demo deployed (kind: see
`../substrate-agents-api/docs/PHASE1.md`), and for the worker an Anthropic
Managed Agents **self_hosted** environment.

```bash
# port-forwards to the cluster (two terminals)
kubectl port-forward -n ate-system svc/api 8080:443
kubectl port-forward -n ate-system svc/atenet-router 8000:80

# manage sandboxes from the CLI
go run ./cmd/agentplane sbx exec -name demo-1 -- uname -a
go run ./cmd/agentplane sbx suspend -name demo-1

# run the Anthropic adapter
ANTHROPIC_ENVIRONMENT_ID=env_… ANTHROPIC_ENVIRONMENT_KEY=sk-ant-oat01-… \
  go run ./cmd/agentplane worker
```

Drive sessions with the drivers in `../substrate-agents-api/cmd/`
(`contdrive`, `multidrive`, …) — the adapter is wire-compatible with them.

## Roadmap
- [ ] Control API (serve) over the seam: tenants → atespaces, quotas, audit.
- [ ] OpenAI Agents SDK adapter (BYO-sandbox provider over `pkg/sandbox`).
- [ ] Credential-injecting gateway tool via `Config.ExtraTools` (Warden pattern,
      proven in Phase 3) → later atenet ext_proc when Substrate ships egress.
- [ ] GKE deploy: worker as in-cluster Deployment (Workload Identity, KMS keys).
