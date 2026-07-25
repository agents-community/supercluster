<!--
Thanks for contributing to Andromeda! Keep PRs focused on one change.
See CONTRIBUTING.md for setup and the full guidance.
-->

## What & why

<!-- What does this change do, and what problem does it solve? Link any issue. -->

Closes #

## How I tested

<!-- Andromeda is a TUI — please include a terminal recording or screenshot. -->

- [ ] `node src/cli.mjs` (list mode) works
- [ ] started a session (`--agent …`) and sent messages
- [ ] detach (`esc`) + re-attach (`--session …`) replays history
- [ ] suspend (`ctrl+s`) wakes on the next message

_Node version:_ `node -v` →

## Checklist

- [ ] One logical change; no unrelated churn
- [ ] Matches the existing style (ESM, `React.createElement`/`h`, no JSX/TS, no build step)
- [ ] No new runtime dependency (or explained why it's needed)
- [ ] **No secrets, tokens, or hardcoded host endpoints committed**
- [ ] Docs (README / CONTRIBUTING / ONBOARDING) updated if behavior changed
- [ ] No stray `console.log`, `node_modules`, or `.env`
