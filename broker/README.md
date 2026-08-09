# broker — the mediator between brain and hand

```
[ brain actor ]  ──►  [ broker pod ]  ──►  [ hand actor ]
  Substrate           NOT Substrate         Substrate
  harness / LLM       MCP + credentials     execution zone
  (unchanged)         + approval policy     (unchanged)
```

The broker is a plain Kubernetes pod — **not** a Substrate actor. It sits between
the brain and the hand and does the trusted mediation: it receives the brain's
tool calls, applies credentials and approval policy, and forwards the call to the
hand. Both actor templates stay byte-for-byte unchanged; the broker is inserted
in the path, not carved out of either actor.

## Why a separate pod

Everything the broker does is *our* trusted code — MCP mediation, credential
injection, `ask:` approval gating. None of it runs untrusted model output. So it
does not need the gVisor sandbox, and putting it in a plain pod means:

- credentials and policy live in a component we control and can update/scale
  freely, **outside** the sandbox that runs untrusted commands;
- the brain and hand actors do not change — no template edits, no image rebuilds
  on either side to insert the broker.

## The front door: two phases, one core

The broker's job is identical no matter how the brain's request reaches it:
**receive a request + its destination → apply policy → forward the MCP call to
the hand.** Only the *front door* changes. So the core is built once and the door
is swapped underneath it.

### Phase 1 — HTTP front door (works today, no Substrate change)

The brain is pointed at the broker via its harness MCP endpoint (`handURL()` in
`brain/identity.mjs`, overridable by `HAND_MCP_URL`, or pushed by serve at
session start over the existing `/options` channel). The broker exposes an MCP
StreamableHTTP server on `/mcp`, resolves the target hand from the request, and
proxies to the hand's real `/mcp`.

This validates the whole path end-to-end while touching nothing risky.

### Phase 2 — egress-capture front door (the Substrate-native path)

Substrate PR #559 (merged, and in our local build) ships transparent actor
egress capture in `ateom-gvisor`:

```
actor outbound TCP
  → nftables REDIRECT              (installed only when egress_gateway_address != "")
  → atunnel egress listener        (internal/atunnel/egress.go)
  → CONNECT over mTLS              (internal/atunnel/client.go, carries EgressMetadata
  → egress gateway                  { atespace, actorName, actorVersion } + original dst)
```

In this phase the broker becomes that **egress gateway**: an mTLS server that
speaks atunnel's CONNECT protocol, reads the source actor identity and the
original destination for free, then does the same core mediation. The brain is
never told anything — its egress is transparently redirected. Nothing in the
brain changes at all.

The machinery is live but **dormant**: nothing populates `egress_gateway_address`
today (the PR calls the producer side a "fast follow" that has not landed). So
Phase 2 also needs a **producer**: a Substrate delta that sets
`egress_gateway_address` → this broker for brain actors, carried in
`infra/substrate-patches/*.patch` the same way the atenet stream-timeout delta
already is (`substrate@a73a14d`).

## Reaching the hand

The broker is a plain pod, so it cannot dial a hand's atunnel ingress directly —
atunnel only accepts the `atenet-router` SPIFFE identity. The broker reaches the
hand the same way `serve` does: through atenet's ingress address, routed by the
actor Host (`h-<id>.<atespace>.actors.resources.substrate.ate.dev`). See
`internal/backend/substrate/substrate.go` for the existing pattern.

Because we do **not** change atenet ingress routing, there is no loop: the
broker → atenet → hand hop is a normal actor request.

## What the broker does NOT do

- It does not replace the hand's gateway. To honor "don't touch the hand
  template," the hand keeps its `hand` + `exec` containers; the broker fronts the
  hand's existing `/mcp`, adding the mediation layer in front of it.
- It does not hold the model key — that stays in the brain, which is what calls
  the LLM provider.
