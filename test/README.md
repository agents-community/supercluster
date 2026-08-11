# Testing

CI (`.github/workflows/ci.yml`) runs the **unit level**: Go build + vet +
unit tests, compiled against Substrate cloned at
`infra/substrate-patches/BASE_COMMIT` with our patches applied — so a patch
that stops applying fails CI loudly.

The **integration layers run locally**:

| Layer | Command | Needs |
|---|---|---|
| hand MCP smoke | `docker run --rm -v "$PWD/hand":/app -w /app node:22-slim node test/mcp-smoke.mjs` | Docker (or node ≥ 22: `cd hand && node test/mcp-smoke.mjs`) |
| live smoke | `AGENTPLANE_URL=https://… AGENTPLANE_TOKEN=apl_… ./test/smoke-live.sh` | deployed agentplane; costs one model turn |

Run the hand smoke before merging changes to `hand/`; run the live smoke
before handing the system to testers or after a redeploy.
