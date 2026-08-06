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
import { readFileSync } from "node:fs";
import { Client } from "./api.mjs";
import { App, historyLines } from "./app.mjs";
import { loadConfig, saveConfig, clearConfig, configPath } from "./config.mjs";

const h = React.createElement;
const fail = (msg) => { console.error(`andromeda: ${msg}`); process.exit(1); };

// Accepts `--name value`, `--name=value`, and the single-dash short form
// `-f value`. The short form matters: `agent create -f <spec.yaml>` is what
// --help and the onboarding guide both tell you to type, and matching only
// `--f` meant that exact command fell through to the usage error with no hint
// that the flag was the problem.
function arg(name) {
  for (const form of [`--${name}`, `-${name}`]) {
    const i = process.argv.indexOf(form);
    if (i >= 0) return process.argv[i + 1];
    const eq = process.argv.find((a) => a.startsWith(`${form}=`));
    if (eq !== undefined) return eq.slice(form.length + 1);
  }
  return undefined;
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
  console.log(`  next:  andromeda --agent starter`);
  process.exit(0);
}

// ---- credential vault: store a git PAT (or any secret) for your agents ----
async function runCred(client) {
  const sub = process.argv[3];
  if (sub === "ls" || sub === "list") {
    const names = await client.listCredentials();
    console.log(names.length ? names.join("\n") : "(no credentials)");
    process.exit(0);
  }
  if (sub === "rm" || sub === "delete") {
    const name = process.argv[4];
    if (!name) fail("usage: andromeda cred rm <name>");
    await client.deleteCredential(name);
    console.log(`removed credential "${name}"`);
    process.exit(0);
  }
  if (sub === "set") {
    const name = process.argv[4] || "gh-token";
    const type = arg("type") || "git";
    let payload;
    if (type === "git") {
      const host = arg("host") || (await prompt("Git host [github.com]: ")) || "github.com";
      const username = arg("user") || (await prompt("Username [x-access-token]: ")) || "x-access-token";
      const value = arg("token") || (await prompt(`Token (PAT for ${host}): `));
      if (!value) fail("no token provided");
      payload = { type: "git", value, host, username };
    } else if (type === "env") {
      const varName = arg("var") || (await prompt("Env var name: "));
      const value = arg("value") || (await prompt("Value: "));
      if (!varName || !value) fail("need a var name and value");
      payload = { type: "env", value, varName };
    } else if (type === "header") {
      const value = arg("value") || (await prompt("Header value: "));
      if (!value) fail("no value provided");
      payload = { type: "header", value };
    } else {
      fail(`unknown --type "${type}" (use git | env | header)`);
    }
    await client.putCredential(name, payload);
    console.log(`✔ stored "${name}" (${type}) — reference it in an agent's  credentials: [${name}]`);
    process.exit(0);
  }
  fail("usage: andromeda cred set <name> [--type git|env|header] | ls | rm <name>");
}

// ---- agents: create from a spec file, or list -----------------------------
async function runAgent(client) {
  const sub = process.argv[3];
  if (sub === "ls" || sub === "list") {
    for (const a of await client.agents())
      console.log(`${a.name.padEnd(18)} ${a.harness.padEnd(12)} ${String(a.version ? "v" + a.version : "-").padEnd(5)} ${a.phase}`);
    process.exit(0);
  }
  if (sub === "create") {
    const file = arg("f") ?? arg("file");
    if (!file) fail("usage: andromeda agent create -f <spec.yaml>");
    let spec;
    try { spec = readFileSync(file, "utf8"); } catch (e) { fail(`cannot read ${file}: ${e.message}`); }
    let r;
    try { r = await client.createAgent(spec); } catch (e) { fail(`create failed: ${e.message}`); }
    console.log(`agent "${r.name}": ${r.note || r.phase}`);
    console.log(`  watch it come up:  andromeda agent ls   (until phase is Ready)`);
    process.exit(0);
  }
  // `agent get` exists so writing your own agent needs no repo checkout. Every
  // version stores the spec verbatim, so an existing agent IS the example — and
  // one from THIS platform, so its image digest is already right for the
  // account. A digest copied out of a repo's examples/ is wrong everywhere but
  // the account that built it, which is the failure this avoids.
  if (sub === "get" || sub === "show") {
    const name = process.argv[4];
    if (!name || name.startsWith("-")) fail("usage: andromeda agent get <name> [--version N] > my-agent.yaml");
    let versions;
    try { versions = await client.agentVersions(name); } catch (e) { fail(`cannot read ${name}: ${e.message}`); }
    if (!versions.length) fail(`agent "${name}" has no recorded versions`);
    const want = arg("version");
    // Don't trust list order for "latest" — pick the highest version number.
    const pick = want
      ? versions.find((v) => String(v.version) === String(want))
      : versions.reduce((a, b) => (b.version > a.version ? b : a));
    if (!pick) fail(`agent "${name}" has no version ${want} (have: ${versions.map((v) => v.version).join(", ")})`);
    if (!pick.spec) fail(`version ${pick.version} of "${name}" predates spec capture — try a newer version`);
    // Header on stderr, spec on stdout: `> my-agent.yaml` must capture valid
    // YAML only, with nothing prepended that a later `agent create` would choke
    // on. The guidance still reaches a human running it interactively.
    process.stderr.write(`# ${name} v${pick.version}, created ${pick.created_at} by ${pick.created_by}\n`);
    process.stderr.write(`# Edit "name:" before creating — reusing a name makes a new VERSION of that agent.\n`);
    process.stdout.write(pick.spec.endsWith("\n") ? pick.spec : pick.spec + "\n");
    process.exit(0);
  }
  // Without this, an agent created from the CLI could only be removed with a
  // hand-rolled curl — so anyone experimenting accumulated dead agents.
  if (sub === "rm" || sub === "delete") {
    const name = process.argv[4];
    if (!name || name.startsWith("-")) fail("usage: andromeda agent rm <name> [--cascade]");
    const cascade = process.argv.includes("--cascade");
    let r;
    try {
      r = await client.deleteAgent(name, cascade);
    } catch (e) {
      if (e.status === 409) {
        // Name the sessions rather than just refusing: --cascade destroys minds,
        // and the point of the 409 is that the caller sees what they would lose.
        const live = e.body?.sessions ?? [];
        console.error(`agent "${name}" still has ${live.length} live session(s):`);
        for (const s of live) console.error(`  ${s}`);
        fail(`re-run with --cascade to delete the agent AND these sessions`);
      }
      fail(`delete failed: ${e.message}`);
    }
    const gone = r.sessions_removed ?? [];
    console.log(`agent "${name}" deleted${gone.length ? ` (${gone.length} session(s) removed)` : ""}`);
    process.exit(0);
  }
  fail("usage: andromeda agent create -f <spec.yaml> | get <name> | rm <name> | ls");
}

// ---- sessions: list, optionally filtered by --agent -----------------------
async function runSessions(client) {
  const filter = arg("agent");
  let sessions = await client.sessions();
  if (filter) sessions = sessions.filter((s) => s.agent === filter);
  if (!sessions.length) {
    console.log(filter ? `(no sessions for agent "${filter}")` : "(no sessions)");
    process.exit(0);
  }
  for (const s of sessions) console.log(`${s.id.padEnd(20)} ${s.agent.padEnd(18)} ${s.harness.padEnd(12)} ${s.status}`);
  console.log(`\nattach:  andromeda --session <sess-…>`);
  process.exit(0);
}

if (positional === "logout") {
  console.log(clearConfig() ? `logged out — removed ${configPath}` : "not logged in");
  process.exit(0);
}
if (positional === "whoami") {
  // Resolve exactly like every other command (flags > env > config). Reading
  // only the config file made `whoami` report "not logged in" while `agent ls`
  // and `sessions` worked fine off ANDROMEDA_TOKEN — the one command you run
  // to check your setup was the one that lied about it.
  const who = {
    url: arg("url") ?? process.env.ANDROMEDA_URL ?? cfg.url,
    token: arg("token") ?? process.env.ANDROMEDA_TOKEN ?? cfg.token,
  };
  if (!who.token) fail("not logged in — run: andromeda login (or set ANDROMEDA_TOKEN)");
  const from = arg("token") !== undefined ? "--token flag"
    : process.env.ANDROMEDA_TOKEN ? "ANDROMEDA_TOKEN"
    : configPath;
  // Where the credentials came from is the thing you actually need when the
  // client is talking to a different cluster than you expect.
  console.log(`user: ${cfg.user || "(unknown)"}\nurl:  ${who.url}\nfrom: ${from}`);
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
  andromeda sessions [--agent X] list sessions (optionally for one agent)
  andromeda agent ls             list agents and the version each is serving
  andromeda agent get starter    print an agent's spec (start your own from it)
  andromeda agent create -f f.yaml   create or re-version an agent from a spec
  andromeda agent rm <name>      delete an agent (--cascade if it has sessions)
  andromeda cred set gh-token    store a git PAT (or: cred ls | cred rm <name>)
  andromeda logout | whoami

connection (flags > env > ~/.andromeda):
  --url <url>       or ANDROMEDA_URL    (default http://localhost:7433)
  --token <token>   or ANDROMEDA_TOKEN

while chatting:
  enter   send a message
  /sleep  suspend the mind in place · /quit  detach (it keeps its memory)`);
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

// Authenticated subcommands (need the client).
if (positional === "cred") await runCred(client);
if (positional === "agent") await runAgent(client);
if (positional === "sessions") await runSessions(client);

let sessionId = arg("session");
let agentName = arg("agent");

if (!sessionId && !agentName) {
  // No target: show what exists and how to attach, then exit.
  const [agents, sessions] = await Promise.all([client.agents(), client.sessions()]);
  console.log("agents:");
  for (const a of agents)
    console.log(`  ${a.name.padEnd(16)} ${a.harness.padEnd(12)} ${String(a.version ? "v" + a.version : "-").padEnd(5)} ${a.phase}`);
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
