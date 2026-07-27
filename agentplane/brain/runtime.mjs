// The harness-agnostic runtime: everything durability-critical lives here, once.
//
// Responsibilities (a Harness has NONE of these):
//  - the input queue and the turn clock (stamped at pull time — finding #2)
//  - the turn watchdog: abort a turn that exceeds the deadline (finding #1)
//  - the poison-message guard: re-queue an in-flight message once on teardown
//  - the event log (events.jsonl on the actor fs) + SSE fan-out
//  - vendor session-id persistence (resume-by-id across process lives)
//
// The runtime drives a Harness (see harness/index.mjs): it feeds inputs, maps
// the harness's normalized events onto the wire, and restarts the harness with
// resume-by-id on any error.

import { appendFileSync, existsSync, mkdirSync, readFileSync, writeFileSync } from "node:fs";

export function createRuntime({ workdir, spec, harness, identity }) {
  const EVENT_LOG = `${workdir}/events.jsonl`;
  const SESSION_ID_FILE = `${workdir}/.session-id`;
  const deadlineMs = 1000 * (spec.turnDeadlineSeconds || Number(process.env.TURN_DEADLINE_SECONDS) || 300);
  mkdirSync(workdir, { recursive: true });

  // ---- event log ----
  let seq = 0;
  const buffered = [];       // this process's events (SSE live buffer)
  const sseClients = new Set();
  let lastEventAt = null;
  if (existsSync(EVENT_LOG)) {
    try { seq = readFileSync(EVENT_LOG, "utf8").trim().split("\n").length; } catch { /* best-effort */ }
  }
  function emit(type, payload = {}) {
    const ev = { id: `evt_${String(++seq).padStart(6, "0")}`, type, session: identity(), time: new Date().toISOString(), ...payload };
    lastEventAt = ev.time;
    buffered.push(ev);
    try { appendFileSync(EVENT_LOG, JSON.stringify(ev) + "\n"); }
    catch (e) { console.error("event log append failed:", e.message); }
    const frame = `id: ${ev.id}\nevent: ${type}\ndata: ${JSON.stringify(ev)}\n\n`;
    for (const res of sseClients) res.write(frame);
    return ev;
  }

  // Live-only fan-out: stream to current SSE viewers WITHOUT persisting to the
  // event log or replay buffer (for transient text deltas). No id, so it never
  // advances a client's cursor; the durable `agent.message` still lands via emit().
  function emitLive(type, payload = {}) {
    const ev = { type, session: identity(), ...payload };
    const frame = `event: ${type}\ndata: ${JSON.stringify(ev)}\n\n`;
    for (const res of sseClients) res.write(frame);
  }

  // ---- turn state + input queue ----
  const queue = [];
  let wakeInput = null, failInput = null;
  let busy = false;
  let turnStartedAt = 0;
  let delivered = [];        // pulled into the harness, no result yet (re-queue on teardown)
  let sessionId = existsSync(SESSION_ID_FILE) ? readFileSync(SESSION_ID_FILE, "utf8").trim() : null;

  // Ephemeral per-session vendor key (BYO-key). Lives ONLY here, in process
  // memory: never written to disk, never an event, never logged. It survives
  // suspend/resume and harness restarts (it's in the checkpointed memory) and
  // dies with the actor. null = fall back to the image's env key (shared).
  let apiKey = null;

  function setSessionId(id) {
    sessionId = id;
    try { writeFileSync(SESSION_ID_FILE, id); } catch (e) { console.error("session-id persist failed:", e.message); }
  }

  // The shared input stream. Both harnesses pull from this; the turn clock
  // starts at PULL time (finding #2: queue latency is not execution time).
  async function* inputs() {
    for (;;) {
      while (queue.length === 0) {
        await new Promise((res, rej) => { wakeInput = res; failInput = rej; });
        wakeInput = null; failInput = null;
      }
      const m = queue.shift();
      busy = true;
      turnStartedAt = Date.now();
      delivered.push(m);
      yield m;
    }
  }

  // Map a harness's normalized event onto the wire + manage turn-end state.
  function onHarnessEvent(ev) {
    if (ev.type === "agent.message_delta") {
      emitLive("agent.message_delta", { text: ev.text }); // live typing; not persisted
    } else if (ev.type === "agent.tool_use") {
      emit("agent.tool_use", { name: ev.name, input: ev.input });
    } else if (ev.type === "agent.message") {
      emit("agent.message", { content: ev.content });
    } else if (ev.type === "session.status_idle") {
      busy = false;
      turnStartedAt = 0;
      delivered = [];
      emit("session.status_idle", { stop_reason: ev.stop_reason, usage: ev.usage });
    }
  }

  // ---- the driver: run the harness, watchdog it, restart on error ----
  let started = false;
  function ensureStarted() { if (!started) { started = true; drive(); } }

  async function drive() {
    for (;;) {
      const ac = new AbortController();
      let timedOut = false;
      const ctx = {
        spec, workdir,
        get sessionId() { return sessionId; },
        setSessionId,
        get apiKey() { return apiKey; }, // BYO-key override, else null → env
        signal: ac.signal,
      };
      // Watchdog: a turn past the deadline is torn down. Abort signals the
      // harness to interrupt; rejecting a pending input unblocks a harness
      // waiting between turns. A fire always ends run() → restart (resume-by-id).
      const watchdog = setInterval(() => {
        if (busy && turnStartedAt && Date.now() - turnStartedAt > deadlineMs) {
          console.error(`turn watchdog: deadline ${deadlineMs}ms exceeded — tearing down turn`);
          timedOut = true;
          ac.abort();
          if (failInput) { const f = failInput; failInput = null; f(new Error("TURN_DEADLINE_EXCEEDED")); }
        }
      }, 5000);
      try {
        console.log(`${harness.name} harness starting`, { resume: sessionId, deadlineMs });
        for await (const ev of harness.run(inputs(), ctx)) onHarnessEvent(ev);
        console.log(`${harness.name} harness stream ended; restarting`);
      } catch (e) {
        const msg = String((e && e.message) || e);
        const timeout = timedOut || msg.includes("TURN_DEADLINE_EXCEEDED");
        console.error(`${harness.name} harness error:`, msg);
        emit("session.error", {
          error: {
            message: timeout ? "turn deadline exceeded; harness restarted (resume-by-id)" : msg,
            reason: timeout ? "turn_timeout" : "harness_error",
          },
        });
        // Poison-message guard: re-queue delivered-but-unanswered inputs ONCE so
        // work behind a wedged turn survives teardown but a poison message can't
        // loop forever.
        for (let i = delivered.length - 1; i >= 0; i--) {
          const m = delivered[i];
          if (!m._retried) { m._retried = true; queue.unshift(m); }
        }
        delivered = [];
      } finally {
        clearInterval(watchdog);
        busy = false;
        turnStartedAt = 0;
      }
      await new Promise((r) => setTimeout(r, 1000)); // backoff, then resume-by-id
    }
  }

  // ---- API for the HTTP layer ----
  return {
    // Accept a user message: record it, mark running, enqueue, ensure the
    // harness is alive, and wake a waiting input pull.
    acceptUserMessage(text) {
      emit("user.message", { content: [{ type: "text", text }] });
      emit("session.status_running", {});
      busy = true;
      queue.push({ text });
      ensureStarted();
      if (wakeInput) wakeInput();
    },
    // Persisted turn log (optionally after a cursor).
    listEvents(since) {
      const raw = existsSync(EVENT_LOG) ? readFileSync(EVENT_LOG, "utf8").trim() : "";
      let events = raw ? raw.split("\n").map((l) => { try { return JSON.parse(l); } catch { return null; } }).filter(Boolean) : [];
      if (since) events = events.filter((ev) => ev.id > since);
      return events;
    },
    // Subscribe an SSE response: replay from the persisted log (cursor-aware),
    // then stream live. Cursors work across restarts because ids are the log's.
    subscribeSSE(res, since) {
      let replay = buffered;
      try {
        if (existsSync(EVENT_LOG)) {
          const raw = readFileSync(EVENT_LOG, "utf8").trim();
          if (raw) replay = raw.split("\n").map((l) => { try { return JSON.parse(l); } catch { return null; } }).filter(Boolean);
        }
      } catch { /* fall back to in-memory buffer */ }
      if (since) replay = replay.filter((ev) => ev.id > since);
      for (const ev of replay) res.write(`id: ${ev.id}\nevent: ${ev.type}\ndata: ${JSON.stringify(ev)}\n\n`);
      sseClients.add(res);
      const hb = setInterval(() => res.write(`: hb ${Date.now()}\n\n`), 10000);
      res.on?.("close", () => { clearInterval(hb); sseClients.delete(res); });
      return () => { clearInterval(hb); sseClients.delete(res); };
    },
    // setApiKey stores the ephemeral per-session key in memory only. Takes
    // effect on the NEXT turn (the harness reads ctx.apiKey at run start).
    // Returns nothing and logs nothing — the key must not surface anywhere.
    setApiKey(k) { apiKey = k || null; },
    health() {
      return { ok: true, session: identity(), harness: harness.name, sdk_session_id: sessionId, busy, queued: queue.length, last_event_at: lastEventAt, events: seq, keyed: apiKey != null };
    },
  };
}
