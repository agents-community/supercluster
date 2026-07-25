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
