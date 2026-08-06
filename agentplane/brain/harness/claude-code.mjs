// claude-code harness — a single PERSISTENT Agent SDK session per actor.
//
// The conversation lives in the SDK stream's process memory across turns (this
// is what Substrate checkpoints for ~1s warm recall); the SDK also persists a
// resumable session id, so a restarted process rejoins the exact conversation.

import { query } from "@anthropic-ai/claude-agent-sdk";
import { handURL } from "../identity.mjs";
import { createHash } from "node:crypto";

function textFromContent(content) {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content.filter((b) => b.type === "text").map((b) => b.text).join("");
}

// Does this tool need a human yes? Entries are matched two ways so the `ask`
// list uses the same vocabulary as `allow`/`deny`: a bare tool name (`Bash`),
// or a federated `server/tool` pair, which reaches the model as
// `mcp__hand__<server>__<tool>` when the agent has a hand.
export function needsApproval(tool, askList) {
  if (!Array.isArray(askList) || askList.length === 0) return false;
  for (const entry of askList) {
    if (typeof entry !== "string" || !entry) continue;
    if (entry === tool) return true;
    const [server, name] = entry.split("/");
    if (!server || !name) continue;
    if (name === "*") {
      if (tool.startsWith(`mcp__hand__${server}__`) || tool.startsWith(`mcp__${server}__`)) return true;
      continue;
    }
    if (tool === `mcp__hand__${server}__${name}` || tool === `mcp__${server}__${name}`) return true;
  }
  return false;
}

// A stable id for "this exact call". Derived from the tool AND its input, so
// approving `rm -rf build` does not also approve `rm -rf /` — the human is
// deciding about the call they were shown, not about the tool in general.
//
// Hashed rather than sent raw because the id travels in URLs; the readable
// tool name and input ride alongside it in the event.
export function approvalRequestId(tool, input) {
  let body;
  try { body = JSON.stringify(input ?? {}); } catch { body = String(input); }
  return "apr_" + createHash("sha256").update(`${tool}\u0000${body}`).digest("hex").slice(0, 16);
}

// Adapt the runtime's {text} inputs into the shape the SDK's prompt expects.
async function* asUserMessages(inputs) {
  for await (const inp of inputs) {
    yield { type: "user", message: { role: "user", content: inp.text } };
  }
}

export const claudeCode = {
  name: "claude-code",

  // AgentSpec → Agent SDK options. Pure; unit-testable in isolation.
  // resolvedDisallow comes from serve (#58) and is UNIONED with the spec's own
  // deny list — never substituted for it. A tool the user disabled stays
  // disabled regardless of what the control plane sends.
  optionsFromSpec(spec, resumeId, workdir, resolvedDisallow = []) {
    const opts = {
      cwd: workdir,
      permissionMode: "dontAsk",
      includePartialMessages: true, // stream text deltas for a live-typing UI
      // Tool policy is USER-controlled via allow/deny (the platform imposes
      // none). Safe by default: only allow-listed tools are auto-approved,
      // everything else is denied headless. gVisor is the safety boundary.
      disallowedTools: [],
      allowedTools: [],
    };
    if (spec.systemPrompt) opts.systemPrompt = spec.systemPrompt;
    if (spec.model) opts.model = spec.model;

    const hu = handURL(); // D3: pair the hand lazily from CURRENT identity
    // Hand-as-gateway: when paired to a hand, the brain connects to ONE door —
    // the hand — and the user's own MCP servers are federated THROUGH it (serve
    // injects them into the hand, with credentials, at session create). So the
    // brain holds no upstream URLs or credentials; every tool is mcp__hand__*.
    // Without a hand, connect the user's servers directly from the brain.
    const mcp = hu ? { hand: { url: hu } } : { ...(spec.mcp || {}) };
    if (Object.keys(mcp).length > 0) {
      opts.mcpServers = {};
      for (const [name, cfg] of Object.entries(mcp)) {
        opts.mcpServers[name] = { type: "http", url: cfg.url, ...(cfg.headers ? { headers: cfg.headers } : {}) };
        opts.allowedTools.push(`mcp__${name}__*`);
      }
    }
    if (Array.isArray(spec.allow)) opts.allowedTools.push(...spec.allow);
    if (Array.isArray(spec.deny)) opts.disallowedTools.push(...spec.deny);
    // Serve's additions land last and can only add. Deduped so a tool named in
    // both does not appear twice.
    if (Array.isArray(resolvedDisallow) && resolvedDisallow.length) {
      opts.disallowedTools = [...new Set([...opts.disallowedTools, ...resolvedDisallow])];
    }
    if (resumeId) opts.resume = resumeId;
    return opts;
  },
  // Gate the tools an agent's `ask:` list names behind a human decision (#67).
  //
  // PreToolUse rather than canUseTool, for two reasons that both matter:
  //
  //   * canUseTool would have to AWAIT the human, holding the turn open. The
  //     watchdog then fires at turnDeadlineSeconds, aborts and restarts the
  //     harness — so an unanswered approval would not merely time out, it would
  //     destroy the turn and replay the user's message.
  //   * Under `permissionMode: "dontAsk"` the SDK denies anything not
  //     pre-approved as a short-circuit, and documents that PreToolUse denies
  //     bypass canUseTool. PreToolUse is the hook that reliably runs here.
  //
  // Denying returns immediately, so the model reads the reason, reports what it
  // wanted to do, and the turn ENDS. The actor suspends; waiting costs nothing.
  approvalHook(ctx) {
    return async (input) => {
      const tool = input?.tool_name;
      if (!tool || !needsApproval(tool, ctx.askList)) return { continue: true };

      const req = approvalRequestId(tool, input.tool_input);
      if (ctx.approvalState(req) === "granted") {
        // Spend it BEFORE running: one-shot, so an identical call later asks
        // again rather than riding on a decision the human already spent.
        ctx.useApproval(req);
        return {
          continue: true,
          hookSpecificOutput: {
            hookEventName: "PreToolUse",
            permissionDecision: "allow",
            permissionDecisionReason: `human approved ${req}`,
          },
        };
      }

      ctx.requestApproval(req, tool, input.tool_input);
      return {
        continue: true,
        hookSpecificOutput: {
          hookEventName: "PreToolUse",
          permissionDecision: "deny",
          permissionDecisionReason:
            `This call needs human approval (request ${req}) and is not approved yet. ` +
            `The request has been raised — do NOT retry it this turn. Finish by telling ` +
            `the human exactly what you were about to do and why, then stop.`,
        },
      };
    };
  },

  async *run(inputs, ctx) {
    // BYO-key: the SDK reads ANTHROPIC_API_KEY from the environment. Setting it
    // here (single-session actor) applies the per-session key for this turn;
    // absent, the image's shared env key stays in effect.
    if (ctx.apiKey) process.env.ANTHROPIC_API_KEY = ctx.apiKey;
    const options = this.optionsFromSpec(ctx.spec, ctx.sessionId, ctx.workdir, ctx.resolvedDisallow);
    // Registered here rather than in optionsFromSpec so that stays pure and
    // unit-testable: the hook closes over the live runtime ctx.
    if (Array.isArray(ctx.askList) && ctx.askList.length) {
      options.hooks = { ...(options.hooks || {}), PreToolUse: [{ hooks: [this.approvalHook(ctx)] }] };
    }
    const q = query({ prompt: asUserMessages(inputs), options });
    // The watchdog's lever: interrupting the live SDK query tears down a wedged
    // in-flight turn (finding #1). The runtime aborts the signal on deadline.
    const onAbort = () => { try { q.interrupt?.(); } catch { /* best-effort */ } };
    ctx.signal.addEventListener("abort", onAbort);
    // The SDK connects to MCP servers ONCE, at init. If the hand was down at
    // that moment (wake race, pool exhaustion), this session would otherwise be
    // tool-less FOREVER — the durable mind preserves the failed connect. Detect
    // it and self-heal: finish the turn, then end the stream so the runtime's
    // restart (resume-by-id) reconnects on the next turn.
    const handConfigured = !!options.mcpServers?.hand;
    let handDown = false;
    try {
      for await (const msg of q) {
        if (msg.type === "system" && msg.subtype === "init") {
          if (msg.session_id && msg.session_id !== ctx.sessionId) ctx.setSessionId(msg.session_id);
          if (handConfigured) {
            const hs = (msg.mcp_servers || []).find((s) => s.name === "hand");
            handDown = !hs || hs.status !== "connected";
            if (handDown) console.error("hand MCP not connected at init:", JSON.stringify(hs ?? "absent"));
          }
          continue;
        }
        // Live text deltas (includePartialMessages). Ephemeral — the runtime
        // streams these to viewers but does NOT persist them; the complete
        // `assistant` message below is the durable one.
        if (msg.type === "stream_event") {
          const e = msg.event;
          if (e?.type === "content_block_delta" && e.delta?.type === "text_delta" && e.delta.text) {
            yield { type: "agent.message_delta", text: e.delta.text };
          }
          continue;
        }
        if (msg.type === "assistant") {
          const content = msg.message?.content || [];
          for (const block of Array.isArray(content) ? content : []) {
            if (block.type === "tool_use") yield { type: "agent.tool_use", name: block.name, input: block.input };
          }
          const text = textFromContent(content);
          if (text) yield { type: "agent.message", content: [{ type: "text", text }] };
          continue;
        }
        if (msg.type === "result") {
          // The SDK does the price math (total_cost_usd) — we only report it.
          const u = msg.usage ?? {};
          yield {
            type: "session.status_idle",
            stop_reason: { type: msg.subtype === "success" ? "end_turn" : "error" },
            usage: {
              cost_usd: msg.total_cost_usd ?? null,
              turns: msg.num_turns ?? null,
              input_tokens: u.input_tokens ?? null,
              output_tokens: u.output_tokens ?? null,
              cache_read_tokens: u.cache_read_input_tokens ?? null,
              cache_creation_tokens: u.cache_creation_input_tokens ?? null,
            },
          };
          if (handDown) {
            console.error("restarting harness to reconnect the hand (turn completed cleanly)");
            return; // stream end → runtime restarts us (resume-by-id) → fresh MCP connect
          }
          continue;
        }
      }
    } finally {
      ctx.signal.removeEventListener("abort", onAbort);
    }
  },
};
