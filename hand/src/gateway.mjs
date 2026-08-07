// The hand's GATEWAY: it is not only a tool server, it is an MCP *client* to the
// user's own MCP servers. It connects outward to each upstream, attaches that
// upstream's credential, and re-publishes the upstream's tools as its own
// (namespaced <upstream>__<tool>). The brain talks to ONE door — the hand — and
// gets bash/fs PLUS every server the user brings, with all credentials and
// egress contained HERE.
//
// State is MEMORY ONLY. Nothing here is written to /workspace (the durable dir),
// so credentials never land in the workspace checkpoint. They are re-injected on
// a cold resume (the BYO-key tradeoff), and purged when the session is deleted.

import { writeFileSync, existsSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

// Environment variables the hand DELIBERATELY exposes to tool subprocesses:
// credentials the agent declared and is meant to use. Everything else in the
// hand's own environment is scrubbed by toolEnv() — see server.mjs.
//
// This exists because a tool subprocess used to inherit process.env whole,
// which handed model-written code HAND_ADMIN_TOKEN — a token mounted into
// EVERY hand, not just this one (#74).
export const exposedToTools = new Set();

// name -> { url, headers, client, tools: [{ name, description, inputSchema }] }
const upstreams = new Map();

const SEP = "__"; // <upstream>__<tool> — matches how the brain sees mcp__hand__…

export function upstreamStatus() {
  return [...upstreams.entries()].map(([name, u]) => ({
    name, url: u.url, tools: u.tools.map((t) => t.name),
  }));
}

// The upstream tools re-published as the hand's own, namespaced and labeled.
export function federatedTools() {
  const out = [];
  for (const [name, u] of upstreams) {
    for (const t of u.tools) {
      out.push({
        name: `${name}${SEP}${t.name}`,
        description: `[${name}] ${t.description || ""}`.trim(),
        inputSchema: t.inputSchema || { type: "object" },
      });
    }
  }
  return out;
}

// Is this tool name one of ours to proxy? Returns {client, tool} or null.
export function resolveFederated(toolName) {
  const idx = toolName.indexOf(SEP);
  if (idx < 0) return null;
  const name = toolName.slice(0, idx);
  const tool = toolName.slice(idx + SEP.length);
  const u = upstreams.get(name);
  return u ? { client: u.client, tool } : null;
}

// Forward a call to the owning upstream and return its MCP result verbatim.
export async function callFederated(toolName, args) {
  const r = resolveFederated(toolName);
  if (!r) throw new Error(`no upstream owns tool ${toolName}`);
  return r.client.callTool({ name: r.tool, arguments: args || {} });
}

// Connect (or reconnect) an upstream MCP server. The credential lives in
// `headers` and stays in this process's memory — attached to every request.
export async function connectUpstream({ name, url, headers }) {
  const prev = upstreams.get(name);
  if (prev?.client) { try { await prev.client.close(); } catch { /* best-effort */ } }

  const transport = new StreamableHTTPClientTransport(new URL(url), {
    requestInit: headers ? { headers } : undefined,
  });
  const client = new Client({ name: "agentplane-hand-gateway", version: "0.1.0" });
  await client.connect(transport);
  const { tools } = await client.listTools();
  upstreams.set(name, { url, headers, client, tools });
  return tools.map((t) => t.name);
}

export async function removeUpstream(name) {
  const u = upstreams.get(name);
  if (u?.client) { try { await u.client.close(); } catch { /* best-effort */ } }
  return upstreams.delete(name);
}

// Pull credentials from serve using this session's grant, and apply them. The
// value materializes ONLY here, in the hand's memory — serve holds the grant→
// vault mapping; the brain is never involved. Called when serve pushes a grant
// at session start (and could be re-run on resume). git → .git-credentials;
// env → process.env; header → exposed as HAND_CRED_<NAME> for opt-in upstreams.
export async function pullCredentials({ serveBase, grant, credentials }) {
  const applied = [];
  for (const name of credentials || []) {
    try {
      const r = await fetch(`${serveBase}/v1/hand/credentials/${encodeURIComponent(name)}`, {
        headers: { authorization: `Bearer ${grant}` },
      });
      if (!r.ok) { applied.push({ name, ok: false, status: r.status }); continue; }
      const p = await r.json();
      if (p.type === "git") {
        setGitCredentials([{ host: p.host, username: p.username, token: p.value }]);
        applied.push({ name, ok: true, type: "git", host: p.host || "github.com" });
      } else if (p.type === "env" && p.varName) {
        process.env[p.varName] = p.value;
        exposedToTools.add(p.varName); // the agent asked for this one
        applied.push({ name, ok: true, type: "env", var: p.varName });
      } else if (p.type === "header") {
        {
          const varName = `HAND_CRED_${name.toUpperCase().replace(/[^A-Z0-9]/g, "_")}`;
          process.env[varName] = p.value;
          exposedToTools.add(varName); // the agent asked for this one
        }
        applied.push({ name, ok: true, type: "header" });
      } else {
        applied.push({ name, ok: false, reason: `unsupported type ${p.type}` });
      }
    } catch (e) {
      applied.push({ name, ok: false, error: e.message });
    }
  }
  return applied;
}

// Git credentials for on-hand `git` run via the bash tool. Written to
// $HOME/.git-credentials (mode 0600) — OUTSIDE /workspace, so the token is not
// part of the durable workspace checkpoint. git's `store` helper reads it; the
// token never appears in a command line (so it never reaches the LLM/event log).
export function setGitCredentials(creds) {
  if (!Array.isArray(creds) || creds.length === 0) return 0;
  const home = process.env.HOME || "/root";
  const lines = creds.map((c) => {
    const host = c.host || "github.com";
    const user = encodeURIComponent(c.username || "x-access-token");
    const tok = encodeURIComponent(c.token);
    return `https://${user}:${tok}@${host}`;
  });
  writeFileSync(`${home}/.git-credentials`, lines.join("\n") + "\n", { mode: 0o600 });
  spawnSync("git", ["config", "--global", "credential.helper", "store"]);
  // A sane default identity so `git commit` works without extra setup.
  if (!spawnSync("git", ["config", "--global", "user.email"]).stdout?.length) {
    spawnSync("git", ["config", "--global", "user.email", "agent@agentplane.local"]);
    spawnSync("git", ["config", "--global", "user.name", "agentplane"]);
  }
  return creds.length;
}

// ---- declarative repositories, cloned through the git proxy (#49) ----------

// Point git at the proxy and give it the session grant, so the credential is
// attached OUTSIDE this sandbox. Nothing secret is written here: the grant is
// short-lived and scoped to this session's declared credentials, and the real
// token never arrives.
export function configureGitProxy({ proxyBase, grant, credential }) {
  if (!proxyBase) return false;
  // Never prompt. Without this, a 401 makes git block asking for a username
  // that no one can type, and the clone fails with a message about terminals
  // instead of about auth — which is what happened on the first live run.
  process.env.GIT_TERMINAL_PROMPT = "0";
  // The proxy authenticates on our behalf, so git must not go looking for
  // credentials for the proxy host itself: `credential.helper store` (set by
  // the legacy pull path) would otherwise try, find none, and prompt.
  spawnSync("git", ["config", "--global", `credential.${proxyBase.replace(/\/$/, "")}.helper`, ""]);
  // insteadOf rewrites https://github.com/... to the proxy, so the URL the
  // model sees and types stays the normal public one.
  spawnSync("git", ["config", "--global", `url.${proxyBase.replace(/\/$/, "")}/gh/.insteadOf`,
    "https://github.com/"]);
  // extraHeader travels with every git HTTP request; the proxy reads it to
  // decide whose credential to attach, then strips it before calling upstream.
  spawnSync("git", ["config", "--global", "--unset-all", "http.extraHeader"]);
  if (grant) {
    spawnSync("git", ["config", "--global", "--add", "http.extraHeader",
      `X-Agentplane-Grant: ${grant}`]);
  }
  if (credential) {
    spawnSync("git", ["config", "--global", "--add", "http.extraHeader",
      `X-Agentplane-Credential: ${credential}`]);
  }
  // Verify rather than assume: spawnSync failures are silent, and a config that
  // did not apply looks identical to one that did until a clone fails oddly.
  const check = spawnSync("git", ["config", "--global", "--get-regexp", "^url\\."],
    { encoding: "utf8" });
  const applied = (check.stdout || "").includes(proxyBase.replace(/\/$/, ""));
  if (!applied) {
    console.error("git proxy config did NOT apply:", (check.stderr || check.stdout || "").slice(0, 200));
  }
  return applied;
}

// Clone the agent's declared repositories into the sandbox. Idempotent: an
// existing checkout is left alone, because /workspace is durable and a session
// resuming after a suspend must not lose uncommitted work.
export function cloneRepositories(repos) {
  const out = [];
  for (const r of repos || []) {
    const dest = r.mountPath;
    if (!dest || !dest.startsWith("/workspace/")) {
      out.push({ url: r.url, ok: false, reason: "mountPath must be under /workspace/" });
      continue;
    }
    if (existsSync(`${dest}/.git`)) {
      out.push({ url: r.url, ok: true, skipped: "already checked out" });
      continue;
    }
    const args = ["clone"];
    // A shallow clone by default: agents rarely need full history, and a large
    // repo otherwise dominates session setup.
    if (!r.checkout?.commit) args.push("--depth", "1");
    if (r.checkout?.branch) args.push("--branch", r.checkout.branch);
    else if (r.checkout?.tag) args.push("--branch", r.checkout.tag);
    args.push(r.url, dest);

    const res = spawnSync("git", args, {
      encoding: "utf8", timeout: 10 * 60 * 1000,
      env: { ...process.env, GIT_TERMINAL_PROMPT: "0" },
    });
    if (res.status !== 0) {
      // stderr can contain a URL; it never contains the token, which lives only
      // in the proxy. Truncated so a huge git error cannot flood the log.
      out.push({ url: r.url, ok: false, error: (res.stderr || "").trim().slice(0, 300) });
      continue;
    }
    if (r.checkout?.commit) {
      const co = spawnSync("git", ["-C", dest, "checkout", r.checkout.commit], { encoding: "utf8" });
      if (co.status !== 0) {
        out.push({ url: r.url, ok: false, error: `checkout ${r.checkout.commit}: ${(co.stderr || "").trim().slice(0, 200)}` });
        continue;
      }
    }
    out.push({ url: r.url, ok: true, mountPath: dest });
  }
  return out;
}
