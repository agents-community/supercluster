# Contributing to supercluster

## The workflow: issue-first, one feature per PR

1. **Open a GitHub issue before writing code.** Describe the feature or fix:
   what, why, the user-visible behavior, and anything it touches (API, deploy,
   docs). Design discussion happens on the issue, not in review.
2. **One feature per PR**, branched from `main`, referencing the issue
   (`Closes #N`). Small, reviewable, revertable. Unrelated fixes go in their
   own PR — even one-liners.
3. **Docs move with the code.** If a PR changes behavior, the same PR updates
   the docs that describe it (`README.md`, `agentplane/docs/`,
   `andromeda/ONBOARDING.md`, galaxy READMEs). Doc drift is a review blocker.
4. **CI must be green** (unit level: Go build/vet/test against the
   pinned+patched Substrate). Run the local integration layers when you touch
   their area — see [`test/README.md`](test/README.md).
5. Merge via PR; `main` is never pushed to directly.

## Where things live

| Area | Start at |
|---|---|
| Control plane / runtime / API | [`agentplane/CONTRIBUTING.md`](agentplane/CONTRIBUTING.md) |
| TUI | [`andromeda/CONTRIBUTING.md`](andromeda/CONTRIBUTING.md) |
| Hand (MCP tool gateway) | [`hand/README.md`](hand/README.md) |
| Deploy + Substrate patches | [`infra/README.md`](infra/README.md), [`infra/substrate-patches/README.md`](infra/substrate-patches/README.md) |

## Substrate dependency

We build against a sibling checkout of Agent Substrate pinned at
`infra/substrate-patches/BASE_COMMIT` with our patches applied — CI enforces
this on every run. Syncing to a newer upstream is its own PR (sync, rebase
patches, update `BASE_COMMIT`, verify build), per the process in
`infra/substrate-patches/README.md`.
