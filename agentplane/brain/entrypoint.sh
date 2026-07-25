#!/bin/sh
# agentplane brain entrypoint. Identity (session name, paired hand) is derived
# LAZILY inside the server from /run/ate/actor-id, so golden snapshots restore
# with the correct per-actor identity.
set -u
export HOME=/root
mkdir -p /workspace
echo "[brain] booting; identity resolved lazily from /run/ate/actor-id"
exec node /app/server.mjs
