#!/bin/sh
# agentplane hand entrypoint. The MCP tool server operates on /workspace (a
# Substrate DurableDir volume — artifacts persist across checkpoints).
set -u
export HOME=/root
mkdir -p /workspace
echo "[hand] booting MCP tool server"
exec node /app/src/server.mjs
