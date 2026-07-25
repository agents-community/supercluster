// A minimal upstream MCP server standing in for "the user's own MCP server"
// (e.g. a git-MCP). Exposes one tool `echo` so the gateway test can prove a
// call to the hand proxies through to here. Streamable HTTP, stateless.
import http from "node:http";
import { Server } from "@modelcontextprotocol/sdk/server/index.js";
import { StreamableHTTPServerTransport } from "@modelcontextprotocol/sdk/server/streamableHttp.js";
import { CallToolRequestSchema, ListToolsRequestSchema } from "@modelcontextprotocol/sdk/types.js";

const PORT = Number(process.env.PORT || 9091);

function build() {
  const s = new Server({ name: "upstream", version: "0.1.0" }, { capabilities: { tools: {} } });
  s.setRequestHandler(ListToolsRequestSchema, async () => ({
    tools: [{
      name: "echo",
      description: "echo the text back",
      inputSchema: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
    }],
  }));
  s.setRequestHandler(CallToolRequestSchema, async (req) => ({
    content: [{ type: "text", text: `UPSTREAM echoes: ${req.params.arguments?.text ?? ""}` }],
  }));
  return s;
}

http.createServer(async (req, res) => {
  if (req.url === "/healthz") { res.writeHead(200); return res.end("ok"); }
  const mcp = build();
  const t = new StreamableHTTPServerTransport({ sessionIdGenerator: undefined });
  res.on("close", () => { t.close(); mcp.close(); });
  await mcp.connect(t);
  if (req.method === "POST") {
    const chunks = []; for await (const c of req) chunks.push(c);
    await t.handleRequest(req, res, JSON.parse(Buffer.concat(chunks).toString() || "{}"));
  } else { await t.handleRequest(req, res); }
}).listen(PORT, () => console.log(`upstream MCP on :${PORT}`));
