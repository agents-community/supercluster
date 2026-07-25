# infra/substrate-overlays — production patches on top of a Substrate install

## What is an "overlay"?

Changes we apply **on top of** a vanilla Agent Substrate install **without
editing Substrate's own manifests**. Substrate is an external dependency
(agentplane imports only its proto); its manifests are the *base*. These files
are the *overlay* — version-controlled, documented, and re-appliable after any
Substrate re-install (which reverts in-place tweaks). This is the clean way to
customize a dependency you don't own, and where our GKE reliability fixes live.

## Contents

| Overlay | What / why |
|---|---|
| `valkey-pdb.yaml` | PodDisruptionBudget for the `valkey-cluster` StatefulSet — `maxUnavailable:1` keeps quorum during node drains (GKE recommendation "Set PDB for StatefulSet"). |
| `resources.sh` | Production CPU/memory **requests + limits** for the `ate-system` components (GKE recommendation "Low resource requests"). Idempotent; re-run after re-install. |

Also tracked (source patch lives in the Substrate checkout, not this repo):
- **atenet `xds.go` route timeout** — `Timeout:0` + `IdleTimeout:300s` so SSE
  event streams aren't cut at ~10s. Re-apply when rebuilding the atenet image.

## Applying

```bash
kubectl apply -f valkey-pdb.yaml        # PDB (no disruption)
./resources.sh                          # requests/limits — ROLLING RESTART; use a maintenance window
```

`resources.sh` sets these starting values (tune from `kubectl top pods -n ate-system`):

| Component | requests | limits |
|---|---|---|
| ate-api-server | 250m / 256Mi | 1 / 512Mi |
| ate-controller | 100m / 128Mi | 500m / 256Mi |
| atenet-router (router) | 100m / 128Mi | 500m / 256Mi |
| atenet-router (envoy) | 200m / 256Mi | 1 / 512Mi |
| dns (coredns) | 100m / 128Mi | 500m / 256Mi |
| dns (dns-controller) | 50m / 64Mi | 200m / 128Mi |
| atelet (DaemonSet) | 250m / 512Mi | cpu 2, **no mem limit** |
| valkey | 200m / 512Mi | cpu 1, **no mem limit** |

> ⚠️ **No memory limits on atelet or valkey — deliberate.** atelet holds gVisor
> sandbox memory (checkpoint/restore) and valkey is an in-memory datastore; a
> memory *limit* OOMKills them under load. A 1Gi atelet limit once took the
> whole cluster down ("no free workers available" — atelet crash-looped and the
> worker pods lost their connection; recovery was: raise/remove the limit, then
> `kubectl -n agentplane rollout restart deploy/brain-pool-deployment` so the
> workers reconnect). They get memory *requests* (scheduling) but no cap.
> If a live valkey still carries a 1Gi limit from an earlier run, remove it:
> `kubectl -n ate-system patch statefulset valkey-cluster --type=json -p '[{"op":"remove","path":"/spec/template/spec/containers/0/resources/limits/memory"}]'`

## brain-pool workers (WorkerPool CRD) — separate mechanism

The `brain-pool` / `sandbox-workerpool` pods (the `ateom` container) are created
by Substrate's **WorkerPool controller**, not a static Deployment — so a
Deployment patch would be reverted by the controller. Their requests must be set
in the **WorkerPool spec's pod template** (or Substrate's worker defaults). If
the WorkerPool CRD doesn't yet expose container resources, that's a small
upstream ask to Substrate. Tracked here as a follow-up; the actor sandboxes
themselves are gVisor-isolated and sized by Substrate, so this is lower priority
than the control-plane components above.

## The real production answer

This overlay is the **post-install stopgap**. For true production, upstream
these values into the Substrate install (its manifests / Helm values, in a fork
if needed) so they're applied at install time and never need re-patching.
