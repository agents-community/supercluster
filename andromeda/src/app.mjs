// The andromeda UI (Ink). Zero-build ESM: React.createElement via `h`, no JSX,
// so `npx` runs the source directly — nothing to compile, nothing to ship
// but this folder.

import React, { useEffect, useRef, useState } from "react";
import { Box, Text, useApp, useInput, useStdout } from "ink";
import TextInput from "ink-text-input";
import Spinner from "ink-spinner";
import { readFileSync } from "node:fs";

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

// ── event → transcript line ──────────────────────────────────────────────────
function eventToLine(ev, { history = false } = {}) {
  const text = ev.content?.[0]?.text ?? "";
  switch (ev.type) {
    case "user.message": return { kind: "user", text, dim: history };
    case "agent.message": return { kind: "agent", text, dim: history };
    case "agent.tool_use": return { kind: "tool", text: ev.name ?? "tool", dim: history };
    case "session.error": return { kind: "error", text: ev.error?.message ?? "error" };
    default: return null;
  }
}

function Line({ line }) {
  switch (line.kind) {
    case "user":
      return h(Box, {},
        chip("you", line.dim ? C.faint : C.cyan), h(Text, {}, " "),
        h(Text, { color: line.dim ? C.dim : C.text }, line.text));
    case "agent":
      return h(Box, {},
        chip("andromeda", line.dim ? C.faint : C.violet), h(Text, {}, " "),
        h(Text, { color: line.dim ? C.dim : C.text }, line.text));
    case "tool":
      return h(Text, { color: C.magenta, italic: true }, `   ⚙ ${line.text}`);
    case "error":
      return h(Box, {}, chip("!", C.red), h(Text, { color: C.red }, ` ${line.text}`));
    case "info":
      return h(Text, { color: C.star, italic: true }, ` ✦ ${line.text}`);
    default:
      return null;
  }
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
      h(Text, { key: "s", color: C.amber }, "ctrl+s"), " to sleep it · ",
      h(Text, { key: "e", color: C.cyan }, "esc"), " to leave.",
    ])));
}

function StatusPill({ state }) {
  if (state === "waking")
    return h(Text, { color: C.amber }, [h(Spinner, { key: "s", type: "dots" }), " waking from checkpoint"]);
  if (state === "thinking")
    return h(Text, { color: C.violet }, [h(Spinner, { key: "s", type: "dots" }), " thinking"]);
  return h(Text, { color: C.green }, "● online · it remembers");
}

export function App({ client, sessionId, agentName, initialLines, initialCursor }) {
  const { exit } = useApp();
  const { stdout } = useStdout();
  const [lines, setLines] = useState(initialLines);
  const [input, setInput] = useState("");
  const [state, setState] = useState("idle"); // idle | waking | thinking
  const cursor = useRef(initialCursor);
  const busy = state !== "idle";

  const append = (line) => line && setLines((ls) => [...ls, line]);

  useInput((ch, key) => {
    if (key.escape) exit();
    if (key.ctrl && ch === "s") {
      setTimeout(() => setInput(""), 0); // TextInput also gets the key — clear it
      client.suspend(sessionId)
        .then(() => append({ kind: "info", text: "suspended — the mind sleeps in place (any message wakes it)" }))
        .catch((e) => append({ kind: "error", text: `suspend: ${e.message}` }));
    }
  });

  const submit = async (text) => {
    text = text.trim();
    if (!text || busy) return;
    setInput("");
    append({ kind: "user", text });
    setState("waking");
    try {
      await client.send(sessionId, text, (attempt) =>
        append({ kind: "info", text: `waking the mind… (attempt ${attempt})` }));
      setState("thinking");
      cursor.current = await client.streamTurn(sessionId, cursor.current, (ev) => {
        if (ev.type === "session.status_running" || ev.type === "user.message") return;
        append(eventToLine(ev));
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
      h(StatusPill, { state })),
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
        h(Text, { key: "2", color: C.amber }, "ctrl+s"), " sleep  ",
        h(Text, { key: "3", color: C.pink }, "esc"), " detach",
      ])));
}

export function historyLines(events, keep = 20) {
  return events.slice(-keep).map((ev) => eventToLine(ev, { history: true })).filter(Boolean);
}
