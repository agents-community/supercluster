# Substrate dependency & patch process

agentplane builds **against** [Agent Substrate](https://github.com/agent-substrate/substrate)
and carries a few **local changes on top of upstream `main`**. This directory is
the source of truth for those changes so they are versioned, reviewable, and
never silently lost on a sync.

## How Substrate is wired in

- Our Go code imports only Substrate's control-plane protobufs:
  `github.com/agent-substrate/substrate/pkg/proto/ateapipb`.
- `agentplane/go.mod` resolves that module from a **local sibling checkout**:

  ```
  replace github.com/agent-substrate/substrate => ../../substrate
  ```

  So the layout on disk must be:

  ```
  code_vault/
  ├── agentplane/          ← this repo (supercluster)
  │   └── agentplane/      ← the Go module (has the replace)
  └── substrate/           ← upstream checkout the replace points at
  ```

- **The checkout tracks upstream `main`**; we do not maintain a long-lived fork.
  Our deltas live here as patches and are re-applied after each sync.

## What we patch, and why

| Patch | Component | Why |
|-------|-----------|-----|
| `0001-atenet-stream-timeout.patch` | `cmd/atenet` (Envoy xDS router) | **Load-bearing.** Disables the 10s route total-timeout and sets a 300s idle timeout so long-lived SSE streams (the agent turn stream behind `/message/stream`) aren't cut off mid-turn. Without it, every streamed turn dies at 10s. |
| `0002-demo-claude-code-multiplex.patch` | `demos/…` | A local demo template; not required for production. |

`BASE_COMMIT` records the upstream commit these patches were generated against
(`git apply` is happiest re-applying onto the same base; a newer base may need a
rebase — see below).

> **Scope note:** agentplane binaries only import `ateapipb`, so the `atenet`
> patch does **not** affect the agentplane/serve/hand builds. It changes the
> **atenet image**, which is built and deployed separately — re-applying the
> patch only takes effect after you rebuild + redeploy atenet.

## First-time setup

```bash
# from code_vault/ (the parent of this repo)
git clone https://github.com/agent-substrate/substrate.git
cd substrate
git checkout main
# apply our deltas on top of main
git apply /path/to/supercluster/infra/substrate-patches/0001-atenet-stream-timeout.patch
git apply /path/to/supercluster/infra/substrate-patches/0002-demo-claude-code-multiplex.patch
```

Then build agentplane as usual (`cd ../agentplane/agentplane && go build ./...`).

## Syncing from upstream `main` (the routine)

Do this whenever you want the latest Substrate. **Sync first, then re-apply our
changes on top** — never edit the checkout and pull over it.

```bash
cd code_vault/substrate

# 1. make sure the tree is clean; our changes already live as patches here,
#    so it's safe to discard the working copy before syncing.
git checkout -- . && git clean -fd

# 2. pull upstream main
git fetch origin
git checkout main
git reset --hard origin/main

# 3. re-apply our patches on top
for p in /path/to/supercluster/infra/substrate-patches/0*.patch; do
  git apply "$p" || { echo "CONFLICT in $p — resolve, then regenerate (below)"; break; }
done

# 4. rebuild/redeploy what changed (e.g. atenet if 0001 touched it)
```

If a patch no longer applies (upstream moved the code), apply it with
`git apply --3way`, resolve the conflict by hand, then **regenerate the patch**
(next section) and bump `BASE_COMMIT`.

## Making or updating a Substrate change

1. Edit the file(s) directly in the `substrate/` checkout.
2. Regenerate the patch from the diff:

   ```bash
   cd code_vault/substrate
   git diff -- cmd/atenet/internal/router/xds.go \
     > /path/to/supercluster/infra/substrate-patches/0001-atenet-stream-timeout.patch
   ```

3. Update `BASE_COMMIT` if you re-synced:

   ```bash
   git rev-parse HEAD > /path/to/supercluster/infra/substrate-patches/BASE_COMMIT
   ```

4. Commit the updated patch **in supercluster** (this repo) with a message
   explaining the change. The patch — not the checkout — is the record.

## Upstreaming

`0001` is a genuine fix and a good upstream-PR candidate. When a patch lands
upstream, delete it here after the next sync includes it, so we only carry deltas
that aren't yet upstream.

## Upgrading the Substrate install (learned the hard way, 2026-08-02)

The c1ab095 → 860250b upgrade caused a multi-hour outage. Every failure below
was avoidable with a pre-flight check; run these in order.

### Before you start

1. **Diff the manifests for RENAMES.** This was the single biggest cause of
   pain — three workloads were renamed upstream, and Kubernetes has no notion
   of a rename: the installer creates the new object while the **old one keeps
   running**, so both serve at once.

   ```bash
   git diff <old>..<new> -- manifests/ | grep -E '^[-+]\s+name:'
   ```

   In this upgrade: `ate-api-server-deployment` → `ate-api-server` (both
   matched the Service selector, so a third of control-plane traffic hit the
   *old* binary), and `brain-pool-deployment` → `brain-pool` (the new pools
   crash-looped while the old ones served, so the controller dialed ateom
   sockets on pods that no longer existed). Delete the orphans explicitly.

2. **Check the protos for reserved fields.** protojson *rejects* a reserved
   field rather than ignoring it, so every record written by the old version
   becomes unreadable:

   ```bash
   git diff <old>..<new> -- '*.proto' | grep -E '^\+\s+reserved'
   ```

   Here `latest_snapshot_info` was reserved, and every actor record in valkey
   failed to unmarshal until stripped — **across all three shards**
   (`valkey-cli --scan` only walks the node you ask, not the cluster).

3. **Plan to rebuild ateom in the same pass.** `--deploy-ate-system` does *not*
   build it (it ships via our WorkerPool CRs), but the new controller injects
   `--atunnel-*` flags into it. Skew guarantees `unknown flag: …` crash-loops.

   ```bash
   KO_DOCKER_REPO=gcr.io/<proj>/ate-images KO_DEFAULTPLATFORMS=linux/amd64 \
     ./hack/run-tool.sh ko build ./cmd/ateom-gvisor      # no extra flags: --bare/--tags skip the manifest push
   kubectl -n agentplane patch workerpool <pool> --type=merge \
     -p '{"spec":{"ateomImage":"<new digest>"}}'
   ```

4. **Keep the patches on a branch, not in the working tree.** `agentplane-patches`
   in the Substrate checkout. A stray `git checkout .` would otherwise silently
   drop the atenet stream-timeout fix and rebuild a clean atenet whose 10s route
   timeout kills every SSE turn — healthy-looking and completely broken.

### Running it

```bash
kubectl apply -f manifests/ate-install/generated/role.yaml   # FIRST: RBAC gates the controller
./hack/install-ate.sh --deploy-ate-system                    # retry on push failures; ko resumes
./hack/install-ate.sh --create-valkey-ca-certs-secret        # if valkey peers fail TLS
```

RBAC first because the controller watches resources the old ClusterRole didn't
grant (`networkpolicies`), and without it the manager aborts on cache-sync and
crash-loops — while the install applies that role *last*.

Pushes fail intermittently with `tls: bad record MAC`. ko skips blobs already
pushed, so retrying converges; `hack/` has no retry wrapper, so loop it.

### After

- Old renamed Deployments deleted (step 1) — check `kubectl get endpoints` to
  confirm only new pods serve.
- valkey: `cluster_state:ok`. If nodes restarted together their `nodes.conf`
  holds stale IPs — `CLUSTER MEET <current-ip> 6379` from one node re-meshes,
  but only *after* TLS works (a 2-root trust bundle: servicedns + podidentity).
- Golden actors that crashed during the broken window stay `STATUS_CRASHED` and
  the controller refuses to resume them — delete and recreate the agents.
- `./test/smoke-live.sh` must pass before declaring done.
