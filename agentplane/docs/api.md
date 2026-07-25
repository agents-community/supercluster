# Control-plane API (`agentplane serve`)

`serve` exposes the control plane as one HTTP endpoint — no kubectl, no
Host-header routing, no gRPC on the client side.

```bash
export AGENTPLANE_TOKEN=…                     # optional bearer auth
export OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4317   # optional tracing
agentplane serve -addr :7433
```

## Endpoints

| Method | Path | Notes |
|---|---|---|
| GET | `/healthz` | liveness (unauthenticated) |
| GET | `/v1/agents` | agents + phase + live session counts |
| POST | `/v1/agents` | body = AgentSpec (YAML/JSON) → `202`; poll GET until `Ready` (~30s golden bake) |
| DELETE | `/v1/agents/{id}` | `409` + session list if sessions live; `?cascade=true` escrows + removes them first |
| POST | `/v1/sessions` | `{"agent":"brain"}` → `201 {"id","agent","harness"}`; optional `"apiKey"` = BYO-key |
| PUT | `/v1/sessions/{id}/key` | set/replace the session's ephemeral BYO vendor key |
| GET | `/v1/sessions` | derived status: `sleeping / idle / running / unreachable` |
| GET | `/v1/sessions/{id}` | session object; adds `busy/queued/last_event_at` when awake |
| DELETE | `/v1/sessions/{id}` | cascade: escrow → delete actor → remove snapshots |
| POST | `/v1/sessions/{id}/suspend` | checkpoint in place; `409` mid-turn unless `?force=true` |
| POST | `/v1/sessions/{id}/events` | send a message (auto-wakes; retries 5xx wake races) |
| GET | `/v1/sessions/{id}/events` | persisted log, `?since=evt_…` cursor |
| GET | `/v1/sessions/{id}/events/stream` | SSE; honors `Last-Event-ID` on reconnect |

### Errors

Every error is a sanitized, stable JSON object — internal `kubectl`/`gcloud`/
gRPC detail is logged server-side, never returned:

```json
{ "error": { "code": "not_found", "message": "no such session" } }
```

`code` is a machine-readable slug you can branch on: `invalid_request` (400),
`unauthorized` (401), `not_found` (404), `conflict` (409), `backend_error`
(502), `timeout` (504). The 409 from `DELETE /v1/agents/{id}` additionally
carries a `sessions` array of the stranded session ids.

Bodies are capped at 1 MiB. If `AGENTPLANE_TOKEN` is set, all `/v1/*` routes
require `Authorization: Bearer <token>` (compared in constant time). Agent and
session ids are strictly validated before use — a flag-shaped name like
`--all` is rejected with `400 invalid_request`, never passed to a shell.

!!! note "Status probing never wakes a sleeping mind"
    List/get derive `sleeping` from the actor state alone — `/healthz` probes
    go only to awake minds, because probing a suspended actor would resume it.

## Bring-your-own key (ephemeral)

A session can run on the **user's own vendor key** instead of the shared one
the agent was configured with. Pass `apiKey` when creating the session (or
`PUT …/key` later):

```bash
curl -X POST …/v1/sessions -d '{"agent":"tutor","apiKey":"sk-…"}'
```

The key is **never stored**: it's forwarded once to the mind and held only in
that actor's memory for the session's life (it survives suspend/resume, dies
when the session is deleted). It is never written to the event log, never
logged server-side, never placed in a Secret or on a command line. Omit it and
the session falls back to the agent's shared key.

!!! warning "At-rest note"
    Because the key lives in the actor's memory, it is present in that actor's
    checkpoint. Keep the snapshot bucket CMEK-encrypted and treat the gVisor
    actor as the trust boundary.

## Distributed tracing

`serve` is the **trace root**. Every request gets a span; outbound calls carry
W3C `traceparent`, which Substrate's atenet (Envoy, natively OTel-instrumented)
joins and forwards all the way to the brain actor. Point everything at a
collector and you get one waterfall per request:

```
client → agentplane-serve ─▶ atenet Envoy ─▶ brain actor
              │                   │
              └──── OTLP ────▶ collector (yours: local or cloud) ─▶ Jaeger/Tempo/…
```

Setup:

1. **serve**: set `OTEL_EXPORTER_OTLP_ENDPOINT=http://<collector>:4317`
   (must be a URL, not bare host:port). Unset = tracing off, zero overhead.
2. **Substrate router** (one-time, per install): point atenet at the same
   collector —

    ```bash
    kubectl -n ate-system patch deploy atenet-router --type=json -p \
      '[{"op":"replace","path":"/spec/template/spec/containers/0/args/11","value":"--otlp-collector-address=<collector-host>:4317"}]'
    kubectl -n ate-system set env deploy/atenet-router -c atenet-router \
      OTEL_EXPORTER_OTLP_ENDPOINT=http://<collector-host>:4317
    ```

3. **In-cluster demo stack** (collector + Jaeger UI): apply Substrate's
   `manifests/ate-install/kind/otel-collector.yaml`, then
   `kubectl -n otel-system set env deploy/jaeger COLLECTOR_OTLP_ENABLED=true`
   (the manifest misses it) and open `svc/jaeger:16686`. Swap in your own
   collector by changing the addresses above — that address pair is the whole
   integration surface.

Sampling: serve samples **always** at the edge; Substrate services run
`ParentBased` samplers and follow serve's decision, so every API request is a
complete trace.
