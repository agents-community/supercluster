# Observability — traces & cold-start metrics

Everything exports OTLP to whatever collector you point it at
(`OTEL_EXPORTER_OTLP_ENDPOINT`). See [Control-plane API → tracing](api.md) for
the trace setup; this page is about **metrics**, especially cold-start.

## Cold start, measured in two layers

"Cold start" spans two things, and you want both:

| Layer | Metric | Emitted by | What it captures |
|---|---|---|---|
| **Infra** | `atenet.router.route.duration` (histogram, s) | **Substrate** (out of the box) | request received → target worker resolved — for a sleeping actor this **includes the checkpoint restore** |
| **Product** | `agentplane.session.wake_latency` (histogram, s, `cold` attr) | **serve** | message sent → the mind **accepts** it — the user-perceived wake, which Substrate's metric stops short of |

Substrate gives you the infra half for free; serve adds the product half,
because `route.duration` explicitly ends at "worker resolved, excluding actor
execution and response" — it can't see the harness spawn / first-token tail
that makes cold ≈9s vs warm ≈1–2s.

Also available from Substrate: `atelet.snapshot.size` (histogram, bytes — the
variable that drives restore time), and restore timing as spans
(`ResumeActor` / `CallAteletRestore`).

## Querying (Prometheus names)

OTLP histograms surface in Prometheus with a unit suffix and `_bucket/_sum/_count`:

```promql
# p95 product cold-start (cold wakes only), last 5m
histogram_quantile(0.95,
  sum by (le) (rate(agentplane_session_wake_latency_seconds_bucket{cold="true"}[5m])))

# p95 infra restore-to-routable, per agent
histogram_quantile(0.95,
  sum by (le, actor_template_name) (rate(atenet_router_route_duration_seconds_bucket[5m])))
```

## Setup

serve initializes an OTLP **metric** exporter whenever
`OTEL_EXPORTER_OTLP_ENDPOINT` is set (same var as traces); unset = no-op, zero
overhead. In-cluster it points at the managed collector; locally, at your
port-forwarded one. A cold send records into the `cold="true"`
series (~2.3s, 3s bucket) while warm sends stay sub-second.

## LLM-level monitoring (Claude Code telemetry)

The trace layers above stop at the actor boundary — they show a request
reaching the mind, not the model call inside it. Claude Code's own OpenTelemetry
fills that in. `claude-code` agents export (to the same collector) once
telemetry injection is turned on:

```bash
# 1. point serve at the collector — the compiler reads this env at agent-create
#    time and injects Claude Code's OTEL vars into every brain it bakes.
kubectl -n agentplane set env deploy/agentplane-serve \
  AGENTPLANE_OTEL_ENDPOINT=http://opentelemetry-collector.otel-system.svc:4317
# 2. RE-CREATE existing agents so the bake picks it up (new agents get it
#    automatically). Verify the env landed:
kubectl -n agentplane get actortemplate <agent> \
  -o jsonpath='{range .spec.containers[0].env[*]}{.name}{"\n"}{end}' | grep CLAUDE_CODE
```

Then each agent exports:

- **Metrics** — per model and query source (`main` vs `auxiliary`):
  - `claude_code.token.usage` (by type: input / output / cacheCreation / cacheRead)
  - `claude_code.cost.usage` (USD), `claude_code.session.count`, `claude_code.active_time.total`
- **Traces** (enhanced beta) — a **`claude_code.llm_request` span per model call**
  under a `claude_code.interaction`, so you see each Anthropic API round-trip and
  its latency. These land as a separate `claude-code` service in Jaeger (not yet
  stitched into the `agentplane-serve` waterfall — correlate by time/session).

```promql
# cost per model, last hour
sum by (model) (rate(claude_code_cost_usage_USD_total[1h]))
# output tokens per model
sum by (model) (rate(claude_code_token_usage_tokens_total{type="output"}[5m]))
```

**Privacy:** `OTEL_LOGS_EXPORTER=none` — prompt/response *content* is NOT
exported (only usage/timing). Do not flip logs on for BYO-key/multi-tenant
agents without checking what the events contain.

## Viewing it live (Jaeger)

```bash
kubectl -n otel-system port-forward svc/jaeger 16686:16686
# then open http://localhost:16686
```

You'll see five services reporting: `agentplane-serve`, `atenet-router`,
`atenet-router-envoy`, `claude-code`, and `agentplane-hand`. To read a turn's
latency:

- Pick **`claude-code`** → open a `claude_code.interaction` trace. Inside it,
  each **`claude_code.llm_request`** span is one Anthropic API round-trip, tagged
  `gen_ai.request.model` and its duration. (A span "blocked on user" is just the
  idle wait for the next human message — not latency.)
- Pick **`agentplane-hand`** → **`hand.tool <name>`** spans, one per tool
  execution (bash/write/edit/grep/git/…), tagged `hand.tool` + `hand.tool.is_error`.
  These fill the brain's "blocked on tool" window with the real hand-side work
  (they nest under the brain's turn when traceparent is propagated).
- Pick **`agentplane-serve`** / **`atenet-router`** for the request path into the
  mind (route/resolve + `ResumeActor` on a cold wake).

**What a turn actually looks like** (verified — a short prompt on the `starter`
agent, model `sonnet`):

| span | model | ~dur |
|---|---|---|
| `claude_code.llm_request` (auxiliary — tool-search) | `claude-haiku-4-5` | 4.6s |
| `claude_code.llm_request` (main answer) | `claude-sonnet-5` | 4.9s |
| `claude_code.interaction` (whole turn) | — | ~9s |

Three takeaways this surfaces: (1) the **main model resolves as expected** — the
`sonnet` alias → `claude-sonnet-5`; (2) Claude Code makes an **auxiliary Haiku
call + repeated `ToolSearch`** before the answer (it discovers the hand's MCP
tools lazily), which dominates per-turn latency; (3) the **hand's own execution
is ~0–20ms** (`hand.tool` spans) — so latency is the LLM/ToolSearch, *not* the
hand or the cluster. The first turn on a cold session adds `ResumeActor` +
harness spawn on top.

No cluster access for the viewer? Query the Jaeger API from any in-cluster pod:
`curl -s 'http://jaeger.otel-system.svc:16686/api/traces?service=claude-code&limit=5&lookback=30m'`.

## Deeper breakdown (planned)

`wake_latency` is measured at "mind accepts the message." To split
queue-wait vs restore vs **first-token**, add brain-side metrics (the brain
already timestamps `user.message → status_running → agent.message`, so the
split is derivable) — a future addition, kept out of the images until needed.
