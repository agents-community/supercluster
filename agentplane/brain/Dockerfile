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
# PINNED, deliberately. These versions decide whether a subagent inherits the
# agent's `deny` list — i.e. whether model-generated code can run in the BRAIN
# instead of the sandboxed hand. With @latest, two builds of this same file
# produced different security behaviour: an escrowed transcript from
# 2026-08-03 shows `Bash` executing under an agent that denied it, on an image
# built days earlier. Bump these on purpose, and re-run the boundary test in
# test/smoke-live.sh afterwards.
ARG CLAUDE_CODE_VERSION=2.1.220
ARG CLAUDE_AGENT_SDK_VERSION=0.3.220
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" && npm cache clean --force

WORKDIR /app
# OTEL for the brain. Claude Code is installed globally and resolves its OWN
# copy of @opentelemetry/api, so the brain needs its own SDK here — see otel.mjs.
RUN npm install "@anthropic-ai/claude-agent-sdk@${CLAUDE_AGENT_SDK_VERSION}" @opentelemetry/api@^1 @opentelemetry/sdk-node @opentelemetry/exporter-trace-otlp-grpc && npm cache clean --force

# Every file runtime.mjs or a harness imports must be listed here. A missing
# one fails at ACTOR START, not at build: node resolves imports lazily and
# `docker build` is perfectly happy. approval.mjs was added and not copied,
# and otel.mjs was already missing from the pi/codex images — invisible only
# because those had not been rebuilt since it landed.
COPY server.mjs runtime.mjs identity.mjs otel.mjs approval.mjs /app/
COPY harness /app/harness
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

ENV PORT=8080
ENTRYPOINT ["/entrypoint.sh"]
