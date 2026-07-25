#!/usr/bin/env node
// andromeda — a beautiful terminal for durable agent minds.
//
//   npx @agentplane/andromeda --agent buddy       new session, start chatting
//   npx @agentplane/andromeda --session sess-…    re-attach (history replays)
//   npx @agentplane/andromeda                     pick from live sessions/agents
//
// Connection (flags beat env):
//   --url   / ANDROMEDA_URL     control-plane URL   (default http://localhost:7433)
//   --token / ANDROMEDA_TOKEN   bearer token

import React from "react";
import { render, Box, Text } from "ink";
import { Client } from "./api.mjs";
import { App, historyLines } from "./app.mjs";

const h = React.createElement;

function arg(name) {
  const i = process.argv.indexOf(`--${name}`);
  return i >= 0 ? process.argv[i + 1] : undefined;
}

if (process.argv.includes("--help") || process.argv.includes("-h")) {
  console.log(`andromeda — a terminal for durable agent minds

usage:
  andromeda --agent <name>       mint a new session and attach
  andromeda --session <sess-…>   re-attach to an existing mind
  andromeda                      list agents & sessions, then pick

connection:
  --url <url>       or ANDROMEDA_URL    (default http://localhost:7433)
  --token <token>   or ANDROMEDA_TOKEN

keys while chatting:
  enter   send        ctrl+s  suspend the mind in place
  esc     detach — the mind keeps its memory; re-attach any time`);
  process.exit(0);
}

const url = arg("url") ?? process.env.ANDROMEDA_URL ?? "http://localhost:7433";
const token = arg("token") ?? process.env.ANDROMEDA_TOKEN ?? "";
const client = new Client({ url, token });

const fail = (msg) => { console.error(`andromeda: ${msg}`); process.exit(1); };

try {
  await client.health();
} catch (e) {
  fail(`cannot reach the control plane at ${url} (${e.message}) — set --url / ANDROMEDA_URL`);
}

let sessionId = arg("session");
let agentName = arg("agent");

if (!sessionId && !agentName) {
  // No target: show what exists and how to attach, then exit (a full
  // interactive picker is a later iteration — keep the entry obvious).
  const [agents, sessions] = await Promise.all([client.agents(), client.sessions()]);
  console.log("agents:");
  for (const a of agents) console.log(`  ${a.name.padEnd(16)} ${a.harness.padEnd(12)} ${a.phase}`);
  console.log("\nlive sessions:");
  if (!sessions.length) console.log("  (none)");
  for (const s of sessions) console.log(`  ${s.id.padEnd(20)} ${s.agent.padEnd(16)} ${s.harness.padEnd(12)} ${s.status}`);
  console.log(`\nattach:  andromeda --agent <name>   |   andromeda --session <sess-…>`);
  process.exit(0);
}

if (!sessionId) {
  // BYO-key: the user's own key for whichever provider the agent uses — read
  // straight from that provider's standard env var (a key always belongs to a
  // specific provider). Ephemeral: used only for this session, never stored.
  // Prefer an env var over --api-key (a flag is visible in `ps`).
  const apiKey =
    arg("api-key") ??
    process.env.ANTHROPIC_API_KEY ??
    process.env.OPENAI_API_KEY ??
    process.env.GEMINI_API_KEY;
  try {
    const created = await client.createSession(agentName, apiKey);
    sessionId = created.id;
  } catch (e) {
    fail(`could not create a session for agent ${agentName}: ${e.message}`);
  }
}

// Replay recent history so re-attach feels like coming back, not starting over.
let initialLines = [];
let initialCursor = "";
try {
  const events = await client.events(sessionId);
  initialLines = historyLines(events);
  if (events.length) initialCursor = events[events.length - 1].id;
} catch { /* a brand-new session has no history */ }

const instance = render(h(App, { client, sessionId, agentName, initialLines, initialCursor }));
await instance.waitUntilExit();
console.log(`detached — the mind lives on.
  re-attach:  andromeda --session ${sessionId}`);
