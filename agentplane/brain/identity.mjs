// Actor identity, resolved LAZILY at call time — never captured at module load.
//
// A golden snapshot freezes this process before its first turn; every session
// actor restored from that snapshot shares the same code but MUST observe its
// OWN identity. Reading /run/ate/actor-id per call (not once at boot) is what
// makes that correct — see docs/brain-server.md "Golden-time identity capture".

import { readFileSync } from "node:fs";

/** The current actor's id (e.g. "b-sess-abc123"), or a local fallback. */
export function actorIdentity() {
  try {
    return readFileSync("/run/ate/actor-id", "utf8").trim();
  } catch {
    return process.env.SESSION_NAME || "local"; // not running on Substrate
  }
}

/**
 * The paired hand actor's MCP URL for this brain, by the b-X ↔ h-X convention
 * (D3). Overridable via HAND_MCP_URL. Returns null when there is no pairing
 * (non-brain identity / off-Substrate).
 */
export function handURL() {
  if (process.env.HAND_MCP_URL) return process.env.HAND_MCP_URL;
  const id = actorIdentity();
  if (id.startsWith("b-")) {
    const atespace = process.env.AGENTPLANE_ATESPACE || "agents";
    return `http://h-${id.slice(2)}.${atespace}.actors.resources.substrate.ate.dev/mcp`;
  }
  return null;
}
