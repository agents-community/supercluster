# Threat model

**Scope:** the whole agentplane platform as *deployed today* (2026-08-01),
not as designed. Every claim below cites the code or a live cluster check;
where the intended design differs from reality, both are shown.

**Method:** STRIDE per element over a data-flow diagram with explicit trust
boundaries, plus an asset inventory and abuse cases. Diagrams are PlantUML
committed beside this file so they diff in review:

| Diagram | What it shows |
|---|---|
| [`c4-components.puml`](c4-components.puml) | every component, protocol, and authn mechanism across 6 trust boundaries |
| [`seq-access-token.puml`](seq-access-token.puml) | email allowlist → personal bearer |
| [`seq-vault-grant.puml`](seq-vault-grant.puml) | vault → HMAC grant → hand pull |
| [`seq-byo-key.puml`](seq-byo-key.puml) | user's vendor key → actor memory → snapshot |
| [`seq-hand-admin.puml`](seq-hand-admin.puml) | serve → hand `/admin/*` control plane |
| [`seq-egress-injection.puml`](seq-egress-injection.puml) | egress credential injection — **designed vs deployed** |
| [`seq-tool-call.puml`](seq-tool-call.puml) | a message end-to-end: brain → atenet → hand → tool |

## 1. Assets

| Asset | Where it lives | Impact if disclosed |
|---|---|---|
| User access tokens (`apl_…`) | k8s Secret `agentplane-serve-tokens`, client `~/.andromeda/config.json` (0600) | full API access **as that user, and to every other user's sessions** (see F1) |
| Third-party credentials (GitHub PAT, …) | GCP Secret Manager `agentplane-cred-<user>-<name>`; **copied into hand actor memory + `~/.git-credentials`** on grant | attacker acts as the user on their external systems |
| Vendor API keys (BYO + shared) | actor memory; shared key in k8s Secret | billing theft; prompt/response access |
| Memory snapshots | GCS `gs://…/agentplane/` | **contain conversation + BYO key + pulled credentials** — the highest-value asset |
| Transcripts (escrow) | GCS `gs://…/transcripts/` | conversation disclosure |
| Execution journal | actor `/workspace` + Cloud Logging | reveals commands run (by design — it is the audit trail) |
| Grant-signing key = `HAND_ADMIN_TOKEN` | k8s Secret, env in serve **and every hand** | forge grants for any user/credential (see F3) |

## 2. Trust boundaries

1. **Internet ↔ LB** — `http://136.68.213.85.nip.io`, **no TLS** (verified: ingress `tls: NONE`)
2. **LB ↔ serve** — in-cluster HTTP
3. **serve ↔ Substrate control plane** — gRPC/TLS with `InsecureSkipVerify: true` (`session.go:58`)
4. **worker ↔ actor** — gVisor sandbox: *the* boundary against model-generated code
5. **actor ↔ actor / actor ↔ serve** — atenet (Envoy), Host-header routed; **no NetworkPolicies exist** (verified)
6. **cluster ↔ GCP + vendor APIs** — Workload Identity; actor egress NAT'd behind the worker IP

## 3. Findings

Ranked severity × likelihood. Filed: **F1 → #19**, **F2 → #20**, **F3/F4 → #21**, **F5 → #22**.

### F1 — CRITICAL — No per-session ownership: any token reads/writes any session
*Spoofing / Information disclosure / Tampering.* `pathSession` (`serve.go:374`)
validates the id's **shape** and nothing else; `handleSessionGet/Send/Delete/
Suspend` never compare the session to `userOf(r)`. `handleSessionList`
(`serve.go:511`) lists **all** sessions cluster-wide. The authenticated user
label is used only for vault namespacing and log attribution.

> Any allowlisted tester can enumerate every session, read other people's
> conversations, inject messages into them, and delete them. With ~10 testers
> this is a live multi-tenancy hole, not a theoretical one.

**Fix:** stamp an owner on session creation (actor label/annotation) and enforce
it in `pathSession`; filter `handleSessionList` by owner. Deny by default.

### F2 — CRITICAL — Cleartext HTTP on the public endpoint
*Information disclosure.* The ingress has no TLS, and `ONBOARDING.md` hands
testers an `http://` URL. Bearer tokens, prompts, model output, and **the
`PUT /v1/credentials` body (a raw GitHub PAT)** all cross the internet
unencrypted; `POST /v1/access` returns a token in cleartext.

**Fix:** managed cert + HTTPS redirect before any external tester uses it; treat
every token/credential issued over HTTP as compromised and rotate.

### F3 — HIGH — One shared secret is both the hand admin token and the grant-signing key
*Elevation of privilege.* `grantKey = HAND_ADMIN_TOKEN` (`serve.go:174`), and the
same value is mounted into **every** hand actor (`agentplane-hand-admin`). A
single compromised hand — i.e. any session where model-generated code reads its
own env — yields the key that **signs grants**. Since `verifyGrant` checks only
the HMAC and `exp`, the holder can mint a grant for *any* user and *any*
credential name and pull it from `/v1/hand/credentials/{name}` (that route is
deliberately not behind `s.auth` — the grant *is* the auth).

**Fix:** separate keys (grant-signing key never leaves serve); per-session hand
admin tokens; bind grants to the presenting actor's identity (Substrate
`ActorIdentity` mTLS) so a stolen grant is useless from elsewhere.

### F4 — HIGH — 30-day grant TTL
*Elevation of privilege.* `mintGrant(sid, user, names, 30*24*time.Hour)`
(`grant.go:109`). A grant leaked from actor memory, a checkpoint, or a log stays
redeemable for a month, and there is **no revocation** (stateless by design).

**Fix:** minutes-long TTL (the hand pulls once at session start), plus a
revocation list or key rotation; re-mint on resume instead of long life.

### F5 — HIGH — No NetworkPolicy: any actor can reach serve and every other actor
*Elevation of privilege / lateral movement.* Verified: zero NetworkPolicies in
the cluster. Model-generated code in a hand can reach `agentplane-serve:7433`,
the atenet router, other sessions' actors, and the k8s API network. Combined
with F3 (shared admin token) one session can drive another session's hand.

**Fix:** default-deny egress/ingress NetworkPolicies per pool; actors should
reach only atenet, and only for their pair.

### F6 — MEDIUM — Snapshots contain live secrets
*Information disclosure.* Documented already for BYO keys, but now also true of
**vault credentials pulled into the hand** — its memory image lands in GCS.
Anyone with bucket read gets user PATs.

**Fix:** CMEK + tight bucket IAM today; the real fix is F9 (egress injection, so
the sandbox never holds credentials).

### F7 — MEDIUM — `/v1/access` is unauthenticated and unthrottled
*Spoofing / DoS.* No rate limiting (`access.go`). An attacker who guesses or
learns an allowlisted email gets that user's token — and because the endpoint is
idempotent, the **same** token the legitimate user already holds. Email is
therefore a single-factor credential.

**Fix:** rate-limit per IP/email; prefer a one-time link or IdP (OIDC) over
"email in a JSON body"; log + alert on repeated denials (denials are logged).

### F8 — MEDIUM — Vault secret ids are ambiguously concatenated
*Tampering.* `secretID = "agentplane-cred-<user>-<name>"` (`vault.go:55`) with no
delimiter escaping: user `a` + name `b-c` collides with user `a-b` + name `c`.
Emails contain `.` and `@`, so the id is also not obviously Secret-Manager-safe
for all inputs.

**Fix:** hash or length-prefix the components; validate `name` against a strict
charset.

### F9 — MEDIUM — Egress credential injection is unbuilt and **untested on-cluster**
*Design gap.* [`egress/`](../../../egress/) passes as a **local Docker proof
only** — verified: zero egress pods in the cluster. Today's reality is the path
F3/F6 describe: credentials are copied *into* the sandbox. Every claim about
"secretless" applies to the target state, not the deployment.

**Fix:** the v1 deployment tracked in the egress module README; until then the
docs must not imply it is live (this file is the correction).

### F10 — LOW — `InsecureSkipVerify` to ateapi
*Spoofing.* `session.go:58` disables TLS verification to the Substrate control
plane. In-cluster and now behind Substrate's own mTLS, so exposure is limited to
an in-cluster MITM — but it defeats the mTLS that upstream just added.

**Fix:** verify against the ate CA bundle.

### Accepted risks (deliberate, documented)

| Risk | Why accepted |
|---|---|
| The journal records command summaries | Audit requires seeing actions; the trail is the control |
| Model-generated code runs arbitrary commands | gVisor is the boundary; tool policy is the user's (`allow`/`deny`) |
| Grants are stateless (no revocation) | Simplicity; mitigated once F4's TTL shrinks |

## 4. Abuse cases

1. **Tester reads another tester's session** — trivially possible today (F1).
2. **Model exfiltrates its own credentials** — it can `env`/`cat ~/.git-credentials` in its sandbox; contained only by the sandbox and by what the grant covers. Fixed by F9.
3. **Model pivots to another session** — reachable network (F5) + shared admin token (F3).
4. **Passive network capture** — tokens and PATs in cleartext (F2).
5. **Snapshot-bucket reader replays a mind** — reads conversations and keys (F6).
6. **Prompt injection from a fetched web page** steering tool calls — mitigated only by tool policy + sandbox; the egress allowlist (F9) is the real containment.

## 5. What to fix first

1. **F2 (TLS)** and **F1 (session ownership)** — before any external tester. Both are small.
2. **F5 (NetworkPolicy)** and **F4 (grant TTL)** — cheap, big blast-radius reduction.
3. **F3 (key separation)** then **F9 (egress injection)** — the structural fixes; F9 subsumes F6.
