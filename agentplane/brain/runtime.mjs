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
import { startTurnSpan } from "./otel.mjs";
import { actorIdentity, setHandURL } from "./identity.mjs";

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

  // Rebuild approval state (#67) by folding the event log, so a cold start —
  // one where the actor was rebuilt rather than restored from a snapshot — does
  // not silently forget that a human already said yes, or re-ask a question
  // that was already answered.
  //
  // Ordering is the log's, so a later answer always wins over an earlier one:
  // a request re-raised after a denial is open again, which is what a retry
  // should mean.
  function rebuildApprovals() {
    if (!existsSync(EVENT_LOG)) return;
    let lines;
    try { lines = readFileSync(EVENT_LOG, "utf8").trim().split("\n"); }
    catch { return; }
    for (const line of lines) {
      let ev;
      try { ev = JSON.parse(line); } catch { continue; }
      const req = ev.request;
      if (!req) continue;
      if (ev.type === "tool.approval_requested") { openRequests.add(req); continue; }
      if (ev.type === "tool.approval_granted") { openRequests.delete(req); approvedRequests.add(req); continue; }
      if (ev.type === "tool.approval_denied") { openRequests.delete(req); approvedRequests.delete(req); continue; }
      // A grant is spent when the tool actually runs — replaying that keeps
      // one-shot semantics across a restart instead of resurrecting the grant.
      if (ev.type === "tool.approval_used") { approvedRequests.delete(req); }
    }
  }

  // The tool input is what a human reads to decide, so it is kept rather than
  // redacted — it goes to the same durable log that already holds the whole
  // conversation, so this adds no exposure. It IS capped: a Write of a large
  // file would otherwise put the entire body in the log and in every replay.
  const APPROVAL_INPUT_CAP = 4000;
  function summariseInput(input) {
    let text;
    try { text = JSON.stringify(input); } catch { return { unserialisable: true }; }
    if (text === undefined) return {};
    if (text.length <= APPROVAL_INPUT_CAP) return input;
    return { truncated: true, bytes: text.length, preview: text.slice(0, APPROVAL_INPUT_CAP) };
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
  // The span covering the current turn, parented to the trace of the request
  // that started it. Held here because a turn completes asynchronously, long
  // after the HTTP response that queued it.
  let turnSpan = null;
  // Tool restrictions resolved by serve at session setup (#58). Kept separate
  // from the spec so the merge direction is unambiguous: this only ever ADDS to
  // what the spec denies.
  let resolvedDisallow = [];
  // Approvals granted by a human this session, keyed by request id (#67).
  //
  // In actor memory on purpose. Every grant is ALSO written to the event log as
  // tool.approval_granted, so this set is a fold of durable state rather than a
  // second source of truth — a snapshot carries it, and rebuildApprovals()
  // replays it after a cold start.
  //
  // Grants are one-shot: consumed when the tool runs, so approving a call you
  // read does not silently approve every later call that happens to match it.
  const approvedRequests = new Set();
  // Emitted and not yet answered. Without this, a model that retries the same
  // call every turn emits a fresh request each time and the human sees a pile of
  // duplicates for one decision.
  const openRequests = new Set();
  // Called here, not beside its definition: both sets are `const` and would be
  // in the temporal dead zone earlier in the module body.
  rebuildApprovals();

  const askList = () => (Array.isArray(spec.ask) ? spec.ask : []);
  function approvalState(req) {
    if (approvedRequests.has(req)) return "granted";
    if (openRequests.has(req)) return "open";
    return "none";
  }
  // Spend a grant. One-shot, and RECORDED, so the fold agrees after a restart
  // instead of replaying a grant that was already used.
  function useApproval(req) {
    approvedRequests.delete(req);
    emit("tool.approval_used", { request: req });
  }
  // Raise a request unless one is already open for this exact call. Returns the
  // id so a caller can hold on to it.
  function requestApproval(req, tool, input) {
    if (!openRequests.has(req)) {
      openRequests.add(req);
      emit("tool.approval_requested", { request: req, tool, input: summariseInput(input) });
    }
    return req;
  }
  let turnStartedAt = 0;
  let delivered = [];        // pulled into the harness, no result yet (re-queue on teardown)
  let sessionId = existsSync(SESSION_ID_FILE) ? readFileSync(SESSION_ID_FILE, "utf8").trim() : null;

  // Session-lifetime usage totals, summed from each turn's harness-reported
  // usage (we never do price math — the vendor SDK does). Lives in the mind's
  // memory, so the meter is durable with it: survives suspend/resume, dies
  // with the session. Fields a vendor doesn't report simply never advance.
  const usage = { turns: 0, cost_usd: 0, input_tokens: 0, output_tokens: 0, cache_read_tokens: 0, cache_creation_tokens: 0 };
  function addUsage(u) {
    if (!u) return;
    usage.turns += 1;
    for (const k of ["cost_usd", "input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens"])
      if (typeof u[k] === "number") usage[k] += u[k];
  }

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
      if (turnSpan) {
        turnSpan.setHarnessSession(sessionId); // resolved by now if it wasn't at accept
        if (ev.stop_reason) turnSpan.setAttribute("agentplane.stop_reason", String(ev.stop_reason));
        turnSpan.end();
        turnSpan = null;
      }
      turnStartedAt = 0;
      delivered = [];
      addUsage(ev.usage);
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
        // Serve-resolved restrictions, read fresh each turn so a push mid-session
        // applies to the next one.
        get resolvedDisallow() { return resolvedDisallow; },
        // Approval gate (#67). The harness's PreToolUse hook calls these; the
        // runtime owns the state so it survives a harness restart.
        get askList() { return askList(); },
        approvalState, useApproval, requestApproval,
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
        // The turn died without reaching status_idle: record why and close the
        // span, or it would stay open and the trace would never be exported.
        if (turnSpan) {
          turnSpan.fail(msg);
          turnSpan.end();
          turnSpan = null;
        }
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
    acceptUserMessage(text, parentCtx) {
      emit("user.message", { content: [{ type: "text", text }] });
      emit("session.status_running", {});
      busy = true;
      // Continue serve's trace into the brain. Only the first message of a turn
      // opens a span: messages that arrive while busy are folded into the turn
      // already running, and opening a second span would leak the first.
      if (parentCtx && !turnSpan) {
        turnSpan = startTurnSpan(parentCtx, { "agentplane.actor": actorIdentity() });
        turnSpan.setHarnessSession(sessionId);
      }
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
      return { ok: true, session: identity(), harness: harness.name, sdk_session_id: sessionId, busy, queued: queue.length, last_event_at: lastEventAt, events: seq, keyed: apiKey != null, usage };
    },
    // Apply tool restrictions resolved by serve.
    //
    // UNION with the spec, never replace. If the user disabled a tool it stays
    // disabled no matter what serve sends — a control plane bug must not be able
    // to re-enable something an operator turned off. The reverse direction is
    // the whole point: serve can only take tools away.
    // Answer an open approval request (#67). Returns what happened so the
    // caller can 404 an unknown id instead of reporting a phantom success.
    //
    // Approving also QUEUES a nudge, because the model was told the tool was
    // denied and has already finished its turn. Without an input the grant
    // would sit unused until the human happened to send another message, and
    // the agent would look like it ignored the approval.
    resolveApproval(req, approve, note) {
      if (!openRequests.has(req)) {
        return { ok: false, reason: approvedRequests.has(req) ? "already granted" : "no such open request" };
      }
      openRequests.delete(req);
      if (!approve) {
        emit("tool.approval_denied", { request: req, ...(note ? { note } : {}) });
        this.acceptUserMessage(`The human DENIED approval request ${req}.${note ? " Reason: " + note + "." : ""} Do not retry it; continue without that tool or say what you cannot do.`);
        return { ok: true, decision: "denied" };
      }
      approvedRequests.add(req);
      emit("tool.approval_granted", { request: req, ...(note ? { note } : {}) });
      this.acceptUserMessage(`The human APPROVED approval request ${req}. Retry that exact tool call now — the approval is single-use.`);
      return { ok: true, decision: "granted" };
    },

    // Exposed so the HTTP layer and tests use the same gate the hook does,
    // rather than a parallel implementation that could drift from it.
    approvalState, useApproval, requestApproval,

    // Everything a human still owes a decision on. A fold over the log, not a
    // second table that could disagree with it.
    pendingApprovals() {
      const out = new Map();
      for (const ev of this.listEvents()) {
        if (ev.type === "tool.approval_requested" && openRequests.has(ev.request)) {
          out.set(ev.request, { request: ev.request, tool: ev.tool, input: ev.input, since: ev.time });
        }
      }
      return [...out.values()];
    },
    setResolvedOptions({ disallowedTools, handMcpUrl }) {
      const before = resolvedDisallow.length;
      const merged = new Set(resolvedDisallow);
      for (const t of disallowedTools || []) {
        if (typeof t === "string" && t) merged.add(t);
      }
      resolvedDisallow = [...merged];
      // The broker: serve pushes the hand MCP URL so the harness dials the
      // broker instead of the hand directly. Read via handURL() when the harness
      // builds options at the start of a turn, so a push before the first turn
      // takes effect on that turn.
      if (typeof handMcpUrl === "string" && handMcpUrl) setHandURL(handMcpUrl);
      // The harness reads this when it builds options, which happens at the
      // start of a turn — so a push mid-session takes effect on the next turn,
      // not the one in flight.
      return {
        disallowedTools: resolvedDisallow.length,
        added: resolvedDisallow.length - before,
        handMcpUrl: !!handMcpUrl,
      };
    },
  };
}
