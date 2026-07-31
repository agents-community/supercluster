// agentplane hand — a durable TOOL server AND an MCP gateway, one per session,
// paired to a brain (h-<id> ↔ b-<id>). It exposes execution + filesystem tools
// over MCP (streamable HTTP) on a durable /workspace, AND federates the user's
// own MCP servers: it connects outward to each, attaches its credential, and
// re-publishes their tools as its own (namespaced <upstream>__<tool>). The brain
// (which owns the agent loop) calls one door — the hand; ALL commands run HERE,
// every artifact lives in THIS actor's /workspace, and every credential + egress
// is contained HERE — never in the brain.
//
// Protocol: MCP over streamable HTTP. Stateless per request; durability comes
// from the checkpointed /workspace, and the gateway's upstream connections +
// credentials live in this process's memory (see gateway.mjs).

import http from "node:http";
import { spawnSync } from "node:child_process";
import { mkdirSync, readFileSync, writeFileSync, readdirSync, statSync, globSync } from "node:fs";
import path from "node:path";
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js";
import {
  connectUpstream, removeUpstream, federatedTools, resolveFederated,
  callFederated, setGitCredentials, upstreamStatus, pullCredentials,
} from "./gateway.mjs";
import { initOtel } from "./otel.mjs";
import { trace, context, propagation, SpanStatusCode } from "@opentelemetry/api";

initOtel(); // start tracing before anything runs (no-op if OTEL endpoint unset)
const tracer = trace.getTracer("agentplane-hand");

const PORT = Number(process.env.PORT || 8080);
const WORKDIR = process.env.HAND_WORKDIR || "/workspace";
const ADMIN_TOKEN = process.env.HAND_ADMIN_TOKEN || ""; // gate /admin; empty = open (dev)
mkdirSync(WORKDIR, { recursive: true });

// Resolve a caller-supplied path inside the workspace (absolute paths under
// /workspace are honored; relative ones resolve against it). Refuses escapes.
function resolveInWorkdir(p) {
  const abs = path.resolve(WORKDIR, p || ".");
  if (abs !== WORKDIR && !abs.startsWith(WORKDIR + path.sep)) {
    throw new Error(`path escapes the workspace: ${p}`);
  }
  return abs;
}

const ok = (text) => ({ content: [{ type: "text", text }] });
const errResult = (text) => ({ content: [{ type: "text", text }], isError: true });

// The hand's OWN tools — JSON Schema so the gateway can merge them with the
// upstreams' schemas verbatim. Handlers run locally on /workspace.
// Descriptions are written to READ AS THE MODEL'S PRIMARY TOOLS — this is its
// terminal and filesystem. With the brain's built-in Bash/Read/Write denied,
// these are the only way to touch a shell or files, so the model reaches for
// them on its own; no system-prompt instruction is needed.
// Descriptions follow the Managed Agents guidance ("extremely detailed
// descriptions… three to four sentences"): they read as the model's PRIMARY
// terminal and filesystem. With the brain's built-in Bash/Read/Write denied,
// these are the only shell/file tools available, so the model reaches for them
// on its own — no system-prompt instruction required.
const OWN_TOOLS = [
  {
    name: "bash",
    description: "Execute a shell command and return its combined stdout+stderr with the exit code. This is your terminal and the primary way you act: use it to run and test code, build, run git (clone/commit/push), install packages, move or inspect files, curl URLs — any command-line operation. Commands run in your durable workspace directory, so files persist across turns. Prefer chaining with pipes and `&&` over many round-trips; output larger than ~10MB is truncated.",
    inputSchema: { type: "object", properties: { command: { type: "string", description: "the shell command to run (executed via sh -c in the workspace)" } }, required: ["command"] },
  },
  {
    name: "write",
    description: "Create a new file, or overwrite an existing one, with the exact contents you provide. Use this to author or fully rewrite any file in your workspace — source code, configs, docs. Parent directories are created automatically. For a small change to a large file, read it first, then write back the full updated contents.",
    inputSchema: { type: "object", properties: { path: { type: "string", description: "file path, absolute under the workspace or relative to it" }, content: { type: "string", description: "the full file contents to write" } }, required: ["path", "content"] },
  },
  {
    name: "read",
    description: "Read a file from your workspace and return its full text contents. Use this to open and inspect any file before editing it, or to review a command's output that was written to disk. The path may be absolute (under the workspace) or relative to the workspace root.",
    inputSchema: { type: "object", properties: { path: { type: "string", description: "file path to read" } }, required: ["path"] },
  },
  {
    name: "list",
    description: "List the entries of a directory in your workspace, marking each as a file or directory. Use this to explore the filesystem and discover what exists before reading files or running commands. The path defaults to the workspace root when omitted.",
    inputSchema: { type: "object", properties: { path: { type: "string", description: "directory to list; defaults to the workspace root" } } },
  },
  {
    name: "edit",
    description: "Replace an exact string in a file — the precise way to make a targeted change without rewriting the whole file. Provide the file path, the exact old_string to find (include enough surrounding context to make it unique), and the new_string to put in its place. Fails if old_string is absent or matches more than once; set replace_all to change every occurrence. Prefer this over write for small edits to existing files.",
    inputSchema: { type: "object", properties: { path: { type: "string" }, old_string: { type: "string", description: "exact text to find (must be unique unless replace_all)" }, new_string: { type: "string", description: "text to replace it with" }, replace_all: { type: "boolean", description: "replace every occurrence (default false)" } }, required: ["path", "old_string", "new_string"] },
  },
  {
    name: "glob",
    description: "Find files by glob pattern (e.g. `**/*.ts`, `src/**/test_*.py`) and return the matching paths, one per line. Use this to locate files by name or extension across the workspace before reading them. Matching is relative to `path`, which defaults to the workspace root. Returns '(no matches)' when nothing matches.",
    inputSchema: { type: "object", properties: { pattern: { type: "string", description: "glob pattern, e.g. **/*.js" }, path: { type: "string", description: "base directory; defaults to the workspace root" } }, required: ["pattern"] },
  },
  {
    name: "grep",
    description: "Search file contents with a regular expression (extended regex) and return matching lines prefixed with file:line, searching recursively. Use this to find where a symbol, string, or pattern appears across the codebase. Narrow the search with `path` (a subdirectory) and/or `glob` (e.g. `*.go`). Returns '(no matches)' when nothing is found.",
    inputSchema: { type: "object", properties: { pattern: { type: "string", description: "extended regular expression" }, path: { type: "string", description: "subdirectory to search; defaults to the workspace root" }, glob: { type: "string", description: "only search files matching this glob, e.g. *.py" } }, required: ["pattern"] },
  },
];
const OWN_NAMES = new Set(OWN_TOOLS.map((t) => t.name));

function runOwnTool(name, args) {
  switch (name) {
    case "bash": {
      const r = spawnSync("sh", ["-c", args.command], {
        cwd: WORKDIR, encoding: "utf8", timeout: 120000, maxBuffer: 10 * 1024 * 1024,
      });
      if (r.error) return errResult(`failed to run: ${r.error.message}`);
      const body = (r.stdout || "") + (r.stderr || "");
      return { content: [{ type: "text", text: `exit ${r.status}\n${body}` }], isError: r.status !== 0 };
    }
    case "write": {
      const abs = resolveInWorkdir(args.path);
      mkdirSync(path.dirname(abs), { recursive: true });
      writeFileSync(abs, args.content);
      return ok(`wrote ${abs} (${Buffer.byteLength(args.content)} bytes)`);
    }
    case "read":
      return ok(readFileSync(resolveInWorkdir(args.path), "utf8"));
    case "list": {
      const abs = resolveInWorkdir(args.path ?? ".");
      const entries = readdirSync(abs).map((n) => {
        const st = statSync(path.join(abs, n));
        return `${st.isDirectory() ? "d" : "-"} ${n}`;
      });
      return ok(entries.length ? entries.join("\n") : "(empty)");
    }
    case "edit": {
      const abs = resolveInWorkdir(args.path);
      const content = readFileSync(abs, "utf8");
      const { old_string, new_string, replace_all } = args;
      const count = content.split(old_string).length - 1;
      if (count === 0) return errResult(`old_string not found in ${args.path}`);
      if (count > 1 && !replace_all) return errResult(`old_string is not unique (${count} matches) — add surrounding context or set replace_all`);
      const out = replace_all ? content.split(old_string).join(new_string) : content.replace(old_string, new_string);
      writeFileSync(abs, out);
      return ok(`edited ${abs} (${count} replacement${count === 1 ? "" : "s"})`);
    }
    case "glob": {
      const base = resolveInWorkdir(args.path ?? ".");
      const matches = [...globSync(args.pattern, { cwd: base })];
      return ok(matches.length ? matches.join("\n") : "(no matches)");
    }
    case "grep": {
      const base = resolveInWorkdir(args.path ?? ".");
      const gargs = ["-rnE"];
      if (args.glob) gargs.push(`--include=${args.glob}`);
      gargs.push(args.pattern, ".");
      const r = spawnSync("grep", gargs, { cwd: base, encoding: "utf8", timeout: 60000, maxBuffer: 5 * 1024 * 1024 });
      if (r.status === 0) return ok(r.stdout || "(no output)");
      if (r.status === 1) return ok("(no matches)");
      return errResult(r.stderr || `grep exited ${r.status}`);
    }
    default:
      return errResult(`unknown tool: ${name}`);
  }
}

// A fresh MCP server per request (stateless streamable HTTP). Its tool list is
// the hand's own tools PLUS whatever upstreams are currently federated; calls
// dispatch locally or proxy to the owning upstream.
function buildServer() {
  const server = new Server(
    { name: "agentplane-hand", version: "0.2.0" },
    { capabilities: { tools: {} } },
  );
  server.setRequestHandler(ListToolsRequestSchema, async () => ({
    tools: [...OWN_TOOLS, ...federatedTools()],
  }));
  server.setRequestHandler(CallToolRequestSchema, async (req) => {
    const { name, arguments: args = {} } = req.params;
    // One span per tool execution — this is what makes hand work visible in
    // Jaeger (name + success only; no command/content, for privacy).
    return tracer.startActiveSpan(`hand.tool ${name}`, async (span) => {
      span.setAttribute("hand.tool", name);
      span.setAttribute("hand.tool.federated", !OWN_NAMES.has(name));
      try {
        let r;
        if (OWN_NAMES.has(name)) r = runOwnTool(name, args);
        else if (resolveFederated(name)) r = await callFederated(name, args);
        else r = errResult(`unknown tool: ${name}`);
        span.setAttribute("hand.tool.is_error", !!r.isError);
        if (r.isError) span.setStatus({ code: SpanStatusCode.ERROR });
        return r;
      } catch (e) {
        span.recordException(e);
        span.setStatus({ code: SpanStatusCode.ERROR, message: e.message });
        return errResult(e.message);
      } finally {
        span.end();
      }
    });
  });
  return server;
}

// ---- admin (control) plane: serve injects upstreams + credentials here -------

function adminAuthed(req) {
  if (!ADMIN_TOKEN) return true; // open in dev; set HAND_ADMIN_TOKEN in prod
  const h = req.headers["authorization"] || "";
  return h === `Bearer ${ADMIN_TOKEN}`;
}

async function readJson(req) {
  const chunks = [];
  for await (const c of req) chunks.push(c);
  const raw = Buffer.concat(chunks).toString("utf8") || "{}";
  return JSON.parse(raw);
}

// POST /admin/upstreams — register/replace federated MCP servers + git creds.
// Body: { upstreams: [{name,url,headers}], gitCredentials: [{host,username,token}] }
async function handleAdminUpstreams(req, res) {
  const body = await readJson(req);
  const registered = [];
  for (const u of body.upstreams || []) {
    const tools = await connectUpstream(u);
    registered.push({ name: u.name, tools });
  }
  const gitCount = setGitCredentials(body.gitCredentials || []);
  res.writeHead(200, { "content-type": "application/json" });
  res.end(JSON.stringify({ ok: true, registered, gitCredentials: gitCount }));
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");

  if (url.pathname === "/healthz") {
    res.writeHead(200, { "content-type": "application/json" });
    return res.end(JSON.stringify({ ok: true, workdir: WORKDIR, upstreams: upstreamStatus() }));
  }

  if (url.pathname.startsWith("/admin/")) {
    if (!adminAuthed(req)) { res.writeHead(401); return res.end("unauthorized"); }
    try {
      if (url.pathname === "/admin/upstreams" && req.method === "POST") return await handleAdminUpstreams(req, res);
      if (url.pathname === "/admin/grant" && req.method === "POST") {
        const applied = await pullCredentials(await readJson(req));
        res.writeHead(200, { "content-type": "application/json" });
        return res.end(JSON.stringify({ ok: true, applied }));
      }
      if (url.pathname === "/admin/status" && req.method === "GET") {
        res.writeHead(200, { "content-type": "application/json" });
        return res.end(JSON.stringify({ upstreams: upstreamStatus() }));
      }
      if (url.pathname.startsWith("/admin/upstreams/") && req.method === "DELETE") {
        const name = decodeURIComponent(url.pathname.split("/").pop());
        const removed = await removeUpstream(name);
        res.writeHead(200, { "content-type": "application/json" });
        return res.end(JSON.stringify({ ok: true, removed }));
      }
      res.writeHead(404); return res.end("not found");
    } catch (e) {
      res.writeHead(400, { "content-type": "application/json" });
      return res.end(JSON.stringify({ error: e.message }));
    }
  }

  if (url.pathname !== "/mcp") { res.writeHead(404); return res.end("not found"); }

  // Stateless: build a fresh server + transport per request.
  const mcp = buildServer();
  const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
  res.on("close", () => { transport.close(); mcp.close(); });
  // If the brain propagates a traceparent header, nest our tool spans under its
  // turn; otherwise they stand alone (still visible, correlate by time).
  const parentCtx = propagation.extract(context.active(), req.headers);
  try {
    await mcp.connect(transport);
    if (req.method === "POST") {
      const body = await readJson(req).catch(() => null);
      if (body === null) { res.writeHead(400); return res.end(JSON.stringify({ error: "invalid json" })); }
      await context.with(parentCtx, () => transport.handleRequest(req, res, body));
    } else {
      await context.with(parentCtx, () => transport.handleRequest(req, res));
    }
  } catch (e) {
    console.error("mcp error:", e.message);
    if (!res.headersSent) { res.writeHead(500); res.end(); }
  }
});

server.listen(PORT, () => console.log(`hand: MCP gateway on :${PORT} (workdir ${WORKDIR})`));
