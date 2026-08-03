// agentplane brain server — one durable agent mind per actor, exposed via the
// agentplane session-events dialect v0:
//
//   POST /v1/sessions/{id}/events          send events (user.message)
//   GET  /v1/sessions/{id}/events          list persisted events (the turn log)
//   GET  /v1/sessions/{id}/events/stream   SSE: replay buffered + live events
//   GET  /healthz                          liveness (dispatcher probe)
//
// This file is deliberately thin: it does HTTP and nothing else. The durable
// engine is ../runtime.mjs; the per-vendor logic is a Harness (harness/). See
// docs/brain-server.md for the durability model and docs/harness-interface.md
// for the Harness contract.

import http from "node:http";
import { createRuntime } from "./runtime.mjs";
import { actorIdentity } from "./identity.mjs";
import { selectHarness } from "./harness/index.mjs";
import { contextFromRequest, initOtel } from "./otel.mjs";

const PORT = Number(process.env.PORT || 8080);
const WORKDIR = process.env.BRAIN_WORKDIR || "/workspace";

function loadSpec() {
  try {
    return process.env.AGENTPLANE_SPEC ? JSON.parse(process.env.AGENTPLANE_SPEC) : {};
  } catch (e) {
    console.error("invalid AGENTPLANE_SPEC:", e.message);
    return {};
  }
}

function textFromContent(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content.filter((b) => b.type === "text").map((b) => b.text).join("");
}

const spec = loadSpec();
const harness = await selectHarness(process.env.AGENTPLANE_HARNESS);
const rt = createRuntime({ workdir: WORKDIR, spec, harness, identity: actorIdentity });

function readBody(req) {
  return new Promise((resolve, reject) => {
    let data = "";
    req.on("data", (c) => { data += c; if (data.length > 1 << 20) req.destroy(); });
    req.on("end", () => resolve(data));
    req.on("error", reject);
  });
}

initOtel();

const server = http.createServer(async (req, res) => {
  const url = new URL(req.url, "http://x");

  if (url.pathname === "/healthz") {
    res.writeHead(200, { "content-type": "application/json" });
    return res.end(JSON.stringify(rt.health()));
  }

  // BYO-key: store the ephemeral per-session vendor key in memory. Kept out of
  // the events route on purpose — the key is never an event, never logged.
  if (url.pathname.match(/^\/v1\/sessions\/[^/]+\/key$/) && req.method === "POST") {
    let body;
    try { body = JSON.parse((await readBody(req)) || "{}"); }
    catch { res.writeHead(400); return res.end(JSON.stringify({ error: "invalid json" })); }
    rt.setApiKey(typeof body.apiKey === "string" ? body.apiKey : null);
    res.writeHead(204);
    return res.end();
  }

  const m = url.pathname.match(/^\/v1\/sessions\/([^/]+)\/events(\/stream)?$/);
  if (!m) { res.writeHead(404); return res.end("not found"); }

  // send events (user.message)
  if (!m[2] && req.method === "POST") {
    let body;
    try { body = JSON.parse((await readBody(req)) || "{}"); }
    catch { res.writeHead(400); return res.end(JSON.stringify({ error: "invalid json" })); }
    const events = body.events || (body.message ? [{ type: "user.message", content: [{ type: "text", text: body.message }] }] : []);
    const accepted = [];
    for (const ev of events) {
      if (ev.type !== "user.message") continue; // v0: only user messages
      const text = textFromContent(ev.content ?? ev.text ?? "");
      if (!text) continue;
      // Hand the caller's trace context to the runtime so the turn's span is a
      // child of serve's request rather than the root of a disconnected trace.
      rt.acceptUserMessage(text, contextFromRequest(req));
      accepted.push(ev.type);
    }
    res.writeHead(accepted.length ? 202 : 400, { "content-type": "application/json" });
    return res.end(JSON.stringify({ accepted }));
  }

  // list events (the persisted turn log)
  if (!m[2] && req.method === "GET") {
    res.writeHead(200, { "content-type": "application/json" });
    return res.end(JSON.stringify({ events: rt.listEvents(url.searchParams.get("since")) }));
  }

  // SSE stream: cursor-aware replay (?since= or Last-Event-ID) then live.
  if (m[2] && req.method === "GET") {
    const since = url.searchParams.get("since") || req.headers["last-event-id"] || "";
    res.writeHead(200, {
      "content-type": "text/event-stream",
      "cache-control": "no-cache",
      connection: "keep-alive",
      "x-accel-buffering": "no",
    });
    rt.subscribeSSE(res, since);
    return;
  }

  res.writeHead(405);
  res.end();
});

server.listen(PORT, () =>
  console.log(`brain server: session=${actorIdentity()} harness=${harness.name} port=${PORT} workdir=${WORKDIR} (harness starts on first message)`));
