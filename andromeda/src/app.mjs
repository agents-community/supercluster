// The andromeda UI (Ink). Zero-build ESM: React.createElement via `h`, no JSX,
// so `npx` runs the source directly — nothing to compile, nothing to ship
// but this folder.

import React, { useEffect, useRef, useState } from "react";
import { Box, Text, useApp, useInput, useStdout } from "ink";
import TextInput from "ink-text-input";
import Spinner from "ink-spinner";
import { readFileSync } from "node:fs";
import { renderMarkdown } from "./markdown.mjs";

const h = React.createElement;

// The startup banner (raw ASCII in banner.txt — no escaping headaches).
const BANNER = (() => {
  try {
    return readFileSync(new URL("./banner.txt", import.meta.url), "utf8").replace(/\n+$/, "").split("\n");
  } catch {
    return [];
  }
})();
const BANNER_W = BANNER.reduce((m, l) => Math.max(m, l.length), 0);

// ── cosmic palette ───────────────────────────────────────────────────────────
const C = {
  violet: "#a78bfa", purple: "#c084fc", magenta: "#e879f9", pink: "#f472b6",
  cyan: "#22d3ee", blue: "#60a5fa", star: "#818cf8",
  green: "#34d399", amber: "#fbbf24", red: "#fb7185",
  ink: "#0b0f1a", text: "#e5e7eb", dim: "#6b7280", faint: "#4b5563",
};
// title gradient runs across the nebula
const TITLE = [C.purple, C.magenta, C.pink, C.blue, C.cyan];

const grad = (text, palette = TITLE) =>
  [...text].map((ch, i) => h(Text, { key: i, color: palette[i % palette.length], bold: true }, ch));

// A smooth cosmic gradient for the banner — colored per character, flowing
// diagonally down-right so the whole thing shimmers.
const GRADIENT = ["#c084fc", "#e879f9", "#f472b6", "#a78bfa", "#818cf8", "#60a5fa", "#22d3ee", "#818cf8", "#a78bfa"];
const bannerLine = (text, row, offset = 0) =>
  [...text].map((ch, col) =>
    ch === " " ? ch : h(Text, { key: col, color: GRADIENT[((col + row * 2 + offset) % GRADIENT.length + GRADIENT.length) % GRADIENT.length], bold: true }, ch));

const chip = (label, bg) => h(Text, { backgroundColor: bg, color: C.ink, bold: true }, ` ${label} `);

// A glyph per tool so a stream of tool calls reads at a glance.
const TOOL_ICON = { bash: "❯", write: "✎", edit: "✎", read: "◉", list: "☰", grep: "⌕", glob: "⌕", ToolSearch: "⌕" };

// ── event → transcript line ──────────────────────────────────────────────────
function eventToLine(ev, { history = false } = {}) {
  const text = ev.content?.[0]?.text ?? "";
  switch (ev.type) {
    case "user.message": return { kind: "user", text, dim: history };
    case "agent.message": return { kind: "agent", text, dim: history };
    case "agent.tool_use": {
      // Strip the mcp__hand__ prefix and show a short arg summary so you can
      // see what it's doing (run bash, edit a file) — not just "a tool ran".
      const name = (ev.name ?? "tool").replace(/^mcp__[a-z0-9]+__/, "");
      const inp = ev.input || {};
      let s = inp.command ?? inp.path ?? inp.pattern ?? inp.query ?? inp.file_path ?? "";
      if (typeof s !== "string") s = "";
      return { kind: "tool", text: name, summary: s.replace(/\s+/g, " ").slice(0, 64), dim: history };
    }
    case "session.error": return { kind: "error", text: ev.error?.message ?? "error" };
    default: return null;
  }
}

function Line({ line }) {
  switch (line.kind) {
    case "user":
      return h(Box, { marginTop: 1 },
        chip("you", line.dim ? C.faint : C.cyan), h(Text, {}, " "),
        h(Text, { color: line.dim ? C.dim : C.text }, line.text));
    case "agent":
      // History replays stay flat + dim; live replies get full markdown.
      if (line.dim)
        return h(Box, {}, chip("andromeda", C.faint), h(Text, {}, " "),
          h(Text, { color: C.dim }, line.text));
      return h(Box, { flexDirection: "column", marginBottom: 1 },
        h(Box, {}, chip("andromeda", C.violet)),
        h(Box, { flexDirection: "column", paddingLeft: 1 }, ...renderMarkdown(line.text, C)));
    case "tool": {
      const icon = TOOL_ICON[line.text] || "⚙";
      return h(Box, {},
        h(Text, { color: C.magenta }, `  ${icon} `),
        h(Text, { color: C.magenta, bold: true }, line.text),
        line.summary ? h(Text, { color: C.faint }, `  ${line.summary}`) : null);
    }
    case "stream": // live-typing buffer; replaced by the final markdown message
      return h(Box, { flexDirection: "column" },
        h(Box, {}, chip("andromeda", C.violet)),
        h(Box, { paddingLeft: 1 }, h(Text, { color: C.text }, (line.text || "") + "▌")));
    case "error":
      return h(Box, {}, chip("!", C.red), h(Text, { color: C.red }, ` ${line.text}`));
    case "info":
      return h(Text, { color: C.star, italic: true }, ` ✦ ${line.text}`);
    default:
      return null;
  }
}

// ── slash commands ───────────────────────────────────────────────────────────
// Input starting with "/" is a command for the TUI, never a message to the
// agent. A table, so new commands (like /usage) are one-line additions.
// Exported so the slash commands can be exercised without a terminal: the
// handlers are the part that can break against a changing API, while key
// handling belongs to Ink.
export const COMMANDS = {
  sleep: {
    desc: "suspend the mind (it wakes on your next message)",
    run: ({ client, sessionId, append }) =>
      client.suspend(sessionId)
        .then(() => append({ kind: "info", text: "suspended — the mind sleeps in place (any message wakes it)" }))
        .catch((e) => append({ kind: "error", text: `suspend: ${e.message}` })),
  },
  sessions: {
    desc: "list your sessions for this agent",
    run: async ({ client, agentName, sessionId, append }) => {
      try {
        const all = await client.sessions();
        const mine = all.filter((s) => !agentName || s.agent === agentName);
        if (!mine.length) return append({ kind: "info", text: "no sessions" });
        for (const s of mine)
          append({ kind: "info", text: `${s.id === sessionId ? "▸" : " "} ${s.id}  ${s.agent}  ${s.status}` });
      } catch (e) {
        append({ kind: "error", text: `sessions: ${e.message}` });
      }
    },
  },
  usage: {
    desc: "tokens & cost this session (as the harness reports them)",
    run: async ({ client, sessionId, append }) => {
      try {
        const s = await client.session(sessionId);
        const u = s.usage;
        if (!u) return append({ kind: "info", text: s.status === "sleeping" ? "the mind is asleep — usage shows once it's awake" : "no usage reported yet" });
        const n = (x) => (typeof x === "number" ? x.toLocaleString() : "—");
        const cost = typeof u.cost_usd === "number" && u.cost_usd > 0 ? `$${u.cost_usd.toFixed(4)}` : "—";
        append({ kind: "info", text: `turns ${n(u.turns)} · in ${n(u.input_tokens)} tok · out ${n(u.output_tokens)} tok · cache read ${n(u.cache_read_tokens)} · cost ${cost}` });
      } catch (e) {
        append({ kind: "error", text: `usage: ${e.message}` });
      }
    },
  },
  help: {
    desc: "show available commands",
    run: ({ append }) => {
      for (const [name, c] of Object.entries(COMMANDS))
        append({ kind: "info", text: `/${name} — ${c.desc}` });
    },
  },
  quit: {
    desc: "detach (the mind keeps living; esc does the same)",
    run: ({ exit }) => exit(),
  },
};

function runCommand(text, ctx) {
  const name = text.slice(1).trim().split(/\s+/)[0].toLowerCase();
  const cmd = COMMANDS[name];
  if (!cmd) {
    ctx.append({ kind: "info", text: `unknown command /${name} — try /help` });
    return;
  }
  return cmd.run(ctx);
}

function Welcome({ cols }) {
  // Animate: shift the gradient every tick so the banner shimmers.
  const [t, setT] = useState(0);
  useEffect(() => {
    const id = setInterval(() => setT((x) => x + 1), 130);
    return () => clearInterval(id);
  }, []);
  const banner = BANNER.length && cols >= BANNER_W
    ? BANNER.map((ln, i) => h(Text, { key: i }, ...bannerLine(ln, i, t)))
    : [h(Text, { key: "s" }, grad("✦  ·  ✦  ·  ✦  ·  ✦", [C.star, C.violet, C.magenta, C.blue]))];
  return h(Box, { flexDirection: "column", marginTop: 1, marginBottom: 1, paddingLeft: 1 },
    ...banner,
    h(Box, { marginTop: 1 }, h(Text, { color: C.dim }, [
      "talk to it · ",
      h(Text, { key: "s", color: C.amber }, "/sleep"), " to sleep it · ",
      h(Text, { key: "h", color: C.violet }, "/help"), " for commands · ",
      h(Text, { key: "e", color: C.cyan }, "esc"), " to leave.",
    ])));
}

function StatusPill({ state, elapsed }) {
  const secs = elapsed ? h(Text, { key: "t", color: C.faint }, ` ${elapsed}s`) : null;
  if (state === "waking")
    return h(Text, { color: C.amber }, [h(Spinner, { key: "s", type: "dots" }), " waking from checkpoint", secs]);
  if (state === "thinking")
    return h(Text, { color: C.violet }, [h(Spinner, { key: "s", type: "dots" }), " working", secs]);
  return h(Text, { color: C.green }, "● online · it remembers");
}

export function App({ client, sessionId, agentName, initialLines, initialCursor }) {
  const { exit } = useApp();
  const { stdout } = useStdout();
  const [lines, setLines] = useState(initialLines);
  const [input, setInput] = useState("");
  const [state, setState] = useState("idle"); // idle | waking | thinking
  const [elapsed, setElapsed] = useState(0);
  const cursor = useRef(initialCursor);
  const busy = state !== "idle";

  // Tick an elapsed-seconds counter while the mind is working, so a long first
  // turn shows progress instead of a silent wait.
  useEffect(() => {
    if (state === "idle") { setElapsed(0); return; }
    const t0 = Date.now();
    const id = setInterval(() => setElapsed(Math.round((Date.now() - t0) / 1000)), 500);
    return () => clearInterval(id);
  }, [state]);

  const append = (line) => line && setLines((ls) => [...ls, line]);

  const cmdCtx = { client, sessionId, agentName, append, exit };

  useInput((ch, key) => {
    if (key.escape) exit();
    if (key.ctrl && ch === "s") { // kept as a /sleep alias — no muscle-memory break
      setTimeout(() => setInput(""), 0); // TextInput also gets the key — clear it
      runCommand("/sleep", cmdCtx);
    }
  });

  const submit = async (text) => {
    text = text.trim();
    if (!text || busy) return;
    setInput("");
    if (text.startsWith("/")) { await runCommand(text, cmdCtx); return; }
    append({ kind: "user", text });
    setState("waking");
    try {
      await client.send(sessionId, text, (attempt) =>
        append({ kind: "info", text: `waking the mind… (attempt ${attempt})` }));
      setState("thinking");
      let buf = "";
      const setStream = () => setLines((ls) => {
        const c = ls.slice();
        if (c.length && c[c.length - 1].kind === "stream") c[c.length - 1] = { kind: "stream", text: buf };
        else c.push({ kind: "stream", text: buf });
        return c;
      });
      const clearStream = () => setLines((ls) =>
        ls.length && ls[ls.length - 1].kind === "stream" ? ls.slice(0, -1) : ls);
      cursor.current = await client.streamTurn(sessionId, cursor.current, (ev) => {
        if (ev.type === "session.status_running" || ev.type === "user.message") return;
        if (ev.type === "agent.message_delta") { buf += ev.text || ""; setStream(); return; }
        if (ev.type === "agent.message") { clearStream(); buf = ""; append(eventToLine(ev)); return; }
        if (ev.type === "session.status_idle") { clearStream(); buf = ""; return; }
        append(eventToLine(ev)); // tool_use, error
      });
    } catch (e) {
      append({ kind: "error", text: e.message });
    }
    setState("idle");
  };

  const rows = stdout?.rows ?? 30;
  const visible = lines.slice(-(Math.max(rows - 9, 4)));

  return h(Box, { flexDirection: "column", height: rows - 1 },
    // header band
    h(Box, { justifyContent: "space-between", paddingX: 1 },
      h(Box, {}, ...grad("✦ ANDROMEDA")),
      h(StatusPill, { state, elapsed })),
    h(Box, { paddingX: 1 }, h(Text, { color: C.faint }, "─".repeat(Math.max((stdout?.columns ?? 80) - 2, 10)))),

    // transcript
    h(Box, { flexDirection: "column", flexGrow: 1, paddingX: 1 },
      lines.length === 0 ? h(Welcome, { cols: stdout?.columns ?? 80 }) : null,
      ...visible.map((line, i) => h(Line, { key: i, line }))),

    // input
    h(Box, { borderStyle: "round", borderColor: C.magenta, paddingX: 1 },
      h(Text, { color: C.cyan, bold: true }, "› "),
      h(TextInput, { value: input, onChange: setInput, onSubmit: submit, placeholder: "message your durable mind…", focus: !busy })),

    // footer
    h(Box, { justifyContent: "space-between", paddingX: 1 },
      h(Box, {}, chip(agentName || "agent", C.blue), h(Text, { color: C.dim }, ` ${sessionId}`)),
      h(Text, { color: C.dim }, [
        h(Text, { key: "1", color: C.cyan }, "enter"), " send  ",
        h(Text, { key: "2", color: C.violet }, "/help"), " commands  ",
        h(Text, { key: "3", color: C.pink }, "esc"), " detach",
      ])));
}

export function historyLines(events, keep = 20) {
  return events.slice(-keep).map((ev) => eventToLine(ev, { history: true })).filter(Boolean);
}
