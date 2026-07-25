# infra/tunnel — public HTTPS without a domain

serve authenticates every `/v1` route with a bearer token, but a public
endpoint must also be **HTTPS** — the BYO-key feature sends a user's vendor key
over the wire, so plaintext would leak it. These are the ways to get HTTPS in
front of serve **without buying a domain or a cloud load balancer**.

## Option A — Cloudflare quick tunnel (fastest; ephemeral URL)

Zero setup, no account, free. From any machine that can reach serve
(e.g. your laptop where `agentplane serve` or a port-forward runs):

```bash
cloudflared tunnel --url http://localhost:7433
# → https://<random>.trycloudflare.com   (new URL each run)
```

Point a client at it:
```bash
ANDROMEDA_URL=https://<random>.trycloudflare.com \
ANDROMEDA_TOKEN=<token> node andromeda/src/cli.mjs --agent brain
```

Great for "let someone try it this afternoon." The URL changes each run and the
tunnel lives with that process.

## Option B — Cloudflare named tunnel, in-cluster (persistent)

A stable URL that stays up independent of any laptop. Needs a free Cloudflare
account (for the tunnel token). Apply `cloudflared-deployment.yaml` — two
cloudflared pods connect out to Cloudflare and forward to the serve Service.
See that file's header for the token/hostname steps. Add **Cloudflare Access**
on the hostname for SSO in front of the bearer token.

## Option C — cluster-native GKE (no Cloudflare)

If you'd rather keep it all in GCP:
- **GKE Ingress + ManagedCertificate + a `nip.io` host** — HTTPS with no owned
  domain (`<static-ip>.nip.io` resolves to the IP). Needs a global static IP;
  cert provisions in ~15–60 min; ~$18/mo for the LB. (Ingress works today;
  the Gateway manifest in `../gateway` needs Gateway API enabled on the cluster,
  which it currently is not.)
- **Owned domain + `../gateway`** — the production path; best once you have a
  domain.

## Recommendation

- **Testing / demos now:** Option A (quick tunnel).
- **A stable share-able endpoint:** Option B (named tunnel) or Option C with a
  domain once you have one.

Whatever fronts it, serve's per-route bearer token and the ephemeral BYO-key
model are unchanged — the tunnel only adds reachability + TLS.
