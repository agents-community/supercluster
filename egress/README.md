# egress — the AgentGateway

Enterprise connectivity for the hand with **credential injection at the egress
boundary** (the "secretless" pattern).

The hand runs model-generated code in a gVisor sandbox. We do **not** want
long-lived credentials inside that sandbox. Instead the hand makes
*uncredentialed* outbound calls, and a proxy in the egress path:

1. terminates TLS with an internal CA the hand trusts,
2. matches the destination host against the session's policy,
3. pulls the matching secret from the vault and injects it
   (`Authorization` header / URL rewrite),
4. re-originates the request to the real upstream.

The secret exists only in the gateway — **never** in the sandbox that ran the
request. One controlled egress point gives you allowlisting, DLP, per-call
audit, central rotation, and per-user attribution.

## Why this shape

Substrate's `atenet` is Envoy (xDS + `ext_proc`, the request-mutation hook).
Actor egress today is just NAT'd behind the worker IP
(`internal/ateomnet/net.go:253` calls out a later **AgentGateway** phase that is
not yet built). This module is that gateway, built as agentplane's
differentiator — and a candidate upstream contribution. See the
`identity-strategy` memory for the full plan.

## Status

- **v1 (proven, local):** per-session proxy paired to the hand. Static policy
  file. See `proof/` — a Docker harness that demonstrates the whole claim.
- **v1 (deploy, next):** proxy as a Substrate actor paired to the hand
  (`e-<sid>` ↔ `h-<sid>`); serve renders the policy per session from the
  vault; internal CA baked into the hand image; `HTTPS_PROXY` in the hand
  template.
- **v2:** shared proxy tier + `ateapi.ActorIdentity` mTLS so the gateway maps
  the calling actor → that user's credentials.

## `proof/` — the local v1 proof

```
cd egress/proof
./run.sh          # needs Docker
```

It starts the proxy (mitmproxy + `inject.py`) with `policy.json` mounted into
the **proxy only**, then runs a hand-like container that holds no secret and
routes egress through the proxy. Verified results:

| Check | Result |
|---|---|
| hand holds no secret | env + cred files empty |
| injection | uncredentialed call to a basic-auth endpoint → HTTP 200 `authenticated: true` |
| what upstream received | `Authorization: Basic …` the hand never sent |
| deny-by-default | non-allowlisted host → HTTP 403 |
| git through proxy | `git clone` succeeds over the TLS-terminated path |

`inject.py` is the injection addon, `policy.json` the per-session policy
(placeholder demo tokens — production renders this from Secret Manager),
`hand-test.sh` runs inside the hand, `run.sh` orchestrates it all.
