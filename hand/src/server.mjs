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
import { createHash } from "node:crypto";
import path from "node:path";
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js";
import {
  connectUpstream, removeUpstream, federatedTools, resolveFederated,
  callFederated, setGitCredentials, upstreamStatus, pullCredentials,
  configureGitProxy, cloneRepositories, exposedToTools,
} from "./gateway.mjs";
import { execRun, execFs, execReady } from "./exec-client.mjs";
import { initOtel } from "./otel.mjs";
import { trace, context, propagation, SpanStatusCode } from "@opentelemetry/api";

initOtel(); // start tracing before anything runs (no-op if OTEL endpoint unset)
const tracer = trace.getTracer("agentplane-hand");

// Credentials the agent DECLARED travel per request, so the executor never
// holds them between calls and they never enter its standing environment.
function declaredEnv() {
  const out = {};
  for (const k of exposedToTools) {
    if (process.env[k] !== undefined) out[k] = process.env[k];
  }
  return out;
}

const PORT = Number(process.env.PORT || 8080);
const WORKDIR = process.env.HAND_WORKDIR || "/workspace";
// Where to verify admin grants. From the ENVIRONMENT, never the request body —
// a caller-supplied verification endpoint would be no check at all.
const SERVE_BASE = process.env.AGENTPLANE_SERVE_BASE || "http://agentplane-serve.agentplane.svc:7433";

// A hand actor serves exactly one session for its whole life, and atenet
// routes to it by actor name — so the first request's Host header IS our
// identity (`h-sess-…`). Latch it once; constant thereafter, race-free.
let ACTOR_NAME = "";
let SESSION_ID = "";
function latchIdentity(host) {
  if (ACTOR_NAME || !host) return;
  const name = host.split(":")[0].split(".")[0];
  if (/^h-sess-[a-z0-9]+$/.test(name)) {
    ACTOR_NAME = name;
    SESSION_ID = name.slice(2); // strip the `h-` role prefix
  }
}

// ---- execution journal + idempotency advisory (issue #14) -------------------
// A durable record of what the hand ACTUALLY executed (the brain's event log
// only records the model's intent). Mutating tools journal a start line and a
// completion line to the checkpointed workspace; an identical mutation within
// DUP_WINDOW_MS still executes but the result carries an advisory so the model
// can recognize a possible retry-after-lost-response. Pure tools skip all this.
const MUTATING = new Set(["bash", "write", "edit"]);
const JOURNAL = () => path.join(WORKDIR, ".hand-journal.jsonl");
const DUP_WINDOW_MS = 10 * 60 * 1000;
const recentMutations = new Map(); // argsHash → { ts, outcome }

// PLATFORM-ENFORCED idempotency: when the caller identifies the logical call
// (MCP _meta id / progress token), a re-arrival of the SAME id replays the
// stored result WITHOUT re-executing — exactly-once per model decision, no
// model cooperation needed. A NEW id with identical content is a new decision
// and executes (that case gets the advisory instead). The map lives in actor
// memory, so it survives suspend/resume with everything else.
const executedCalls = new Map(); // callKey → result
const EXECUTED_CAP = 200;
function callKeyOf(params) {
  const m = params._meta ?? {};
  const k = m["claude/toolUseId"] ?? m["claudecode/toolUseId"] ?? m.toolUseId ?? m.progressToken;
  return k == null ? null : String(k);
}
function rememberCall(key, result) {
  if (key == null) return;
  executedCalls.set(key, result);
  if (executedCalls.size > EXECUTED_CAP) {
    executedCalls.delete(executedCalls.keys().next().value); // Map iterates in insertion order — drops the oldest
  }
}

function argsHash(name, args) {
  return createHash("sha256").update(name + "\u0000" + JSON.stringify(args)).digest("hex").slice(0, 16);
}
function journalLine(obj) {
  const line = JSON.stringify(obj);
  // The journal lives in /workspace, which only the executor mounts now.
  // Fire-and-forget, as before: an audit convenience must never fail a tool.
  execFs("append", { path: ".hand-journal.jsonl", content: line + "\n" })
    .catch(() => { /* journal is best-effort */ });
  // Mirror to stdout: the workspace copy dies with the session, but actor
  // stdout lands in Cloud Logging (labeled ate.dev/actor_name) and OUTLIVES
  // it — that's the operator audit trail. Deliberate privacy trade: unlike
  // spans, the journal records the command summary; audit means seeing actions.
  console.log(`hand-journal ${line}`);
}
// Returns {ago, outcome} when an identical mutation completed recently.
function journalStart(name, args, callKey = null) {
  if (!MUTATING.has(name)) return null;
  const hash = argsHash(name, args);
  const summary = String(args.command ?? args.path ?? "").slice(0, 200);
  // `key` doubles as live telemetry for whether the SDK threads a call id —
  // absent key = exactly-once armed but dormant, advisory still covers.
  journalLine({ ts: new Date().toISOString(), ev: "start", tool: name, hash, key: callKey ?? undefined, summary, session: SESSION_ID || undefined });
  const prev = recentMutations.get(hash);
  if (prev && Date.now() - prev.ts < DUP_WINDOW_MS) {
    return { ago: Math.round((Date.now() - prev.ts) / 1000), outcome: prev.outcome };
  }
  return null;
}
function journalEnd(name, args, result, ms) {
  if (!MUTATING.has(name)) return;
  const hash = argsHash(name, args);
  const text = result.content?.[0]?.text ?? "";
  const outcome = result.isError ? "error" : (text.startsWith("exit ") ? text.split("\n", 1)[0] : "ok");
  journalLine({ ts: new Date().toISOString(), ev: "end", tool: name, hash, outcome, ms });
  recentMutations.set(hash, { ts: Date.now(), outcome });
}

// Resolve a caller-supplied path inside the workspace (absolute paths under
// /workspace are honored; relative ones resolve against it). Refuses escapes.

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

async function runOwnTool(name, args) {
  switch (name) {
    case "bash": {
        let r;
        try {
          r = await execRun(["sh", "-c", args.command], { cwd: ".", timeoutMs: 120000, env: declaredEnv() });
        } catch (e) {
          // Fail closed and say so. Falling back to running it here would
          // restore precisely the arrangement this replaced.
          return errResult(`executor unavailable: ${e.message}`);
        }
        const body = (r.stdout || "") + (r.stderr || "");
        return { content: [{ type: "text", text: `exit ${r.status}\n${body}` }], isError: r.status !== 0 };
    }
    case "write": {
      const w = await execFs("write", { path: args.path, content: args.content });
      return ok(`wrote ${w.path} (${w.bytes} bytes)`);
    }
    case "read":
      return ok((await execFs("read", { path: args.path })).text);
    case "list": {
      const { entries } = await execFs("list", { path: args.path ?? "." });
      const lines = entries.map((e) => `${e.dir ? "d" : "-"} ${e.name}`);
      return ok(lines.length ? lines.join("\n") : "(empty)");
    }
    case "edit": {
      const content = (await execFs("read", { path: args.path })).text;
      const { old_string, new_string, replace_all } = args;
      const count = content.split(old_string).length - 1;
      if (count === 0) return errResult(`old_string not found in ${args.path}`);
      if (count > 1 && !replace_all) return errResult(`old_string is not unique (${count} matches) — add surrounding context or set replace_all`);
      const out = replace_all ? content.split(old_string).join(new_string) : content.replace(old_string, new_string);
      const w = await execFs("write", { path: args.path, content: out });
      return ok(`edited ${w.path} (${count} replacement${count === 1 ? "" : "s"})`);
    }
    case "glob": {
      const { matches } = await execFs("glob", { path: args.path ?? ".", pattern: args.pattern });
      return ok(matches.length ? matches.join("\n") : "(no matches)");
    }
    case "grep": {
      const base = args.path ?? ".";
      const gargs = ["-rnE"];
      if (args.glob) gargs.push(`--include=${args.glob}`);
      gargs.push(args.pattern, ".");
      let r;
      try {
        r = await execRun(["grep", ...gargs], { cwd: base, timeoutMs: 60000, env: declaredEnv() });
      } catch (e) {
        return errResult(`executor unavailable: ${e.message}`);
      }
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
      if (SESSION_ID) {
        span.setAttribute("agentplane.session", SESSION_ID);
        span.setAttribute("agentplane.actor", ACTOR_NAME);
      }
      // Exactly-once: a re-arrival of an already-executed logical call replays
      // its stored result — the side effect can not happen twice.
      const callKey = callKeyOf(req.params);
      if (callKey != null && MUTATING.has(name) && executedCalls.has(callKey)) {
        span.setAttribute("hand.tool.replayed", true);
        journalLine({ ts: new Date().toISOString(), ev: "replay", tool: name, key: callKey, session: SESSION_ID || undefined });
        span.end();
        return executedCalls.get(callKey);
      }
      const dup = journalStart(name, args, callKey);
      const t0 = Date.now();
      try {
        let r;
        if (OWN_NAMES.has(name)) r = await runOwnTool(name, args);
        else if (resolveFederated(name)) r = await callFederated(name, args);
        else r = errResult(`unknown tool: ${name}`);
        span.setAttribute("hand.tool.is_error", !!r.isError);
        if (r.isError) span.setStatus({ code: SpanStatusCode.ERROR });
        journalEnd(name, args, r, Date.now() - t0);
        // Advisory for content-duplicates under a NEW call id (a fresh model
        // decision): still executes, but the model is told. Same-id replays
        // never reach here — they were answered from executedCalls above.
        if (dup && !r.isError && r.content?.[0]?.type === "text") {
          r.content[0].text += `\n\n[hand] note: an identical ${name} call completed ${dup.ago}s ago (${dup.outcome}). If this was a retry after a lost response, its side effect may now have happened twice — verify state before repeating mutations.`;
        }
        if (MUTATING.has(name)) rememberCall(callKey, r);
        return r;
      } catch (e) {
        span.recordException(e);
        span.setStatus({ code: SpanStatusCode.ERROR, message: e.message });
        journalEnd(name, args, { isError: true }, Date.now() - t0);
        const r = errResult(e.message);
        // Remember errors too: the call WAS attempted; state is unknown. A
        // same-id replay gets the recorded error — re-executing a mutation
        // with unknown state is exactly what exactly-once forbids. The model
        // retries with a NEW call id when it decides to.
        if (MUTATING.has(name)) rememberCall(callKey, r);
        return r;
      } finally {
        span.end();
      }
    });
  });
  return server;
}

// ---- admin (control) plane: serve injects upstreams + credentials here -------

// Grants verified recently, so a burst of admin calls at session setup does not
// become a burst of round trips. Bounded by the grant's own short life.
const verifiedGrants = new Map(); // token -> expiry ms

// Authorize an /admin call (#74).
//
// A session-scoped grant minted by serve is the ONLY way in. The hand cannot check the
// HMAC itself — that needs the signing key, and keeping that key out of a
// sandbox running model-written code is the entire point — so it asks serve,
// exactly as it already does for credentials.
//
// The returned session is compared against this hand's OWN identity, so a grant
// lifted out of one session's hand does not authorize another's.
//
// There is deliberately no static-token fallback and no open dev mode. The old
// HAND_ADMIN_TOKEN was one secret mounted into every hand, and it stayed usable
// even after the environment scrub because tools run as root in this container
// and could read it out of /proc/1/environ. Accepting it here would have kept
// that path open regardless of the grant work, so it is gone — and the token is
// no longer mounted into the hand at all.
//
// SERVE_BASE comes from the environment, never from the request — taking it
// from the caller would let an attacker point verification at a server of their
// own choosing and mint their own approval.
async function adminAuthed(req) {
  const h = req.headers["authorization"] || "";
  const tok = h.startsWith("Bearer ") ? h.slice(7) : "";
  if (!tok) return false; // no credential, no access — there is no open mode

  const cached = verifiedGrants.get(tok);
  if (cached && cached > Date.now()) return true;

  {
    try {
      const r = await fetch(`${SERVE_BASE}/v1/hand/admin-verify`, {
        headers: { Authorization: `Bearer ${tok}` },
        signal: AbortSignal.timeout(10000),
      });
      if (!r.ok) return false;
      const { session } = await r.json();
      // Bind the grant to this hand. SESSION_ID is latched from the Host header
      // atenet routed on, so it is this actor's own identity, not the caller's
      // claim about it.
      if (!session || (SESSION_ID && session !== SESSION_ID)) {
        console.error(`admin: grant is for ${session}, this hand is ${SESSION_ID} — refused`);
        return false;
      }
      verifiedGrants.set(tok, Date.now() + 60_000);
      return true;
    } catch (e) {
      // Fail CLOSED: an unreachable serve must not mean "allow".
      console.error(`admin: cannot verify grant (${e.message}) — refused`);
      return false;
    }
  }

  return false;
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
  const gitCount = await setGitCredentials(body.gitCredentials || []);
  res.writeHead(200, { "content-type": "application/json" });
  res.end(JSON.stringify({ ok: true, registered, gitCredentials: gitCount }));
}

// POST /admin/repositories — point git at the proxy, then clone what the agent
// declared. Called by serve at session setup, BEFORE the first message, so the
// workspace is ready when the model starts (#49).
//
// Body: { proxyBase, grant, credential, repositories: [{url,mountPath,checkout}] }
async function handleAdminRepositories(req, res) {
  const body = await readJson(req);
  const proxied = await configureGitProxy({
    proxyBase: body.proxyBase, grant: body.grant, credential: body.credential,
  });
  const cloned = await cloneRepositories(body.repositories || []);
  res.writeHead(200, { "content-type": "application/json" });
  res.end(JSON.stringify({ ok: true, proxied, cloned }));
}

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");

  if (url.pathname === "/healthz") {
    res.writeHead(200, { "content-type": "application/json" });
    return res.end(JSON.stringify({ ok: true, workdir: WORKDIR, upstreams: upstreamStatus() }));
  }

  if (url.pathname.startsWith("/admin/")) {
    if (!(await adminAuthed(req))) { res.writeHead(401); return res.end("unauthorized"); }
    try {
      if (url.pathname === "/admin/upstreams" && req.method === "POST") return await handleAdminUpstreams(req, res);
      if (url.pathname === "/admin/repositories" && req.method === "POST") return await handleAdminRepositories(req, res);
      if (url.pathname === "/admin/identity" && req.method === "POST") {
        // serve pushes who we are at session create — the actor itself has no
        // ambient identity (no env, no hostname, Host doesn't survive routing).
        const body = await readJson(req);
        if (typeof body.session === "string" && /^sess-[a-z0-9]+$/.test(body.session)) {
          SESSION_ID = body.session;
          ACTOR_NAME = typeof body.actor === "string" ? body.actor : `h-${body.session}`;
        }
        res.writeHead(200, { "content-type": "application/json" });
        return res.end(JSON.stringify({ ok: true, session: SESSION_ID }));
      }
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
  latchIdentity(req.headers.host);

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
