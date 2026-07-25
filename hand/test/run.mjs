// End-to-end gateway proof: start the hand + a dummy upstream, register the
// upstream via /admin/upstreams, then act as the BRAIN (an MCP client to the
// hand) and verify the hand exposes BOTH its own tools and the proxied upstream
// tool, and that calling the proxied tool round-trips to the upstream.
import { spawn } from "node:child_process";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const HAND = 9090, UP = 9091, ADMIN = "test-admin-token";
const procs = [];
function start(file, env) {
  const p = spawn("node", [file], { env: { ...process.env, ...env }, stdio: "inherit" });
  procs.push(p); return p;
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
async function waitHealth(port) {
  for (let i = 0; i < 50; i++) {
    try { const r = await fetch(`http://localhost:${port}/healthz`); if (r.ok) return; } catch {}
    await sleep(200);
  }
  throw new Error(`port ${port} never became healthy`);
}
function assert(cond, msg) { if (!cond) { console.error("FAIL:", msg); cleanup(1); } else console.log("ok:", msg); }
function cleanup(code) { for (const p of procs) { try { p.kill("SIGKILL"); } catch {} } process.exit(code); }

start("src/server.mjs", { PORT: String(HAND), HAND_WORKDIR: "/tmp/ws", HAND_ADMIN_TOKEN: ADMIN });
start("test/upstream.mjs", { PORT: String(UP) });
await waitHealth(HAND);
await waitHealth(UP);

// 1) Register the upstream through the admin plane (as serve would).
const reg = await fetch(`http://localhost:${HAND}/admin/upstreams`, {
  method: "POST",
  headers: { "content-type": "application/json", authorization: `Bearer ${ADMIN}` },
  body: JSON.stringify({ upstreams: [{ name: "up", url: `http://localhost:${UP}/mcp` }] }),
});
const regBody = await reg.json();
assert(reg.status === 200 && regBody.registered?.[0]?.tools?.includes("echo"),
  `registered upstream 'up' with tools: ${JSON.stringify(regBody.registered)}`);

// 2) Admin auth is enforced.
const noauth = await fetch(`http://localhost:${HAND}/admin/status`);
assert(noauth.status === 401, "admin rejects missing bearer");

// 3) Connect to the hand as the brain does and list tools.
const client = new Client({ name: "brain-sim", version: "0.1.0" });
await client.connect(new StreamableHTTPClientTransport(new URL(`http://localhost:${HAND}/mcp`)));
const { tools } = await client.listTools();
const names = tools.map((t) => t.name);
assert(names.includes("bash"), `hand exposes own tool 'bash' (saw: ${names.join(", ")})`);
assert(names.includes("up__echo"), "hand exposes PROXIED tool 'up__echo'");

// 4) Call the proxied tool — must round-trip to the upstream.
const echo = await client.callTool({ name: "up__echo", arguments: { text: "hello-gateway" } });
assert(echo.content?.[0]?.text?.includes("UPSTREAM echoes: hello-gateway"),
  `proxied call round-tripped: ${JSON.stringify(echo.content)}`);

// 5) Own tool still works.
const bash = await client.callTool({ name: "bash", arguments: { command: "echo local-exec" } });
assert(bash.content?.[0]?.text?.includes("local-exec"), "own bash tool executes on the hand");

console.log("\nALL GATEWAY TESTS PASSED");
cleanup(0);
