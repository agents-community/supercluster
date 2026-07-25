#!/usr/bin/env bash
# Production resource requests + limits for Substrate's ate-system components.
#
# These workloads ship WITHOUT requests (fine for a POC; GKE flags it as "Low
# resource requests" because the scheduler can then over-pack nodes and evict
# pods under pressure, and HPA can't function). This script sets sensible
# starting values — tune from real usage (kubectl top pods -n ate-system).
#
# `kubectl set resources` is idempotent and targets containers by name, so this
# is safe to re-run (e.g. after a Substrate re-install, which reverts the
# in-place values). For true GitOps, upstream these into the Substrate install
# manifests/values instead — this script is the post-install stopgap.
#
# NOTE: applying triggers a ROLLING RESTART of each workload. valkey rolls one
# pod at a time (its PDB), and ate-api-server / atenet-router have a brief
# control-plane blip. Run in a maintenance window.
set -euo pipefail
NS=ate-system

setr() { echo "→ $1"; kubectl -n "$NS" set resources "$1" "${@:2}"; }

# control-plane API (gRPC) — the busiest control component
setr deploy/ate-api-server-deployment \
  --containers=ate-api-server --requests=cpu=250m,memory=256Mi --limits=cpu=1,memory=512Mi

# reconcile controller — light, steady
setr deploy/ate-controller \
  --containers=ate-controller --requests=cpu=100m,memory=128Mi --limits=cpu=500m,memory=256Mi

# router (control) + envoy (data-plane proxy for all actor traffic)
setr deploy/atenet-router \
  --containers=atenet-router --requests=cpu=100m,memory=128Mi --limits=cpu=500m,memory=256Mi
setr deploy/atenet-router \
  --containers=envoy --requests=cpu=200m,memory=256Mi --limits=cpu=1,memory=512Mi

# in-cluster DNS
setr deploy/dns \
  --containers=coredns --requests=cpu=100m,memory=128Mi --limits=cpu=500m,memory=256Mi
setr deploy/dns \
  --containers=dns-controller --requests=cpu=50m,memory=64Mi --limits=cpu=200m,memory=128Mi

# atelet — per-node; holds the gVisor sandbox memory and does checkpoint/restore.
# NO MEMORY LIMIT ON PURPOSE: a too-low limit OOMKills atelet, which severs the
# worker pods on that node and yields "no free workers available" (learned the
# hard way — a 1Gi limit took the cluster down). Request + CPU limit only.
setr daemonset/atelet \
  --containers=atelet --requests=cpu=250m,memory=512Mi --limits=cpu=2

# valkey — the in-memory quorum datastore. Same rule: never cap its memory, or a
# growing dataset OOMKills a data-holding pod. Request + CPU limit only.
setr statefulset/valkey-cluster \
  --containers=valkey --requests=cpu=200m,memory=512Mi --limits=cpu=1

echo "done. brain-pool workers (WorkerPool CRD) are separate — see README."
