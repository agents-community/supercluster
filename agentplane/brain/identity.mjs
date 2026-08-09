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

// The hand MCP URL serve pushed for THIS session (over /options), routing the
// brain through the broker instead of straight to the hand. Session-scoped
// process state, and safe under snapshots: a golden is frozen before any turn,
// so it is null at golden time; every restored actor starts null and gets its
// own push after restore — the same lazy-per-actor discipline as actorIdentity.
let pushedHandURL = null;

/** Set the hand MCP URL for this session (serve → /options → runtime). */
export function setHandURL(u) {
  if (typeof u === "string" && u) pushedHandURL = u;
}

/**
 * The paired hand actor's MCP URL for this brain, by the b-X ↔ h-X convention
 * (D3). Precedence: HAND_MCP_URL env (operator/test override) > serve-pushed
 * (the broker) > the computed hand address. Returns null when there is no
 * pairing (non-brain identity / off-Substrate).
 */
export function handURL() {
  if (process.env.HAND_MCP_URL) return process.env.HAND_MCP_URL;
  if (pushedHandURL) return pushedHandURL;
  const id = actorIdentity();
  if (id.startsWith("b-")) {
    const atespace = process.env.AGENTPLANE_ATESPACE || "agents";
    return `http://h-${id.slice(2)}.${atespace}.actors.resources.substrate.ate.dev/mcp`;
  }
  return null;
}
