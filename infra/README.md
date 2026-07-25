# infra — deploying supercluster

Cross-galaxy infrastructure: how the control plane runs as a **public,
authenticated access point** instead of a laptop behind port-forwards.

```
infra/
  serve/       run `agentplane serve` IN-cluster (Deployment, Service, RBAC, image)
  gateway/     expose it over public HTTPS (GKE Gateway + managed TLS cert)
  terraform/   the cloud resources underneath (planned; per provider)
```

## Why in-cluster serve

Today serve runs locally and reaches Substrate via `kubectl port-forward`. That
can't be a public endpoint. Running serve **in the cluster** gives it direct
service DNS to ateapi/atenet and a stable Service to put a load balancer in
front of. serve is stateless (sessions/agents derive from Substrate), so it
scales freely.

## Deploy the API (in-cluster)

```bash
# 1. build the serve image (binary is built locally — see the Dockerfile note)
cd agentplane && GOOS=linux GOARCH=amd64 go build -o ../infra/serve/agentplane ./cmd/agentplane
cd ../infra/serve
gcloud builds submit --tag gcr.io/<project>/ate-images/agentplane-serve:v1 .

# 2. identity + RBAC (least privilege: ActorTemplates only)
kubectl apply -f rbac.yaml

# 3. the access token clients will present (every /v1 route requires it)
kubectl -n agentplane create secret generic agentplane-serve-token \
  --from-literal=token="$(openssl rand -hex 24)"

# 4. deploy (set the image; optionally set AGENTPLANE_BUCKET + Workload Identity for escrow)
sed 's#gcr.io/PROJECT_ID/ate-images/agentplane-serve:latest#gcr.io/<project>/ate-images/agentplane-serve:v1#' \
  deployment.yaml | kubectl apply -f -

kubectl -n agentplane rollout status deploy/agentplane-serve
```

Verify internally (no public endpoint yet):
```bash
kubectl -n agentplane port-forward svc/agentplane-serve 7433:7433 &
curl -s localhost:7433/healthz
curl -s -H "Authorization: Bearer <token>" localhost:7433/v1/agents
```

## Go public (HTTPS + auth)

serve already authenticates **every** `/v1` route with the bearer token (only
`/healthz` is open). To expose it on a domain:

```bash
gcloud compute addresses create agentplane-ip --global
# DNS: api.<domain> → that IP
# edit infra/gateway/gateway.yaml: replace api.YOUR_DOMAIN
kubectl apply -f ../gateway/gateway.yaml    # provisions a Google-managed cert (~15m)
```

**Auth layers** (choose per audience):
- **Bearer token** (built in) — best for "hand someone a token to try."
- **Cloud IAP** — Google-managed login in front of the LB; best for your team.
- **Cloudflare Tunnel + Access** — fastest public HTTPS without a cloud LB; good for demos.

The token gates *your control plane*; the user's **BYO vendor key** (ephemeral,
per session) gates *their* inference. Two independent secrets — that separation
is what makes "plug your key, use our cluster" safe.

## Status

The in-cluster serve is **deployed and verified** (2026-07-23): auth enforced
on every `/v1` route, agent ops via RBAC-scoped kubectl, and full session
turns via ateapi/atenet — all with **no port-forward to Substrate**. The
Gateway/public-HTTPS step is written and ready to apply once a domain + static
IP exist.

## Still to provision (by hand until `terraform/` lands)

- **CMEK on the snapshot bucket** — closes the BYO-key at-rest gap (an actor's
  checkpoint holds the session's key in memory).
- **Workload Identity** for the serve ServiceAccount — enables transcript escrow.
