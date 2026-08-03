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

**Open findings first, then the fixed ones as a table.** Ids are stable — F5 keeps
its number even though it is now a different (narrower) finding, because issues
and commits reference them. F9/F11/F12 are presented together: they were three
entries for one absent component and read as three separate problems.

Filed: **F1 → #19**, **F2 → #20**, **F3/F4 → #21**, **F5 → #22/#25**.

### Fixed (kept for the audit trail)

These were real and are closed. Detail lives in the PRs; what matters here is
that the control exists and where it stops.

| | Was | Now | Still not covered |
|---|---|---|---|
| **F1** CRITICAL | any token read, wrote and deleted any session; list showed all sessions cluster-wide | `ownedSession()` gates every session route and answers **404, not 403** so ids can't be enumerated; list filtered by owner; ownership in Firestore, claims create-only | — |
| **F3** HIGH | the grant-signing key *was* `HAND_ADMIN_TOKEN`, mounted in every hand, so one compromised hand could forge grants for any user | separate secrets: `AGENTPLANE_GRANT_KEY` in serve only, `agentplane-hand-admin` in hands (verified distinct on the deployment) | the hand admin token is still **one value shared by every hand**, and grants aren't bound to the presenting actor — `ActorIdentity` mTLS remains the endgame |
| **F4** HIGH | 30-day grant TTL, no revocation | `grantTTL = 10 * time.Minute`; the hand pulls once at setup, so a leaked grant dies in minutes | revocation still absent by design — bounded by the TTL, now an accepted risk |
| **F7** MEDIUM | `/v1/access` unthrottled — an allowlisted address could be ground through at speed | `accessLimiter`: 10/min per IP, 5/min per email | **email is still a single factor** and the endpoint is idempotent, so anyone who learns an address gets that user's token. The allowlist is a convenience for a closed tester group, not authentication |
| **F8** MEDIUM | `agentplane-cred-<user>-<name>` — `(a, b-c)` collided with `(a-b, c)`; emails put `.`/`@` in the id | `vault.secretID()` hashes the user to a fixed-width prefix | — |

### F2 — CRITICAL — Cleartext HTTP on the public endpoint
*Information disclosure.* The ingress has no TLS, and `ONBOARDING.md` hands
testers an `http://` URL. Bearer tokens, prompts, model output, and **the
`PUT /v1/credentials` body (a raw GitHub PAT)** all cross the internet
unencrypted; `POST /v1/access` returns a token in cleartext.

**Fix:** managed cert + HTTPS redirect before any external tester uses it; treat
every token/credential issued over HTTP as compromised and rotate.

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

**Fix:** CMEK + tight bucket IAM today; the real fix is the egress section below
(F9/F11/F12), so the sandbox never holds credentials.

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

### F9 / F11 / F12 — HIGH — Egress is neither contained nor credential-free

*Design gap.* These were three findings for one absent component; they are one
story and read better as one.

**Today:** the hand holds the credential. Serve grants it, the hand writes it to
actor memory and `~/.git-credentials`, and it calls GitHub directly through the
worker's NAT — no allowlist, no per-call audit, no central rotation, any
destination reachable. It is in every checkpoint of that actor (F6). Verified:
zero egress pods in the cluster.

**Declared but inert (F9).** The AgentSpec carries `egress.mode` + `allowedHosts`
for reachability and `credentials[].inject` binding a credential to the
destinations that may receive it. It validates, compiles onto the ActorTemplate,
and **nothing consumes it** — declaring a policy constrains nothing today. The
two-layer rule (a host must be both reachable *and* trusted with the secret) is
enforced at create time so a policy can't silently match nothing, but that is
input validation, not runtime containment.

**Why a proxy env var is not the answer (F11).** The local proof routed egress by
setting `HTTPS_PROXY` in the hand. That is a **cooperative** control: code in
that sandbox can `unset HTTPS_PROXY`, pass `--noproxy`, or open a raw socket and
leave via the normal NAT path — unfiltered and unlogged. The threat being
mitigated is precisely "the agent does something we didn't intend", and a
mechanism the agent can switch off does not mitigate it.

Substrate's **atunnel** is the right shape — nftables redirect on the *host*,
below the sandbox, mTLS to a remote gateway, authenticated
`X-Ate-Atespace` / `X-Ate-Actor-Name` headers — and it now ships in the Substrate
we run (every worker logs `atunnel serving`). It carries none of our traffic yet:
it is L4 CONNECT only, and `egress_gateway_address` has **no producer** upstream.

**The placeholder trade-off (F12).** Pure injection assumes the sandbox sends an
uncredentialed request the gateway then authenticates. Many clients won't — `git`
will not attempt Basic auth with nothing configured. Supporting them needs an
opaque placeholder inside the sandbox, swapped at egress. That string is readable
and exfiltratable, so it is weaker than holding nothing but far stronger than
today: it is useless anywhere except through the gateway. Known side effect:
clients that validate key *format* locally fail before any network call.

**Remaining chain:** build the gateway → serve renders the policy per session →
supply `egress_gateway_address` so atunnel actually redirects → prefer pure
injection, placeholder only where a client demands one. Never inject into the URL
path; path-secret webhooks (Slack) are out of scope by design.

**Until then, no document may imply the sandbox is secretless.**

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
3. **F9/F11/F12 (egress)** — the structural fix; it subsumes F6. F3's key
   separation is done; per-hand admin tokens are what remain of it.
4. When building F9, enforce via **atunnel, not `HTTPS_PROXY`** (F11) — otherwise the
   containment is one `unset` away from being nothing.
