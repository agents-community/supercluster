# infra/lb — Google Cloud load balancer for agentplane-serve

A GKE **Ingress** provisions a Google Cloud **L7 HTTP(S) load balancer** in
front of the in-cluster `agentplane-serve` Service. HTTPS is mandatory (BYO-keys
travel over the wire), and a **Google-managed certificate** covers a `nip.io`
host so **no domain purchase is needed**.

## What gets created

- a global **static IP** (`agentplane-ip`) — the stable public address,
- a **ManagedCertificate** for `<ip>.nip.io` (auto-renewing TLS),
- a **BackendConfig** (health-checks `/healthz`, 1h timeout for SSE),
- the **Ingress** (the LB itself) routing `/` → serve on 7433.

## Auth — two layers

**1. Application auth (already live, on every request).** serve authenticates
every `/v1` route with a bearer token (constant-time compare); `/healthz` is the
only open route. This is the auth for "hand someone a token":

```bash
curl -H "Authorization: Bearer <token>" https://<ip>.nip.io/v1/agents
```

The token lives in the `agentplane-serve-token` Secret (what the serve
Deployment reads as `AGENTPLANE_TOKEN`). Rotate by updating that Secret and
restarting the Deployment.

**2. LB-layer auth — Identity-Aware Proxy (optional).** To require **Google SSO**
before a request even reaches serve, enable IAP in `ingress.yaml`'s
BackendConfig:

```bash
# create an OAuth client (console: APIs & Services → Credentials), then:
kubectl -n agentplane create secret generic agentplane-iap-oauth \
  --from-literal=client_id=<id> --from-literal=client_secret=<secret>
# uncomment the iap: block in ingress.yaml and re-apply
```

Use IAP for a **team-internal** endpoint. For **external users bringing their
own key**, keep the bearer token (IAP would force them all into your Google
org). The two are composable — IAP gates who reaches serve, the bearer token
gates what they can call.

## Bring it up

```bash
gcloud compute addresses create agentplane-ip --global          # once
HOST=$(gcloud compute addresses describe agentplane-ip --global --format='value(address)').nip.io
sed "s/\"HOST\"/\"$HOST\"/g" ingress.yaml | kubectl apply -f -
```

## Watch it provision

```bash
kubectl -n agentplane get ingress agentplane                    # ADDRESS appears in ~5–10 min
kubectl -n agentplane get managedcertificate agentplane-cert    # Provisioning → Active (~15–60 min)
curl https://<ip>.nip.io/healthz                                # {"ok":true} once Active
```

Point a client at it:
```bash
ANDROMEDA_URL=https://<ip>.nip.io ANDROMEDA_TOKEN=<token> \
  node andromeda/src/cli.mjs --agent brain
```

## Cost & teardown

A global L7 LB is ~$18–25/mo. To tear down:
```bash
kubectl -n agentplane delete ingress agentplane managedcertificate agentplane-cert
gcloud compute addresses delete agentplane-ip --global
```

> The Cloudflare-tunnel path (`../tunnel`) is the zero-cost, instant-TLS
> alternative when you don't want a standing cloud LB.
