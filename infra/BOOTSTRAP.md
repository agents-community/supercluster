# Deploying agentplane into a fresh GCP account

Everything account-specific lives in [`config.env`](config.env). Nothing else in
the repo should contain a project id, bucket name or IP — if you find one, that
is a bug.

Budget half a day. Most of it is waiting for a cluster and for golden snapshots
to bake.

---

## 1. Project and APIs

```bash
export PROJECT_ID=your-new-project
gcloud config set project "$PROJECT_ID"
gcloud services enable container.googleapis.com cloudbuild.googleapis.com \
  secretmanager.googleapis.com firestore.googleapis.com \
  containerregistry.googleapis.com
gcloud projects describe "$PROJECT_ID" --format='value(projectNumber)'   # → PROJECT_NUMBER
```

Put the id and number into `config.env`.

## 2. The cluster — three settings are not optional

```bash
gcloud container clusters create substrate-poc \
  --location=us-central1-c \
  --release-channel=stable \
  --enable-dataplane-v2 \
  --workload-pool="${PROJECT_ID}.svc.id.goog" \
  --num-nodes=6 --machine-type=e2-standard-4
```

| Setting | Why it matters |
|---|---|
| `--enable-dataplane-v2` | Substrate ships per-pool NetworkPolicies that close actor-to-actor ingress. Without Dataplane V2 you lose that. |
| `--workload-pool` | serve authenticates to Secret Manager, Firestore and GCS as a **Workload Identity principal**. Without it, nothing can reach any of them. |
| `--release-channel=stable` | auto-upgrade is mandatory on a channel; a PDB paces valkey so it survives. |

**Then immediately:**

```bash
gcloud container clusters update substrate-poc --location=us-central1-c \
  --update-addons=NodeLocalDNS=DISABLED
```

**Do not skip this, and never re-enable it.** NodeLocal DNSCache answers the
kube-dns *Service* IP on the **host**, and under Cilium neither a `podSelector`
nor an `ipBlock` matches host-destined traffic — so DNS is silently dropped by
any NetworkPolicy. The documented workaround (`CiliumNetworkPolicy` with
`toEntities: [host]`) does **not** exist on GKE Dataplane V2; those CRDs are not
exposed. This cost two production outages before it was found.

## 3. Substrate

Install from **our fork**, not upstream — we carry two patches:

- `cmd/atenet/internal/router/xds.go`: route timeout `0` + `IdleTimeout 300s`.
  Upstream's 10s cut every SSE stream mid-turn.
- `claude-code-multiplex.yaml.tmpl`

```bash
git clone -b agentplane-patches <your-substrate-fork> && cd substrate
export KO_DOCKER_REPO="gcr.io/${PROJECT_ID}/ate-images"
./hack/install-ate.sh --deploy-ate-system --create-valkey-ca-certs-secret
```

The valkey trust bundle needs **two** roots (servicedns + podidentity); one root
gives `CLUSTERDOWN` and a dead platform. `--create-valkey-ca-certs-secret`
handles it.

Verify before continuing:

```bash
kubectl -n ate-system exec valkey-cluster-0 -- \
  valkey-cli -a "$(kubectl -n ate-system get secret valkey-password -o jsonpath='{.data.password}' | base64 -d)" \
  --no-auth-warning cluster info | head -2      # want cluster_state:ok
```

## 4. Firestore

```bash
gcloud firestore databases create --location=us-central1 --type=firestore-native
```

## 5. IAM — bind to the **Workload Identity principal**, not a service account

This is the step most likely to be got wrong. serve does **not** run as the node
service account; with Workload Identity it presents:

```
principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/agentplane/sa/agentplane-serve
```

```bash
source infra/config.env
PR="principal://iam.googleapis.com/projects/${PROJECT_NUMBER}/locations/global/workloadIdentityPools/${PROJECT_ID}.svc.id.goog/subject/ns/agentplane/sa/agentplane-serve"

gcloud projects add-iam-policy-binding "$PROJECT_ID" --role=roles/datastore.user       --member="$PR" --condition=None
gcloud projects add-iam-policy-binding "$PROJECT_ID" --role=roles/secretmanager.admin   --member="$PR" --condition=None
gcloud storage buckets add-iam-policy-binding "gs://${SNAPSHOT_BUCKET}" --role=roles/storage.objectAdmin --member="$PR"
```

Granting these to the compute service account instead does nothing — the symptom
is a 502 on credential and escrow operations, with correct-looking IAM.

## 6. Networking

```bash
gcloud compute addresses create agentplane-ip --global   # → put the address in config.env
```

## 7. Build and deploy

```bash
source infra/config.env
./infra/build.sh all
./infra/render.sh | kubectl apply -f -
kubectl apply -f infra/serve/networkpolicy.yaml     # environment-independent
```

## 8. Agents

Agent specs pin an image **digest**, which changes per account. After
`build.sh`, update the digest in `agentplane/examples/*.yaml`:

```bash
gcloud container images describe "${IMAGE_REPO}/agentplane-brain-cc:${BRAIN_CC_TAG}" \
  --format='value(image_summary.digest)'
```

Then create them, and wait for `Ready` — each bakes a golden snapshot (~30s):

```bash
curl -X POST -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/yaml' \
  --data-binary @agentplane/examples/starter.yaml "$URL/v1/agents"
```

## 9. Verify

```bash
./test/smoke-live.sh          # asserts a hand tool actually ran, not just a reply
curl -sS "$URL/readyz"        # exercises serve → ateapi → valkey
```

---

## What does NOT come with you

- **Live and sleeping sessions.** A golden references GCS paths and the actor
  registry is cluster-local. Agents survive because their specs live in
  Firestore version history; the minds do not.
- **Secret Manager credentials.** No bulk move — users re-add them.
- **Firestore data.** Exportable (`gcloud firestore export`) if you want session
  ownership, agent history and usage rollups to carry over.
- **The static IP**, so DNS must be repointed.

## Order that matters

Cluster → **NodeLocalDNS off** → Substrate → Firestore → IAM → build → deploy.
IAM before deploy: serve logs a warning and disables the vault if it starts
without access, and the warning is easy to miss.
