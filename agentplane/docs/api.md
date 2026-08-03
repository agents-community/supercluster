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
| POST | `/v1/access` | self-service token: `{"email":"you@…"}` → token if the email is allowlisted (unauthenticated; idempotent per email) |
| GET | `/v1/agents` | agents + phase + live session counts |
| POST | `/v1/agents` | body = AgentSpec (YAML/JSON) → `202`; poll GET until `Ready` (~30s golden bake) |
| DELETE | `/v1/agents/{id}` | `409` + session list if sessions live; `?cascade=true` escrows + removes them first |
| POST | `/v1/sessions` | `{"agent":"brain"}` → `201 {"id","agent","harness"}`; optional `"apiKey"` = BYO-key |
| PUT | `/v1/sessions/{id}/key` | set/replace the session's ephemeral BYO vendor key |
| GET | `/v1/sessions` | derived status: `sleeping / idle / running / unreachable`; `?agent=` filters |
| GET | `/v1/sessions/{id}` | session object; adds `busy/queued/last_event_at` + `usage` (harness-reported session totals) when awake |
| DELETE | `/v1/sessions/{id}` | cascade: escrow → delete actor → remove snapshots |
| POST | `/v1/sessions/{id}/suspend` | checkpoint in place; `409` mid-turn unless `?force=true` |
| POST | `/v1/sessions/{id}/message` | body `{"message":"…"}` (or an `events` array) → `202`; auto-wakes, retries 5xx wake races |
| GET | `/v1/sessions/{id}/message` | persisted log, `?since=evt_…` cursor |
| GET | `/v1/sessions/{id}/message/stream` | SSE; honors `Last-Event-ID` on reconnect; includes live `agent.message_delta` typing events |
| PUT | `/v1/credentials/{name}` | store a credential in the vault (Secret Manager), scoped to the calling user |
| GET | `/v1/credentials` | list the caller's credential names (values never returned) |
| DELETE | `/v1/credentials/{name}` | remove a credential |
| GET | `/v1/hand/credentials/{name}` | **internal** — hand pulls a granted credential with a short-lived HMAC grant, not a user token |

The `…/events` forms of the three message routes still work as **deprecated
aliases** for older clients.

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

Bodies are capped at 1 MiB. Agent and session ids are strictly validated
before use — a flag-shaped name like `--all` is rejected with
`400 invalid_request`, never passed to a shell.

## Authentication & self-service access

When token auth is enabled, every `/v1/*` route (except `/v1/access`) requires
`Authorization: Bearer <token>`, and each token maps to a **user identity** —
sessions and vault credentials are owned by that user.

**Ownership is enforced, not advisory.** Whoever creates a session owns it;
every session-scoped route checks that owner and answers `404 not_found` to
anyone else (never `403`, so session ids can't be enumerated by probing).
`GET /v1/sessions` lists only the caller's own. Ownership is recorded in the
`agentplane-session-owners` ConfigMap and released when the session is deleted.

`POST /v1/access` is rate-limited per client IP and per email (fixed one-minute
window); exceeding it returns `429`.

Tokens are self-service: `POST /v1/access {"email":"you@corp.com"}` returns
the caller's personal token **iff the email is on the operator's allowlist**
(a hot-reloaded file set via `AGENTPLANE_ALLOWED_EMAILS_FILE`; `#` comments
supported). The call is idempotent — the same email always gets the same
token. Non-allowlisted emails get `403`; if no allowlist is configured the
endpoint answers `503` and access is operator-managed.

## Credential vault → the hand

Users store third-party credentials (e.g. a GitHub PAT) with
`PUT /v1/credentials/gh-token {"value":"ghp_…"}`; values land in GCP Secret
Manager, named per user, and are never returned by the API. When a session's
agent declares `credentials: [gh-token]`, serve mints a short-lived HMAC
**grant** for the session's hand, and the hand redeems it against
`GET /v1/hand/credentials/{name}` — pulling the secret straight into actor
memory (git credentials / env / header form). User tokens cannot call the
hand-pull route, and grants cannot call anything else.

> The egress-gateway work (`egress/`) supersedes this hand-pull path: the goal
> state injects credentials at the egress proxy so the sandbox never holds
> them at all.

!!! note "Status probing never wakes a sleeping mind"
    List/get derive `sleeping` from the actor state alone — `/healthz` probes
    go only to awake minds, because probing a suspended actor would resume it.

    `usage`, `last_event_at` and `created_at` come from the **session store**,
    so they are returned for sleeping minds too, and `usage` survives the
    session's deletion. Live values from an awake harness supersede them.

!!! warning "Bodyless POSTs need an explicit `Content-Length: 0` under curl"
    `POST /v1/sessions/{id}/suspend` takes no body. `curl -X POST` sends
    neither `Content-Length` nor `Transfer-Encoding`, and the Google load
    balancer rejects that with **`411 Length Required`** before it reaches
    serve — an HTML error page, not a JSON one. Add `-H 'Content-Length: 0'`
    (or `-d ''`). Browsers and Node `fetch` set the header themselves, so
    andromeda and the SDKs are unaffected.

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
