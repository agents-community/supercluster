// Verify the hand now mirrors the full coding toolset: bash/write/read/list +
// edit/glob/grep. Drives them through the MCP client, as the brain would.
import { spawn } from "node:child_process";
import { Client } from "@modelcontextprotocol/sdk/client/index.js";
import { StreamableHTTPClientTransport } from "@modelcontextprotocol/sdk/client/streamableHttp.js";

const HAND = 9094;
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
let hand;
function assert(c, m) { if (!c) { console.error("FAIL:", m); cleanup(1); } else console.log("ok:", m); }
function cleanup(code) { try { hand.kill("SIGKILL"); } catch {} process.exit(code); }
const text = (r) => r.content?.[0]?.text || "";

hand = spawn("node", ["src/server.mjs"], { env: { ...process.env, PORT: String(HAND), HAND_WORKDIR: "/tmp/wst" }, stdio: "inherit" });
for (let i = 0; i < 50; i++) { try { if ((await fetch(`http://localhost:${HAND}/healthz`)).ok) break; } catch {} await sleep(200); }

const c = new Client({ name: "tools-test", version: "0.1.0" });
await c.connect(new StreamableHTTPClientTransport(new URL(`http://localhost:${HAND}/mcp`)));

const names = (await c.listTools()).tools.map((t) => t.name).sort();
assert(["bash", "edit", "glob", "grep", "list", "read", "write"].every((n) => names.includes(n)),
  `hand exposes full toolset: ${names.join(", ")}`);

await c.callTool({ name: "write", arguments: { path: "src/app.js", content: "const greet = 'hello';\nconsole.log(greet);\n" } });
assert(text(await c.callTool({ name: "read", arguments: { path: "src/app.js" } })).includes("greet"), "write+read round-trip");

await c.callTool({ name: "edit", arguments: { path: "src/app.js", old_string: "'hello'", new_string: "'world'" } });
assert(text(await c.callTool({ name: "read", arguments: { path: "src/app.js" } })).includes("'world'"), "edit replaced the string");

const dup = await c.callTool({ name: "edit", arguments: { path: "src/app.js", old_string: "greet", new_string: "g" } });
assert(dup.isError && text(dup).includes("not unique"), "edit refuses a non-unique old_string without replace_all");

assert(text(await c.callTool({ name: "glob", arguments: { pattern: "**/*.js" } })).includes("src/app.js"), "glob **/*.js finds the file");
assert(text(await c.callTool({ name: "grep", arguments: { pattern: "console\\.log", glob: "*.js" } })).includes("app.js"), "grep finds the pattern with file:line");

console.log("\nTOOLSET TESTS PASSED");
cleanup(0);
