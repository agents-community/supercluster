// pi harness — mitsuhiko's model-agnostic coding agent (@earendil-works/pi).
//
// Why pi matters here: one harness, ANY model provider (anthropic, openai,
// google, bedrock, …) — the AgentSpec's `model: "provider/model-id"` picks the
// vendor, and the compiler wires the matching API-key env (pi-ai resolves
// ANTHROPIC_API_KEY / OPENAI_API_KEY / GEMINI_API_KEY from the environment).
//
// Continuity: pi persists the conversation as a session JSONL under
// ~/.pi/agent/sessions on the actor fs (checkpointed). The session FILE PATH is
// our resume token (ctx.sessionId): SessionManager.open(path) rejoins it after
// a process restart, exactly like claude-code's resume-by-id.
//
// Interruption: session.abort() actively cancels an in-flight turn, so the
// runtime's watchdog signal maps to a REAL interrupt (like claude-code, unlike
// codex's passive between-events check).

// Translate our AgentSpec tool names (claude-style) to pi's built-ins.
const PI_TOOL_NAMES = { bash: "bash", read: "read", write: "write", edit: "edit", grep: "grep", glob: "find", find: "find", ls: "ls" };

function piTools(allow) {
  const out = [];
  for (const a of allow || []) {
    const key = String(a).toLowerCase().replace(/\(.*\)$/, ""); // "Bash(ls*)" → "bash"
    const t = PI_TOOL_NAMES[key];
    if (t && !out.includes(t)) out.push(t);
  }
  return out;
}

export const pi = {
  name: "pi",

  // AgentSpec → createAgentSession options (model resolved separately in run()).
  optionsFromSpec(spec, workdir) {
    const opts = { cwd: workdir };
    const tools = piTools(spec.allow);
    if (tools.length) opts.tools = tools;
    else opts.noTools = "all"; // platform policy: empty allow = chat-only
    const deny = piTools(spec.deny);
    if (deny.length) opts.excludeTools = deny;
    return opts;
  },

  async *run(inputs, ctx) {
    const { createAgentSession, SessionManager, DefaultResourceLoader, ModelRuntime, getAgentDir } =
      await import("@earendil-works/pi-coding-agent");

    if (ctx.spec.mcp && Object.keys(ctx.spec.mcp).length) {
      console.error("pi harness: spec.mcp is not supported yet (pi uses extensions) — ignoring");
    }

    const modelRuntime = await ModelRuntime.create();
    let model, provider = "anthropic";
    if (ctx.spec.model) {
      const rest = String(ctx.spec.model).split("/");
      provider = rest.shift();
      model = modelRuntime.getModel(provider, rest.join("/"));
      if (!model) throw new Error(`pi: unknown model ${ctx.spec.model} (want "provider/model-id")`);
    }
    // BYO-key: pi's first-class runtime override (documented "not persisted").
    if (ctx.apiKey) modelRuntime.setRuntimeApiKey(provider, ctx.apiKey);

    let resourceLoader;
    if (ctx.spec.systemPrompt) {
      // cwd + agentDir are REQUIRED (the SDK docs' minimal example omits them
      // and crashes in resolvePath — verified against 0.81.1).
      resourceLoader = new DefaultResourceLoader({
        cwd: ctx.workdir,
        agentDir: getAgentDir(),
        systemPrompt: ctx.spec.systemPrompt,
      });
      await resourceLoader.reload();
    }

    // Resume by session file when we have one; otherwise create fresh.
    const sessionManager = ctx.sessionId ? SessionManager.open(ctx.sessionId) : SessionManager.create(ctx.workdir);
    const { session } = await createAgentSession({
      ...this.optionsFromSpec(ctx.spec, ctx.workdir),
      sessionManager,
      modelRuntime,
      ...(model ? { model } : {}),
      ...(resourceLoader ? { resourceLoader } : {}),
    });
    if (session.sessionFile && session.sessionFile !== ctx.sessionId) ctx.setSessionId(session.sessionFile);

    const onAbort = () => { session.abort().catch(() => { /* best-effort */ }); };
    ctx.signal.addEventListener("abort", onAbort);
    try {
      for await (const inp of inputs) {
        // Bridge pi's sync event listener into this async generator: the
        // listener queues normalized events; the drain loop below yields them
        // as they arrive and finishes when prompt() settles (= turn end).
        const pending = [];
        let notify = null;
        let text = "";
        const unsubscribe = session.subscribe((ev) => {
          if (ev.type === "message_update" && ev.assistantMessageEvent?.type === "text_delta") {
            text += ev.assistantMessageEvent.delta;
          } else if (ev.type === "message_end") {
            if (text.trim()) pending.push({ type: "agent.message", content: [{ type: "text", text }] });
            text = "";
            notify?.();
          } else if (ev.type === "tool_execution_start") {
            pending.push({ type: "agent.tool_use", name: ev.toolName ?? "tool", input: ev.args ?? {} });
            notify?.();
          }
        });
        const turn = session.prompt(inp.text).then(() => null).catch((e) => e ?? new Error("pi turn failed"));
        try {
          for (;;) {
            while (pending.length) yield pending.shift();
            const settled = await Promise.race([turn.then(() => true), new Promise((r) => { notify = () => r(false); })]);
            notify = null;
            if (settled) break;
          }
          while (pending.length) yield pending.shift();
          if (text.trim()) yield { type: "agent.message", content: [{ type: "text", text }] };
          const err = await turn;
          if (ctx.signal.aborted) throw new Error("TURN_DEADLINE_EXCEEDED");
          if (err) throw err;
        } finally {
          unsubscribe();
        }
        yield { type: "session.status_idle", stop_reason: { type: "end_turn" }, usage: {} };
      }
    } finally {
      ctx.signal.removeEventListener("abort", onAbort);
      session.dispose?.();
    }
  },
};
