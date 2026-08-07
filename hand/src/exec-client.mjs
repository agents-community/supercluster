// Client for the executor container (#74).
//
// Its own module because BOTH the MCP server and the gateway need it: the
// gateway configures git and clones repositories, and after the split those
// happen in the executor too. Running `git config --global` in the gateway
// would write a gitconfig in the wrong container — git itself now runs next to
// the workspace, not next to the credentials.
//
// Loopback: containers in one actor share a network namespace. Nothing outside
// the actor can reach the executor, because atenet routes only to :80.

const EXEC_URL = process.env.EXEC_URL || "http://127.0.0.1:8081";

async function call(path, body, timeoutMs) {
  const r = await fetch(`${EXEC_URL}${path}`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(body),
    signal: AbortSignal.timeout(timeoutMs),
  });
  if (!r.ok) throw new Error(`executor returned ${r.status}`);
  const out = await r.json();
  if (out.ok === false) throw new Error(out.error || "executor failed");
  return out;
}

/**
 * Run a command in the executor. `env` carries only credentials the agent
 * declared; the executor adds nothing else from its own environment.
 */
export function execRun(argv, { cwd, timeoutMs = 120000, env } = {}) {
  return call("/run", { argv, cwd, timeoutMs, env }, timeoutMs + 15000);
}

/** Filesystem operation in the executor, which is the only container that mounts /workspace. */
export function execFs(op, args = {}) {
  return call("/fs", { op, ...args }, 30000);
}

/** Ready when the executor answers; the gateway waits for this before serving tools. */
export async function execReady(timeoutMs = 30000) {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    try {
      const r = await fetch(`${EXEC_URL}/healthz`, { signal: AbortSignal.timeout(3000) });
      if (r.ok) return true;
    } catch { /* not up yet */ }
    if (Date.now() > deadline) return false;
    await new Promise((r) => setTimeout(r, 300));
  }
}
