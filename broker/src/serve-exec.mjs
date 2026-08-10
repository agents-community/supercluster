// Runs a command in a hand actor via serve's internal exec endpoint, which calls
// the control plane's ExecActor (`runsc exec`). This is how the broker executes
// commands with the runtime OUTSIDE the sandbox: nothing of ours runs in the
// actor — the command is launched into it from the control plane.
//
// Auth is by workload identity (F15): the broker presents its projected
// ServiceAccount token; serve validates it with a k8s TokenReview and checks the
// SA + audience. No shared secret. The token is re-read per request because
// kubelet rotates it well before expiry.

import { readFile } from "node:fs/promises";

const SERVE_BASE = process.env.AGENTPLANE_SERVE_BASE || "http://agentplane-serve.agentplane.svc:7433";
const TOKEN_PATH = process.env.AGENTPLANE_BROKER_TOKEN_FILE || "/run/broker-token/token";
const EXEC_CONTAINER = process.env.EXEC_CONTAINER || "exec";

async function bearer() {
  const t = (await readFile(TOKEN_PATH, "utf8")).trim();
  return `Bearer ${t}`;
}

/**
 * Run argv in the hand actor's exec container.
 * @returns {Promise<{stdout:string, stderr:string, exitCode:number}>}
 */
export async function execActor(handActor, argv, { cwd = "/workspace", env = {}, timeoutMs = 120000 } = {}) {
  const r = await fetch(`${SERVE_BASE}/v1/internal/exec`, {
    method: "POST",
    headers: { "content-type": "application/json", authorization: await bearer() },
    body: JSON.stringify({ actor: handActor, container: EXEC_CONTAINER, argv, cwd, env, timeoutMs }),
    signal: AbortSignal.timeout(timeoutMs + 20000),
  });
  const text = await r.text();
  if (!r.ok) {
    let msg = text;
    try { msg = JSON.parse(text).error || text; } catch { /* raw */ }
    throw new Error(`exec via serve failed (${r.status}): ${msg}`);
  }
  return JSON.parse(text);
}
