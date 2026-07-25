// Proves the HAND can PULL a credential and apply it — the "can the hand get a
// secret?" test, minus real Secret Manager. A stub stands in for serve's
// /v1/hand/credentials/{name}: it checks the grant bearer and returns a payload.
// We push a grant to the hand's /admin/grant, then verify via the hand's own
// bash tool that (a) an env credential is set and (b) git credentials were
// written to $HOME/.git-credentials.
import { spawn } from "node:child_process";
import http from "node:http";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const HAND = 9092, STUB = 9093, ADMIN = "test-admin", GRANT = "grant.sig";
const procs = [];
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
function assert(c, m) { if (!c) { console.error("FAIL:", m); cleanup(1); } else console.log("ok:", m); }
function cleanup(code) { for (const p of procs) { try { p.kill("SIGKILL"); } catch {} } try { stub.close(); } catch {} process.exit(code); }

// Stub serve: returns a git cred and an env cred; enforces the grant bearer.
const VALUES = {
  "gh-token": { type: "git", value: "ghp_SECRET123", host: "github.com", username: "x-access-token" },
  "my-env":   { type: "env", value: "env-secret-xyz", varName: "MY_SECRET" },
};
const stub = http.createServer((req, res) => {
  const m = req.url.match(/^\/v1\/hand\/credentials\/(.+)$/);
  if (!m) { res.writeHead(404); return res.end(); }
  if (req.headers.authorization !== `Bearer ${GRANT}`) { res.writeHead(401); return res.end("bad grant"); }
  const v = VALUES[decodeURIComponent(m[1])];
  if (!v) { res.writeHead(404); return res.end(); }
  res.writeHead(200, { "content-type": "application/json" });
  res.end(JSON.stringify(v));
});
stub.listen(STUB);

const hand = spawn("node", ["src/server.mjs"], {
  env: { ...process.env, PORT: String(HAND), HAND_WORKDIR: "/tmp/ws2", HAND_ADMIN_TOKEN: ADMIN, HOME: "/tmp/hh" },
  stdio: "inherit",
});
procs.push(hand);
for (let i = 0; i < 50; i++) { try { if ((await fetch(`http://localhost:${HAND}/healthz`)).ok) break; } catch {} await sleep(200); }
spawn("mkdir", ["-p", "/tmp/hh"]);

// 1) Push the grant — the hand pulls both credentials from the stub.
const g = await fetch(`http://localhost:${HAND}/admin/grant`, {
  method: "POST",
  headers: { "content-type": "application/json", authorization: `Bearer ${ADMIN}` },
  body: JSON.stringify({ serveBase: `http://localhost:${STUB}`, grant: GRANT, credentials: ["gh-token", "my-env"] }),
});
const gb = await g.json();
assert(g.status === 200, `grant accepted: ${JSON.stringify(gb.applied)}`);
assert(gb.applied.find((a) => a.name === "gh-token" && a.ok && a.type === "git"), "git credential applied");
assert(gb.applied.find((a) => a.name === "my-env" && a.ok && a.type === "env"), "env credential applied");

// 2) Verify through the hand's own bash tool (as the brain would see it).
const client = new Client({ name: "verify", version: "0.1.0" });
await client.connect(new StreamableHTTPClientTransport(new URL(`http://localhost:${HAND}/mcp`)));

const envr = await client.callTool({ name: "bash", arguments: { command: "echo GOT=$MY_SECRET" } });
assert(envr.content?.[0]?.text?.includes("GOT=env-secret-xyz"), "env secret visible to bash on the hand");

const gitr = await client.callTool({ name: "bash", arguments: { command: "cat $HOME/.git-credentials" } });
assert(gitr.content?.[0]?.text?.includes("ghp_SECRET123") && gitr.content[0].text.includes("github.com"),
  "git credential written to $HOME/.git-credentials");

const helper = await client.callTool({ name: "bash", arguments: { command: "git config --global credential.helper" } });
assert(helper.content?.[0]?.text?.includes("store"), "git credential.helper=store configured");

// 3) Wrong grant is rejected (the hand's pull would 401).
const bad = await fetch(`http://localhost:${HAND}/admin/grant`, {
  method: "POST",
  headers: { "content-type": "application/json", authorization: `Bearer ${ADMIN}` },
  body: JSON.stringify({ serveBase: `http://localhost:${STUB}`, grant: "WRONG", credentials: ["gh-token"] }),
});
const badb = await bad.json();
assert(badb.applied.every((a) => !a.ok), "wrong grant → pull rejected (no credential applied)");

console.log("\nHAND PULL TESTS PASSED");
cleanup(0);
