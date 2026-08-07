// agentplane executor — where model-written commands actually run.
//
// A second container inside the SAME actor as the hand gateway (#74). gVisor
// already isolates the actor from the host and from every other session; what
// it does not do is isolate the hand's own credentials from the code the hand
// runs, because both used to live in one container. This is that missing
// boundary:
//
//   actor h-<sid>
//   ├── gateway : MCP server, credentials, /admin — mounts /workspace
//   └── exec    : this process — mounts /workspace, holds NO credential
//
// Substrate gives each container its own rootfs and PID namespace while sharing
// the network namespace, so:
//
//   * `ls /app` here lists the executor. The gateway's source is not hidden, it
//     is NOT PRESENT — a different filesystem entirely.
//   * /proc/1 is this process, not the gateway's. There is no environ to read.
//   * the gateway reaches us on loopback; nothing outside the actor can, because
//     atenet routes only to the actor's :80, which is the gateway.
//
// Deliberately tiny and dependency-free: this is the process running untrusted
// input, so its own attack surface should be about as small as a program can be.

import http from "node:http";
import path from "node:path";
import { spawnSync } from "node:child_process";
import {
  appendFileSync, mkdirSync, readFileSync, readdirSync, statSync, writeFileSync, globSync,
} from "node:fs";

const PORT = Number(process.env.EXEC_PORT || 8081);
// Loopback only. Containers in one actor share a network namespace, so the
// gateway can reach this — and binding the wildcard would additionally expose
// it on the actor's pod IP, where atenet could route to it.
const HOST = process.env.EXEC_HOST || "127.0.0.1";
const WORKDIR = process.env.HAND_WORKDIR || "/workspace";
const MAX_OUTPUT = 10 * 1024 * 1024;

mkdirSync(WORKDIR, { recursive: true });

function readJson(req) {
  return new Promise((resolve, reject) => {
    const chunks = [];
    let size = 0;
    req.on("data", (c) => {
      size += c.length;
      // A command body is small; refusing early keeps a runaway client from
      // growing this process's memory.
      if (size > 4 << 20) { reject(new Error("body too large")); req.destroy(); return; }
      chunks.push(c);
    });
    req.on("end", () => {
      try { resolve(JSON.parse(Buffer.concat(chunks).toString("utf8") || "{}")); }
      catch (e) { reject(e); }
    });
    req.on("error", reject);
  });
}

// Path validation lives HERE, with the only process that has the workspace
// mounted. Validating in the gateway and trusting the result would mean the
// component without the filesystem deciding what is inside it.
function resolveInWorkdir(p) {
  const abs = path.resolve(WORKDIR, p || ".");
  if (abs !== WORKDIR && !abs.startsWith(WORKDIR + path.sep)) {
    throw new Error(`path escapes the workspace: ${p}`);
  }
  return abs;
}

// Filesystem operations, because the gateway no longer mounts /workspace.
//
// gVisor refuses to mount one DurableDir into two containers — the second gets
// "repeated submounts are not supported with overlay optimizations" — so the
// executor owns the workspace outright. That is the stronger arrangement
// anyway: the process holding the session's credentials never touches the
// filesystem the model can write to.
function fsOp(op, a) {
  switch (op) {
    case "read":
      return { text: readFileSync(resolveInWorkdir(a.path), "utf8") };
    case "write": {
      const abs = resolveInWorkdir(a.path);
      mkdirSync(path.dirname(abs), { recursive: true });
      writeFileSync(abs, a.content ?? "");
      return { path: abs, bytes: Buffer.byteLength(a.content ?? "") };
    }
    case "append": {
      const abs = resolveInWorkdir(a.path);
      mkdirSync(path.dirname(abs), { recursive: true });
      appendFileSync(abs, a.content ?? "");
      return { path: abs };
    }
    case "list": {
      const abs = resolveInWorkdir(a.path ?? ".");
      return {
        entries: readdirSync(abs).map((n) => ({
          name: n,
          dir: statSync(path.join(abs, n)).isDirectory(),
        })),
      };
    }
    case "glob": {
      const base = resolveInWorkdir(a.path ?? ".");
      return { matches: [...globSync(a.pattern, { cwd: base })] };
    }
    default:
      throw new Error(`unknown fs op: ${op}`);
  }
}

// Run one command. Returns the same shape the gateway used to build inline, so
// the tool result the model sees is unchanged by the split.
function run(argv, { cwd, timeoutMs, env }) {
  const [cmd, ...args] = argv;
  // Resolve cwd inside the workspace. The gateway now sends a RELATIVE path
  // (it has no filesystem to resolve against), and an unresolved "." would run
  // in the executor's own /app rather than the session's workspace.
  const runCwd = resolveInWorkdir(cwd || ".");
  const r = spawnSync(cmd, args, {
    cwd: runCwd,
    encoding: "utf8",
    timeout: timeoutMs || 120000,
    maxBuffer: MAX_OUTPUT,
    // The executor's own environment is already free of platform secrets —
    // that is the point of the split — but pass it explicitly so adding one
    // here later cannot silently reach a command.
    env: {
      PATH: process.env.PATH,
      HOME: process.env.HOME,
      PWD: runCwd,
      ...(process.env.VIRTUAL_ENV ? { VIRTUAL_ENV: process.env.VIRTUAL_ENV } : {}),
      ...(process.env.GIT_TERMINAL_PROMPT ? { GIT_TERMINAL_PROMPT: process.env.GIT_TERMINAL_PROMPT } : {}),
      // Credentials the agent declared, sent by the gateway per call. They are
      // never stored here, so nothing persists between commands and nothing is
      // visible in this process's own environment.
      ...(env && typeof env === "object" ? env : {}),
    },
  });
  if (r.error) return { ok: false, error: r.error.message };
  return { ok: true, status: r.status, stdout: r.stdout || "", stderr: r.stderr || "" };
}

const server = http.createServer(async (req, res) => {
  const send = (code, obj) => {
    res.writeHead(code, { "content-type": "application/json" });
    res.end(JSON.stringify(obj));
  };

  if (req.method === "GET" && req.url === "/healthz") return send(200, { ok: true });

  if (req.method === "POST" && req.url === "/run") {
    let body;
    try { body = await readJson(req); } catch { return send(400, { error: "invalid json" }); }
    if (!Array.isArray(body.argv) || body.argv.length === 0 || typeof body.argv[0] !== "string") {
      return send(400, { error: "argv must be a non-empty string array" });
    }
    // argv, not a shell string: the caller decides whether a shell is involved
    // (`["sh","-c",cmd]`) rather than this process pasting input into one.
    return send(200, run(body.argv, { cwd: body.cwd, timeoutMs: body.timeoutMs, env: body.env }));
  }

  if (req.method === "POST" && req.url === "/fs") {
    let body;
    try { body = await readJson(req); } catch { return send(400, { error: "invalid json" }); }
    try { return send(200, { ok: true, ...fsOp(body.op, body) }); }
    catch (e) { return send(200, { ok: false, error: e.message }); }
  }

  send(404, { error: "not found" });
});

server.listen(PORT, HOST, () => {
  console.log(JSON.stringify({
    msg: "executor listening", host: HOST, port: PORT, workdir: WORKDIR,
    uid: typeof process.getuid === "function" ? process.getuid() : null,
  }));
});
