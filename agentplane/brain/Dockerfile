# agentplane brain image, claude-code variant: ONLY the Claude Code harness.
#
# One image per harness (see Dockerfile.pi for the rationale): gVisor's gofer
# pays a per-file cost at workload creation, and a multi-harness node_modules
# pushed restores past the RPC deadline. The harness registry loads lazily, so
# absent SDKs are never imported.
FROM node:20-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl git \
  && rm -rf /var/lib/apt/lists/*

# The harness CLI (the SDK drives it) — global so `claude` is on PATH too.
RUN npm install -g @anthropic-ai/claude-code@latest && npm cache clean --force

WORKDIR /app
RUN npm install @anthropic-ai/claude-agent-sdk@latest && npm cache clean --force

COPY server.mjs runtime.mjs identity.mjs /app/
COPY harness /app/harness
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENV PORT=8080
ENTRYPOINT ["/entrypoint.sh"]
