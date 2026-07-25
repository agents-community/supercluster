// The Harness interface (Strategy pattern) and the registry.
//
// A Harness adapts ONE agent CLI/SDK to the agentplane runtime. The runtime
// (../runtime.mjs) owns everything reliability-critical and harness-agnostic —
// the input queue, the turn watchdog, the poison-message guard, the event log,
// SSE, and HTTP. A Harness owns only the vendor-specific parts: translating an
// AgentSpec into vendor options, and turning a stream of user messages into a
// stream of *normalized* agentplane events.
//
// This split is deliberate: the durability guarantees (findings #1–#4, watchdog
// v2) are written once, in the runtime, and every harness inherits them. Adding
// a harness must not require re-implementing any of that — see
// docs/harness-interface.md for the step-by-step.
//
// ── The contract ────────────────────────────────────────────────────────────
//
// @typedef {Object} HarnessInput
// @property {string} text                 one user message (the turn's prompt)
//
// A NormalizedEvent is one of (the runtime maps these straight to the wire):
//   { type: "agent.message",       content: [{ type: "text", text }] }
//   { type: "agent.tool_use",      name, input }
//   { type: "session.status_idle", stop_reason: {type}, usage }
// Throwing from run() ends the turn: the runtime emits session.error, re-queues
// the in-flight message once (poison guard), and restarts run() (resume-by-id).
//
// @typedef {Object} HarnessContext
// @property {object}  spec                 the parsed AgentSpec
// @property {string}  workdir              cwd for the harness (/workspace)
// @property {string|null} sessionId        vendor session/thread id for resume
// @property {(id: string) => void} setSessionId   persist a new vendor id
// @property {AbortSignal} signal           aborts when the watchdog fires this
//                                          turn; a harness that can actively
//                                          interrupt SHOULD listen and do so
//
// @typedef {Object} Harness
// @property {string} name
// @property {(inputs: AsyncIterable<HarnessInput>, ctx: HarnessContext)
//            => AsyncGenerator<object>} run
//     Long-lived. Pulls messages from `inputs` (each pull stamps the turn clock
//     in the runtime — do not buffer ahead), yields NormalizedEvents, and calls
//     ctx.setSessionId whenever the vendor id changes.

/**
 * name → lazy loader. Register a new harness here (and nowhere else).
 *
 * Loaders are LAZY so per-harness images work: an image that ships only pi's
 * SDK must never evaluate the claude-code module (whose top-level import of
 * the Anthropic SDK would throw MODULE_NOT_FOUND). Only the selected
 * harness's module — and therefore only its SDK — is ever loaded.
 */
const LOADERS = Object.freeze({
  "claude-code": () => import("./claude-code.mjs").then((m) => m.claudeCode),
  codex: () => import("./codex.mjs").then((m) => m.codex),
  pi: () => import("./pi.mjs").then((m) => m.pi),
});

/** Select and load the harness for this actor; defaults to claude-code. */
export async function selectHarness(name) {
  const load = LOADERS[name || "claude-code"];
  if (!load) throw new Error(`unknown harness ${name} (known: ${Object.keys(LOADERS).join(", ")})`);
  return load();
}
