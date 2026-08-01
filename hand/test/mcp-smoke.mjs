// Integration smoke test for the hand's MCP gateway. Boots the real server
// on a throwaway port + workdir, then drives it over streamable HTTP exactly
// like the brain does: initialize -> tools/list -> tools/call.
//
//   node test/mcp-smoke.mjs
//
// Exits non-zero on the first failed assertion. No cluster, no credentials.
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const HERE = dirname(fileURLToPath(import.meta.url));
const PORT = 18823;
const WORKDIR = mkdtempSync(join(tmpdir(), "hand-smoke-"));

let failures = 0;
function check(name, ok, detail = "") {
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${ok ? "" : `  ${detail}`}`);
  if (!ok) failures++;
}

// -- boot the real server ---------------------------------------------------
const server = spawn("node", [join(HERE, "..", "src", "server.mjs")], {
  env: { ...process.env, PORT: String(PORT), HAND_WORKDIR: WORKDIR, HAND_ADMIN_TOKEN: "test-admin" },
  stdio: ["ignore", "pipe", "pipe"],
});
server.stderr.on("data", (d) => process.stderr.write(`[hand] ${d}`));

async function waitReady() {
  for (let i = 0; i < 50; i++) {
    try {
      // any HTTP answer means the listener is up (GET /mcp may 4xx; that's fine)
      await fetch(`http://127.0.0.1:${PORT}/mcp`);
      return;
    } catch {
      await new Promise((r) => setTimeout(r, 200));
    }
  }
  throw new Error("hand never came up");
}

try {
  await waitReady();

  const client = new Client({ name: "mcp-smoke", version: "0.0.0" });
  await client.connect(new StreamableHTTPClientTransport(new URL(`http://127.0.0.1:${PORT}/mcp`)));

  // -- tools/list: the hand's own toolbelt ----------------------------------
  const { tools } = await client.listTools();
  const names = tools.map((t) => t.name).sort();
  const expected = ["bash", "edit", "glob", "grep", "list", "read", "write"];
  check(
    "tools/list exposes the 7 own tools",
    expected.every((n) => names.includes(n)),
    `got: ${names.join(",")}`
  );
  const bash = tools.find((t) => t.name === "bash");
  check("bash tool ships a JSON schema", !!bash?.inputSchema?.properties?.command);

  // -- tools/call: write + read + bash round-trip through the workdir -------
  const w = await client.callTool({ name: "write", arguments: { path: "hello.txt", content: "hand smoke\n" } });
  check("write succeeds", !w.isError, JSON.stringify(w.content));

  const r = await client.callTool({ name: "read", arguments: { path: "hello.txt" } });
  check("read returns what write wrote", (r.content?.[0]?.text ?? "").includes("hand smoke"));

  const b = await client.callTool({ name: "bash", arguments: { command: "wc -c < hello.txt" } });
  const btext = b.content?.[0]?.text ?? "";
  check("bash runs in the workdir", btext.startsWith("exit 0") && btext.includes("11"), btext);

  // -- confinement: bash must run under WORKDIR, not the repo ---------------
  const p = await client.callTool({ name: "bash", arguments: { command: "pwd" } });
  check("bash cwd is the sandbox workdir", (p.content?.[0]?.text ?? "").includes(WORKDIR));

  // -- admin surface is actually gated --------------------------------------
  const noAuth = await fetch(`http://127.0.0.1:${PORT}/admin/upstreams`, { method: "POST", body: "{}" });
  check("admin rejects missing bearer", noAuth.status === 401 || noAuth.status === 403, `status ${noAuth.status}`);

  // -- identity push (issue #2: spans need agentplane.session) --------------
  const idPush = await fetch(`http://127.0.0.1:${PORT}/admin/identity`, {
    method: "POST",
    headers: { Authorization: "Bearer test-admin", "Content-Type": "application/json" },
    body: JSON.stringify({ session: "sess-smoketest1", actor: "h-sess-smoketest1" }),
  });
  const idBody = await idPush.json().catch(() => ({}));
  check("admin identity push accepted", idPush.status === 200 && idBody.session === "sess-smoketest1", JSON.stringify(idBody));
  const badPush = await fetch(`http://127.0.0.1:${PORT}/admin/identity`, {
    method: "POST",
    headers: { Authorization: "Bearer test-admin", "Content-Type": "application/json" },
    body: JSON.stringify({ session: "../etc/passwd" }),
  });
  const badBody = await badPush.json().catch(() => ({}));
  check("identity push rejects malformed session", badBody.session === "sess-smoketest1", JSON.stringify(badBody));

  await client.close();
} catch (err) {
  check("smoke run completed", false, String(err));
} finally {
  server.kill("SIGKILL");
  rmSync(WORKDIR, { recursive: true, force: true });
}

console.log(failures === 0 ? "\nhand mcp-smoke: ALL PASS" : `\nhand mcp-smoke: ${failures} FAILURE(S)`);
process.exit(failures === 0 ? 0 : 1);
