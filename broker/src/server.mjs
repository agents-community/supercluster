// agentplane broker — the mediator between brain and hand.
//
// The brain connects here (its MCP server). The broker owns the tool set and
// runs each call in the hand actor via the control plane's ExecActor (`runsc
// exec`), applying credentials first (the harness gates approvals). The runtime lives HERE,
// outside gVisor: the hand actor holds only its workspace + toolchain, none of
// our code. See README.md.

import http from "node:http";
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import {
  ListToolsRequestSchema,
  CallToolRequestSchema,
} from "@modelcontextprotocol/sdk/types.js";
import { trace } from "@opentelemetry/api";
import { initOtel, contextFromHeaders, withSpan } from "./otel.mjs";
import { execActor } from "./serve-exec.mjs";
import { TOOL_DEFS, TOOL_NAMES, runTool } from "./tools.mjs";
import { parseUpstreams, federate } from "./federation.mjs";

initOtel(); // start tracing before anything runs (no-op if OTEL endpoint unset)

const PORT = Number(process.env.PORT || 8088);

// buildServer returns the MCP server the brain talks to. It advertises the
// broker's tool set and runs each call in handActor via ExecActor.
//
// The broker does NOT enforce allow/deny/ask — that is the harness's job: the
// claude-code / pi harness applies the agent's policy and gates `ask` tools
// (emitting the approval event) BEFORE the call ever reaches the broker. So the
// broker is a pure executor; adding a second policy here would be a redundant,
// drifting copy of the harness's.
async function buildServer(handActor, upstreams, reqCtx) {
  const server = new Server(
    { name: "agentplane-broker", version: "0.1.0" },
    { capabilities: { tools: {} } },
  );
  const span = trace.getSpan(reqCtx);
  const exec = (argv, opts = {}) => execActor(handActor, argv, opts);

  // Federate the agent's MCP upstreams (if any). The broker is the only MCP
  // client to them; their tools are advertised alongside the hand's.
  const fed = await federate(upstreams);

  server.setRequestHandler(ListToolsRequestSchema, async () => ({ tools: [...TOOL_DEFS, ...fed.defs] }));

  server.setRequestHandler(CallToolRequestSchema, async (req) => {
    const { name, arguments: args = {} } = req.params;
    try { span?.setAttribute("agentplane.tool", name); } catch { /* best-effort */ }
    console.log(`call ${name} (hand ${handActor})`);
    // Federated (upstream) tool → forward to its MCP server.
    if (fed.has(name)) {
      try {
        return await fed.call(name, args);
      } catch (e) {
        return { isError: true, content: [{ type: "text", text: `[broker] ${name} (upstream) failed: ${e.message}` }] };
      }
    }
    // Otherwise a hand tool → run it in the actor.
    if (!TOOL_NAMES.has(name)) {
      return { isError: true, content: [{ type: "text", text: `[broker] unknown tool: ${name}` }] };
    }
    try {
      return await runTool(name, args, exec);
    } catch (e) {
      return { isError: true, content: [{ type: "text", text: `[broker] ${name} failed: ${e.message}` }] };
    }
  });

  return server;
}

// handActorFrom: the actor to exec in. serve points the brain here with
// ?hand=<hand actor MCP url>; the actor name is that host's first label
// (h-<session>). A bare actor name is also accepted.
function handActorFrom(req, url) {
  const raw = req.headers["x-ate-hand-url"] || url.searchParams.get("hand") || "";
  if (!raw) return null;
  try {
    return new URL(raw).host.split(".")[0] || null;
  } catch {
    return raw.split(".")[0] || null; // already a bare actor name
  }
}

const httpServer = http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");
  if (url.pathname === "/healthz") {
    res.writeHead(200, { "content-type": "application/json" });
    return res.end(JSON.stringify({ ok: true }));
  }
  if (url.pathname !== "/mcp") {
    res.writeHead(404);
    return res.end("not found");
  }

  const handActor = handActorFrom(req, url);
  if (!handActor) {
    res.writeHead(400, { "content-type": "application/json" });
    return res.end(JSON.stringify({ error: "no hand target (x-ate-hand-url header or ?hand=)" }));
  }

  const upstreams = parseUpstreams(req.headers["x-agentplane-mcp"]);
  console.log(`mcp request for hand ${handActor}`);
  const parentCtx = contextFromHeaders(req.headers);
  await withSpan("broker.mcp", parentCtx, { "agentplane.hand": handActor }, async (reqCtx) => {
    const server = await buildServer(handActor, upstreams, reqCtx);
    const transport = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
    res.on("close", () => { transport.close?.(); });
    await server.connect(transport);
    await transport.handleRequest(req, res);
  });
});

httpServer.listen(PORT, () => console.log(`broker: MCP mediator on :${PORT}`));
