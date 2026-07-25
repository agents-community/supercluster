// codex harness — the Codex SDK, which spawns the `codex` CLI PER TURN.
//
// Unlike claude-code's persistent stream, continuity here is the thread id
// (persisted via ctx.setSessionId) plus the rollout files under ~/.codex on the
// actor fs — both of which our checkpoints carry. So each turn resumes the
// thread rather than holding it in memory.

export const codex = {
  name: "codex",

  // AgentSpec → Codex ThreadOptions. Codex has sandbox MODES, not tool
  // allow-lists, so we translate: no allow → read-only; any allow → full
  // access (the gVisor actor, not codex's own landlock, is the real boundary).
  optionsFromSpec(spec, workdir) {
    return {
      workingDirectory: workdir,
      skipGitRepoCheck: true,
      sandboxMode: Array.isArray(spec.allow) && spec.allow.length ? "danger-full-access" : "read-only",
      approvalPolicy: "never",
      ...(spec.model ? { model: spec.model } : {}),
    };
  },

  async *run(inputs, ctx) {
    const { Codex } = await import("@openai/codex-sdk");
    // The SDK forwards auth to the spawned CLI ONLY via options.apiKey (as
    // CODEX_API_KEY) — a plain OPENAI_API_KEY in our env is NOT picked up.
    // BYO-key (ctx.apiKey) overrides the image's shared env key per session.
    const codexClient = new Codex({ apiKey: ctx.apiKey || process.env.OPENAI_API_KEY });
    const threadOpts = this.optionsFromSpec(ctx.spec, ctx.workdir);
    for await (const inp of inputs) {
      // v0 watchdog limitation: codex has no active interrupt, so the deadline
      // is enforced only between stream events (a single hung event waits for
      // process teardown). Matches the aborts the runtime signals.
      if (ctx.signal.aborted) throw new Error("TURN_DEADLINE_EXCEEDED");
      const thread = ctx.sessionId ? codexClient.resumeThread(ctx.sessionId, threadOpts) : codexClient.startThread(threadOpts);
      const { events } = await thread.runStreamed(inp.text);
      for await (const ev of events) {
        if (ctx.signal.aborted) throw new Error("TURN_DEADLINE_EXCEEDED");
        if (ev.type === "item.completed") {
          const it = ev.item;
          if (it.type === "agent_message" && it.text) {
            yield { type: "agent.message", content: [{ type: "text", text: it.text }] };
          } else if (it.type === "command_execution") {
            yield { type: "agent.tool_use", name: "shell", input: { command: it.command } };
          } else if (it.type === "mcp_tool_call") {
            yield { type: "agent.tool_use", name: `mcp__${it.server}__${it.tool}`, input: it.arguments };
          } else if (it.type === "file_change") {
            yield { type: "agent.tool_use", name: "apply_patch", input: { changes: it.changes } };
          }
        } else if (ev.type === "turn.completed") {
          yield {
            type: "session.status_idle",
            stop_reason: { type: "end_turn" },
            usage: { input_tokens: ev.usage?.input_tokens ?? null, output_tokens: ev.usage?.output_tokens ?? null },
          };
        } else if (ev.type === "turn.failed") {
          throw new Error(ev.error?.message || "codex turn failed");
        }
      }
      if (thread.id && thread.id !== ctx.sessionId) ctx.setSessionId(thread.id);
    }
  },
};
