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

import { writeFileSync } from "node:fs";
import { spawnSync } from "node:child_process";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

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
        applied.push({ name, ok: true, type: "env", var: p.varName });
      } else if (p.type === "header") {
        process.env[`HAND_CRED_${name.toUpperCase().replace(/[^A-Z0-9]/g, "_")}`] = p.value;
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
