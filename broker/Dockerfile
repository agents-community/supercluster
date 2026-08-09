# agentplane broker — the mediator between brain and hand.
#
# Node (like brain/hand) rather than distroless-static, because it runs the MCP
# SDK. It holds credentials and policy, so it carries only what it needs: the
# runtime, the SDK, and the broker source — no toolchain, no shell utilities an
# attacker could reach.
FROM node:22-slim

WORKDIR /app
# Install deps against the lockfile first so the layer caches independently of
# source edits.
COPY package.json package-lock.json* ./
RUN npm install --omit=dev --no-audit --no-fund

COPY src/ ./src/

ENV PORT=8088
EXPOSE 8088

# Non-root: unlike the hand's executor, this pod is an ordinary container (not a
# gVisor actor mounting a root-owned DurableDir), so dropping privilege works.
USER node

ENTRYPOINT ["node", "src/server.mjs"]
