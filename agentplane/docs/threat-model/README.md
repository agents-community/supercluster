# Threat model

**Scope:** the whole agentplane platform as *deployed today* (2026-08-03),
not as designed. Every claim below cites the code or a live cluster check;
where the intended design differs from reality, both are shown.

**Status at a glance.** Findings are marked FIXED / PARTIAL / OPEN against the
running cluster, not against merged code — the distinction matters, because F10
is fixed in code and still inactive in production for want of one env var.

| | |
|---|---|
| FIXED | F1, F3, F4, F7, F8 |
| PARTIAL | F5 (ingress only), F10 (code merged, not enabled) |
| OPEN | F2, F6, F9, F11, F12 |

Code is cited by *function*, not line number: every line-number citation in the
first revision of this document had drifted within two days and pointed at
unrelated code, which is worse than no citation at all.

**Method:** STRIDE per element over a data-flow diagram with explicit trust
boundaries, plus an asset inventory and abuse cases. Diagrams are PlantUML
committed beside this file so they diff in review:

Rendered SVGs are committed beside each source so the docs site and GitHub
show them without a PlantUML build; regenerate after editing a `.puml` with:

```bash
docker run --rm -v "$PWD":/data -w /data plantuml/plantuml -tsvg -o /data *.puml
```

### The system

![Components, protocols and trust boundaries](c4-components.svg)

Every arrow carries its protocol **and** its authentication mechanism; red
marks where there is none. Source: [`c4-components.puml`](c4-components.puml).

### Credential and token exchanges

Each of these is a place where a secret changes hands — the flows worth
attacking, drawn as deployed rather than as designed.

**Self-service access — email allowlist to bearer token** ([source](seq-access-token.puml))

![Access token issuance](seq-access-token.svg)

**Credential vault to hand, via HMAC grant** ([source](seq-vault-grant.puml))

![Vault grant flow](seq-vault-grant.svg)

**Egress credential injection — designed vs deployed** ([source](seq-egress-injection.puml))

![Egress injection](seq-egress-injection.svg)

**Hand admin plane — one shared token for every hand** ([source](seq-hand-admin.puml))

![Hand admin plane](seq-hand-admin.svg)

**Bring-your-own vendor key into actor memory and snapshots** ([source](seq-byo-key.puml))

![BYO key](seq-byo-key.svg)

**A message end-to-end, with the controls that exist today** ([source](seq-tool-call.puml))

![Tool call end to end](seq-tool-call.svg)

## 1. Assets

| Asset | Where it lives | Impact if disclosed |
|---|---|---|
| User access tokens (`apl_…`) | k8s Secret `agentplane-serve-tokens`, client `~/.andromeda/config.json` (0600) | full API access **as that user, and to every other user's sessions** (see F1) |
| Third-party credentials (GitHub PAT, …) | GCP Secret Manager `agentplane-cred-<user>-<name>`; **copied into hand actor memory + `~/.git-credentials`** on grant | attacker acts as the user on their external systems |
| Vendor API keys (BYO + shared) | actor memory; shared key in k8s Secret | billing theft; prompt/response access |
| Memory snapshots | GCS `gs://…/agentplane/` | **contain conversation + BYO key + pulled credentials** — the highest-value asset |
| Transcripts (escrow) | GCS `gs://…/transcripts/` | conversation disclosure |
| Execution journal | actor `/workspace` + Cloud Logging | reveals commands run (by design — it is the audit trail) |
| Grant-signing key | k8s Secret `agentplane-grant-key`, env in **serve only** | forge grants for any user/credential — no longer shared with hands (F3 fixed) |
| Hand admin token | k8s Secret `agentplane-hand-admin`, env in every hand | drive another session's hand admin plane |
| Session metadata | **Firestore** `sessions/{sid}` — owner, agent, version pin, activity, usage | maps sessions to the people who own them; reveals who ran what and what it cost |
| Agent definitions + version history | **Firestore** `agents/{name}/versions/{n}` — the submitted spec, verbatim | system prompts and tool policy; a writer could point new sessions at an attacker-authored agent |
| Lifetime spend per user | **Firestore** `usage/{email}` | billing/usage disclosure, keyed by email |

## 2. Trust boundaries

1. **Internet ↔ LB** — `http://136.68.213.85.nip.io`, **no TLS** (verified: ingress `tls: NONE`)
2. **LB ↔ serve** — in-cluster HTTP
3. **serve ↔ Substrate control plane** — gRPC/TLS. `ateapiTLS()` verifies against
   `AGENTPLANE_ATEAPI_CA` when set and warns loudly when not; **the deployment does
   not set it**, so verification is off in practice (F10)
4. **worker ↔ actor** — gVisor sandbox: *the* boundary against model-generated code
5. **actor ↔ actor / actor ↔ serve** — atenet (Envoy), Host-header routed.
   Substrate now ships per-pool NetworkPolicies (`substrate-brain-pool-*`,
   `substrate-hand-pool-*`) — **`policyTypes: [Ingress]` only**, admitting just
   `atenet-router`. Actor-to-actor *inbound* is therefore closed; **egress is
   entirely unrestricted** (F5)
6. **cluster ↔ GCP + vendor APIs** — Workload Identity; actor egress NAT'd behind the worker IP

## 3. Findings

Ranked severity × likelihood. Filed: **F1 → #19**, **F2 → #20**, **F3/F4 → #21**, **F5 → #22**.

### F1 — CRITICAL — **FIXED** — No per-session ownership: any token reads/writes any session
*Spoofing / Information disclosure / Tampering.* `pathSession()`
validates the id's **shape** and nothing else; `handleSessionGet/Send/Delete/
Suspend` never compare the session to `userOf(r)`. `handleSessionList`
(`handleSessionList()`) listed **all** sessions cluster-wide. The authenticated user
label is used only for vault namespacing and log attribution.

> Any allowlisted tester can enumerate every session, read other people's
> conversations, inject messages into them, and delete them. With ~10 testers
> this is a live multi-tenancy hole, not a theoretical one.

**Fixed.** `ownedSession()` gates every session-scoped route and answers **404,
not 403**, so ids cannot be enumerated by probing for the difference; the list is
filtered by owner. Ownership was first held in a ConfigMap and now lives in
Firestore (`sessions/{sid}.owner`), which removed that store's 1 MiB ceiling and
its lost-update race — a dropped owner locked the creator out of their own
session. Claims are create-only, so a claim can never reassign ownership.
Unowned sessions remain operator-only, so a store outage can only ever deny.

### F2 — CRITICAL — Cleartext HTTP on the public endpoint
*Information disclosure.* The ingress has no TLS, and `ONBOARDING.md` hands
testers an `http://` URL. Bearer tokens, prompts, model output, and **the
`PUT /v1/credentials` body (a raw GitHub PAT)** all cross the internet
unencrypted; `POST /v1/access` returns a token in cleartext.

**Fix:** managed cert + HTTPS redirect before any external tester uses it; treat
every token/credential issued over HTTP as compromised and rotate.

### F3 — HIGH — **FIXED** — One shared secret is both the hand admin token and the grant-signing key
*Elevation of privilege.* `grantKey = HAND_ADMIN_TOKEN` (as originally built), and the
same value is mounted into **every** hand actor (`agentplane-hand-admin`). A
single compromised hand — i.e. any session where model-generated code reads its
own env — yields the key that **signs grants**. Since `verifyGrant` checks only
the HMAC and `exp`, the holder can mint a grant for *any* user and *any*
credential name and pull it from `/v1/hand/credentials/{name}` (that route is
deliberately not behind `s.auth` — the grant *is* the auth).

**Fixed (partly).** The keys are separate secrets — `grantSigningKey()` reads
`AGENTPLANE_GRANT_KEY` (Secret `agentplane-grant-key`, mounted into serve only)
while hands get `agentplane-hand-admin`. Verified distinct on the deployment. A
compromised hand therefore no longer yields the grant-signing key.

**Still outstanding:** the hand admin token is one value shared by every hand, so
a compromised hand can still drive *another* session's hand admin plane, and
grants are not bound to the presenting actor. Binding them to Substrate
`ActorIdentity` mTLS remains the endgame — a stolen grant would then be useless
from anywhere else.

### F4 — HIGH — **FIXED** — 30-day grant TTL
*Elevation of privilege.* `mintGrant(sid, user, names, 30*24*time.Hour)`
(`mintGrant()`, as originally built). A grant leaked from actor memory, a checkpoint, or a log stays
redeemable for a month, and there is **no revocation** (stateless by design).

**Fixed.** `grantTTL = 10 * time.Minute`. The hand pulls once at session setup,
so a short life costs nothing; a grant leaked from actor memory or a checkpoint
is dead within ten minutes instead of a month. Revocation is still absent by
design (grants are stateless) — now an accepted risk rather than an open one,
because the TTL bounds it.

### F5 — HIGH → MEDIUM — **PARTIAL** — Actor egress is unrestricted
*Elevation of privilege / lateral movement.*

**This finding's original claim — "zero NetworkPolicies in the cluster" — is no
longer true.** The Substrate upgrade brought per-pool policies
(`substrate-brain-pool-*`, `substrate-hand-pool-*`), verified present and
selecting the pool pods by `ate.dev/worker-pool`.

What they cover: `policyTypes: [Ingress]`, admitting only `atenet-router` from
`ate-system`. So an actor can no longer be *reached* by another actor or by an
arbitrary pod — the lateral-movement half is closed, and closed below us, by the
platform rather than by our manifest.

What remains: those policies declare **no egress rules at all**, so
model-generated code in a hand can still open outbound connections to
`agentplane-serve:7433`, the atenet router, other sessions' actors, the k8s API
network, and the GCP metadata server. Combined with the still-shared hand admin
token (F3), one session can reach another session's hand admin plane — it just
has to initiate the connection itself.

Our own `infra/serve/networkpolicy.yaml` covers egress and is deliberately
**unapplied**: a first attempt broke DNS for every actor (wrong pod label, plus
Dataplane V2 + NodeLocal DNSCache resolving via a link-local address that
`ipBlock: 0.0.0.0/0` does not match). Both causes are fixed in the file and
neither is re-validated. See #22 / #25.

**Fix:** apply the egress half behind a live canary — mint a throwaway session,
send one turn, confirm the reply lands *before* walking away. Deny the metadata
server explicitly; it is the one destination that turns egress into credential
theft.

### F6 — MEDIUM — Snapshots contain live secrets
*Information disclosure.* Documented already for BYO keys, but now also true of
**vault credentials pulled into the hand** — its memory image lands in GCS.
Anyone with bucket read gets user PATs.

**Fix:** CMEK + tight bucket IAM today; the real fix is F9 (egress injection, so
the sandbox never holds credentials).

### F7 — MEDIUM — **FIXED** — `/v1/access` is unauthenticated and unthrottled
*Spoofing / DoS.* No rate limiting (`access.go`). An attacker who guesses or
learns an allowlisted email gets that user's token — and because the endpoint is
idempotent, the **same** token the legitimate user already holds. Email is
therefore a single-factor credential.

**Fixed (the throttle).** `accessLimiter` caps issuance at 10/min per IP and
5/min per email, so the endpoint can no longer be ground through at speed.

**Unchanged by design:** email remains a single factor, and the endpoint is still
idempotent, so anyone who learns an allowlisted address gets that user's token.
An IdP (OIDC) or a one-time link is the real fix; the rate limit buys time, it
does not change the trust model. Treat the allowlist as a *convenience for a
closed tester group*, never as authentication.

### F8 — MEDIUM — **FIXED** — Vault secret ids are ambiguously concatenated
*Tampering.* `secretID = "agentplane-cred-<user>-<name>"` (`vault.secretID()`, as originally built) with no
delimiter escaping: user `a` + name `b-c` collides with user `a-b` + name `c`.
Emails contain `.` and `@`, so the id is also not obviously Secret-Manager-safe
for all inputs.

**Fixed.** `vault.secretID()` hashes the user into a fixed-width prefix —
`agentplane-cred-<sha256(user)[:8] hex>-<name>` — so the user segment can no
longer run into the name segment, and an email's `.`/`@` never reach the secret
id. Collisions between `(a, b-c)` and `(a-b, c)` are structurally impossible.

### F9 — MEDIUM — Egress credential injection: API declared, gateway unbuilt
*Design gap.* [`egress/`](../../../egress/) passes as a **local Docker proof
only** — verified: zero egress pods in the cluster. Today's reality is the path
F3/F6 describe: credentials are copied *into* the sandbox.

**Update (#29 / #30):** the AgentSpec now carries an egress policy — `egress.mode`
+ `allowedHosts` for reachability, and `credentials[].inject` binding a
credential to the destinations that receive it. It validates, compiles onto the
ActorTemplate, and is **inert**: no gateway consumes it, so declaring a policy
constrains nothing. Two layers are enforced at *create* time (a host must be
both reachable and trusted with the secret) so a policy can't silently match
nothing — but that is input validation, not runtime containment.

Remaining chain: serve renders the policy per session → gateway actor
(`e-<sid>`) → traffic actually forced through it (F11) → `egress_gateway_address`
wired, which Substrate leaves unproduced.

**Fix:** deploy the gateway. Until then the docs must not imply the sandbox is
secretless — this file and `docs/agents.md` both carry that correction.

### F11 — HIGH — Proxy enforcement via `HTTPS_PROXY` is bypassable by the code it contains
*Elevation of privilege / design gap.* The v1 proof routes egress by setting
`HTTPS_PROXY` in the hand and trusting the proxy's CA. That is a **cooperative**
control: model-generated code running in that same sandbox can `unset
HTTPS_PROXY`, pass `--noproxy`, or open a raw socket, and its traffic leaves via
the worker's normal NAT path — unfiltered, unlogged, and unaffected by any
allowlist. The proof demonstrates that injection *works*; it does not
demonstrate that egress is *contained*.

This matters because the threat being mitigated is precisely "the agent does
something we didn't intend" — an enforcement mechanism the agent can switch off
does not mitigate it.

**Fix:** enforce below the sandbox, where the actor cannot reach the control.
Substrate's `atunnel` does exactly this — nftables redirect on the *host*, mTLS
to a remote gateway, authenticated `X-Ate-Atespace` / `X-Ate-Actor-Name` headers
— and it is **no longer hypothetical**: it shipped in the Substrate we now run,
and every worker logs `atunnel serving` at boot.

What it does not do yet is carry our traffic. atunnel is L4 CONNECT only (no TLS
termination, so no injection), and its `egress_gateway_address` has **no
producer** in Substrate — nothing sets it, so no traffic is redirected today.
The remaining work is ours: build the gateway (F9) and supply that address.

Treat `HTTPS_PROXY` as a development convenience only, never as the production
containment boundary.

### F12 — LOW — Placeholder credentials are a deliberate, smaller exposure
*Information disclosure (accepted trade-off).* Pure injection assumes the
sandbox sends an *uncredentialed* request the gateway then authenticates. Many
real clients won't: `git` will not attempt Basic auth with no credential
configured, and most CLIs read a key from the environment before making any
call. Supporting them requires an **opaque placeholder** inside the sandbox
which the gateway swaps for the real secret at egress (the approach Anthropic's
Managed Agents uses for `environment_variable` credentials).

The placeholder is a real string the agent can read and exfiltrate — so this is
weaker than holding nothing, but far stronger than F6 (the placeholder is
useless anywhere except through the gateway, which decides whether the caller
and destination are entitled to the real value). Known side effect: clients
that validate key *format* locally fail before any network call.

**Fix:** support both modes — placeholder where the client demands one, pure
injection everywhere else — and prefer the latter. Never inject into the URL
path (Slack-style path-secret webhooks are out of scope by design).

### F10 — LOW — **PARTIAL (merged, not enabled)** — `InsecureSkipVerify` to ateapi
*Spoofing.* `ateapiTLS()` now verifies the control plane against a PEM bundle at
`AGENTPLANE_ATEAPI_CA`, falling back to unverified TLS with a loud one-shot
warning when it is absent or unparseable.

**The deployment does not set `AGENTPLANE_ATEAPI_CA`**, so the running cluster is
still on the unverified path. This is the one finding where "merged" and "fixed"
diverge, which is why the status table above is written against the cluster
rather than against `main`.

Exposure stays limited to an in-cluster MITM, and Substrate's own mTLS sits
underneath — but leaving it unset defeats the verification upstream added.

**Fix:** mount the ate CA bundle and set `AGENTPLANE_ATEAPI_CA`; the code path
already exists and the warning in the logs is the reminder.

### Accepted risks (deliberate, documented)

| Risk | Why accepted |
|---|---|
| The journal records command summaries | Audit requires seeing actions; the trail is the control |
| Model-generated code runs arbitrary commands | gVisor is the boundary; tool policy is the user's (`allow`/`deny`) |
| Grants are stateless (no revocation) | Simplicity; bounded by F4's 10-minute TTL, which is now in place |

## 4. Abuse cases

1. **Tester reads another tester's session** — trivially possible today (F1).
2. **Model exfiltrates its own credentials** — it can `env`/`cat ~/.git-credentials` in its sandbox; contained only by the sandbox and by what the grant covers. Fixed by F9.
3. **Model pivots to another session** — reachable network (F5) + shared admin token (F3).
4. **Passive network capture** — tokens and PATs in cleartext (F2).
5. **Snapshot-bucket reader replays a mind** — reads conversations and keys (F6).
6. **Prompt injection from a fetched web page** steering tool calls — mitigated only by tool policy + sandbox; the egress allowlist (F9) is the real containment.

## 5. Alignment with Substrate's own threat model

Substrate published [`docs/threat-model.md`](https://github.com/agent-substrate/substrate/blob/main/docs/threat-model.md)
in [PR #559](https://github.com/agent-substrate/substrate/pull/559) (the atunnel
work, already in our pinned base). It was written independently of this
document, which makes the overlap evidence rather than coincidence.

**It validates our findings.** Their Critical-rated threats map onto ours:

| Their threat (priority) | Ours |
|---|---|
| "Malicious actor gains access to other actors via network" — *policies must deny ingress and egress by default* (Critical) | **F5** |
| "…via node-local endpoints exposed on the network (e.g. instance metadata)" (Critical) | **F5** — our policy blocks `169.254.169.254` |
| "…via Kubernetes APIs — **strong preference on blocking actor access**" (Critical) | **F5** |
| "Malicious actor gains access to snapshots of other actors and steals data" + *avoid snapshotting sensitive credentials* (Critical) | **F6** |
| "Tricks Substrate identity broker into returning credentials for a different actor" — *tie claims to actor/worker, validate on use* (Critical) | **F3** — our grants are HMAC+exp only, unbound to the caller |
| "Improper handling of Secrets — ensure an official, secure way to pass secret data to actors" (High) | **F3 / F6** |

**It prescribes our egress module by name.** Their highest agent-specific
threat is one only an agent platform has:

> *"Agent leaks credentials exposed in sandbox, because LLMs are unreliable.
> Due to prompt injection or just agent silliness."* (High)
>
> **Mitigating invariant:** *"Credentials are not exposed in sandboxes by default."*
>
> **Suggested mitigation:** *"Opt-in to credentials, none by default.
> **Credential injecting proxy (injects tokens or terminates TLS and holds
> x509 private key on behalf of sandbox).**"*

That is precisely [`egress/`](../../../egress/), arrived at independently. It
promotes **F9** from "our differentiator" to "the mitigation the platform's own
security analysis calls for" — and the *GitHub Issue* column is empty
throughout their table, so none of it appears claimed yet.

### What they cover that we do not

Substrate-layer concerns we inherit rather than own, tracked here because a
failure there defeats our controls:

- **Worker reuse** — all actor state (process, filesystem, env, network policy)
  must be reset between actors sharing a worker; they call out stale-policy
  races explicitly.
- **Snapshot integrity** — corrupt or attacker-written snapshots must be
  verified before restore; we treat snapshots as trusted today.
- **Actor self-modification** — an actor reading or writing its own snapshot;
  their fix is separate credentials for snapshot access.
- **Cluster DNS exposure** — actors can enumerate internal topology; they
  recommend not exposing Substrate-internal DNS to actors at all.
- **Actor-creation quotas** — fork-bomb style resource exhaustion. Relevant to
  us as soon as testers can create sessions freely.

## 5. What to fix first

1. **F2 (TLS)** and **F1 (session ownership)** — before any external tester. Both are small.
2. **F5 (NetworkPolicy)** and **F4 (grant TTL)** — cheap, big blast-radius reduction.
3. **F3 (key separation)** then **F9 (egress injection)** — the structural fixes; F9 subsumes F6.
4. When building F9, enforce via **atunnel, not `HTTPS_PROXY`** (F11) — otherwise the
   containment is one `unset` away from being nothing.
