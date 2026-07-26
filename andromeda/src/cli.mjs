#!/usr/bin/env node
// andromeda — a beautiful terminal for durable agent minds.
//
//   andromeda login                save your endpoint + token to ~/.andromeda
//   andromeda --agent buddy        new session, start chatting
//   andromeda --session sess-…     re-attach (history replays)
//   andromeda                      pick from live sessions/agents
//   andromeda logout | whoami
//
// Connection resolves as flags > env > ~/.andromeda config:
//   --url   / ANDROMEDA_URL     control-plane URL   (default http://localhost:7433)
//   --token / ANDROMEDA_TOKEN   bearer token

import React from "react";
import { render } from "ink";
import readline from "node:readline";
import { Client } from "./api.mjs";
import { App, historyLines } from "./app.mjs";
import { loadConfig, saveConfig, clearConfig, configPath } from "./config.mjs";

const h = React.createElement;
const fail = (msg) => { console.error(`andromeda: ${msg}`); process.exit(1); };

function arg(name) {
  const i = process.argv.indexOf(`--${name}`);
  return i >= 0 ? process.argv[i + 1] : undefined;
}

function prompt(q) {
  const rl = readline.createInterface({ input: process.stdin, output: process.stdout });
  return new Promise((res) => rl.question(q, (a) => { rl.close(); res(a.trim()); }));
}

const positional = process.argv[2] && !process.argv[2].startsWith("-") ? process.argv[2] : null;
const cfg = loadConfig();

// ---- auth commands (like `hf auth login`) --------------------------------

async function runLogin() {
  // URL: from a flag/env if given, else prompt (defaulting to the saved one).
  const defUrl = arg("url") ?? process.env.ANDROMEDA_URL ?? cfg.url ?? "";
  let url = defUrl;
  if (arg("url") === undefined && !process.env.ANDROMEDA_URL) {
    url = (await prompt(`Control-plane URL${defUrl ? ` [${defUrl}]` : ""}: `)) || defUrl;
  }
  if (!url) fail("no URL — pass --url or set ANDROMEDA_URL");

  const c = new Client({ url, token: "" });
  try { await c.health(); } catch (e) { fail(`cannot reach ${url} (${e.message})`); }

  // Two ways in: paste a token, or self-issue one with an allowlisted email.
  let token = arg("token");
  let user;
  if (!token) {
    const email = arg("email") ?? (await prompt("Email (allowlisted): "));
    if (!email) fail("no email or --token provided");
    try {
      const r = await c.access(email);
      token = r.token;
      user = r.user;
    } catch (e) {
      if (e.status === 403) fail(`"${email}" isn't on the allowlist — ask your host to add it`);
      if (e.status === 503) fail(`self-service access isn't enabled here — get a token from your host, then: andromeda login --token <apl_…>`);
      fail(`login failed: ${e.message}`);
    }
  }
  saveConfig({ url, token, user });
  console.log(`\n✔ logged in${user ? ` as ${user}` : ""} — saved to ${configPath}`);
  console.log(`  next:  andromeda --agent migrator`);
  process.exit(0);
}

if (positional === "logout") {
  console.log(clearConfig() ? `logged out — removed ${configPath}` : "not logged in");
  process.exit(0);
}
if (positional === "whoami") {
  if (!cfg.token) fail("not logged in — run: andromeda login");
  console.log(`user: ${cfg.user || "(unknown)"}\nurl:  ${cfg.url}`);
  process.exit(0);
}
if (positional === "login") {
  await runLogin(); // exits
}

if (process.argv.includes("--help") || process.argv.includes("-h")) {
  console.log(`andromeda — a terminal for durable agent minds

usage:
  andromeda login                save your endpoint + token to ~/.andromeda
  andromeda --agent <name>       mint a new session and attach
  andromeda --session <sess-…>   re-attach to an existing mind
  andromeda                      list agents & sessions, then pick
  andromeda logout | whoami

connection (flags > env > ~/.andromeda):
  --url <url>       or ANDROMEDA_URL    (default http://localhost:7433)
  --token <token>   or ANDROMEDA_TOKEN

keys while chatting:
  enter   send        ctrl+s  suspend the mind in place
  esc     detach — the mind keeps its memory; re-attach any time`);
  process.exit(0);
}

// ---- run: connection resolves flags > env > saved config -----------------

const url = arg("url") ?? process.env.ANDROMEDA_URL ?? cfg.url ?? "http://localhost:7433";
const token = arg("token") ?? process.env.ANDROMEDA_TOKEN ?? cfg.token ?? "";
const client = new Client({ url, token });

try {
  await client.health();
} catch (e) {
  fail(`cannot reach the control plane at ${url} (${e.message}) — check --url / ANDROMEDA_URL, or run: andromeda login`);
}
if (!token) {
  fail("no token — run: andromeda login   (or set ANDROMEDA_TOKEN / pass --token)");
}

let sessionId = arg("session");
let agentName = arg("agent");

if (!sessionId && !agentName) {
  // No target: show what exists and how to attach, then exit.
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
  // straight from that provider's standard env var. Ephemeral: used only for
  // this session, never stored. Prefer an env var over --api-key (a flag is
  // visible in `ps`).
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
