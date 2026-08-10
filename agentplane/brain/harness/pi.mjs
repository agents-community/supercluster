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

import { approvalRequestId } from "../approval.mjs";
import { handURL } from "../identity.mjs";

// Translate our AgentSpec tool names (claude-style) to pi's built-ins.
// AgentSpec tool names -> the HAND's tool names. The hand publishes bash,
// write, read, list, grep; federated upstream tools arrive as <upstream>__<tool>
// and are matched by their own name.
const HAND_TOOL_NAMES = { bash: "bash", read: "read", write: "write", edit: "write", grep: "grep", ls: "list", list: "list", glob: "list" };

function handNames(specNames) {
  const out = [];
  for (const a of specNames || []) {
    const key = String(a).toLowerCase().replace(/\(.*\)$/, "");
    const t = HAND_TOOL_NAMES[key] ?? (key.includes("/") ? key.replace("/", "__") : null);
    if (t && !out.includes(t)) out.push(t);
  }
  return out;
}

const PI_TOOL_NAMES = { bash: "bash", read: "read", write: "write", edit: "edit", grep: "grep", glob: "find", find: "find", ls: "ls" };

// pi exports a definition factory per built-in; a gated tool wraps one of these.
// Keyed by pi's own tool name so it lines up with piTools() output.
const PI_DEFINITION_FACTORIES = {
  bash: "createBashToolDefinition", read: "createReadToolDefinition",
  write: "createWriteToolDefinition", edit: "createEditToolDefinition",
  grep: "createGrepToolDefinition", find: "createFindToolDefinition",
  ls: "createLsToolDefinition",
};

function piTools(allow) {
  const out = [];
  for (const a of allow || []) {
    const key = String(a).toLowerCase().replace(/\(.*\)$/, ""); // "Bash(ls*)" → "bash"
    const t = PI_TOOL_NAMES[key];
    if (t && !out.includes(t)) out.push(t);
  }
  return out;
}

// Gate a pi tool behind human approval (#67).
//
// pi has no permission hook — CreateAgentSessionOptions offers `tools`,
// `excludeTools` and `noTools`, all static at session creation, and
// `tool_execution_start` is a NOTIFICATION with no return value, fired once the
// tool is already running. The one bash-specific hook, BashSpawnHook, returns a
// rewritten context rather than a decision, so it cannot refuse either.
//
// What pi does give us is better than a hook: tools are ordinary values built
// by exported factories, and `customTools` accepts our own. So we take the real
// tool definition and replace its execute with one that checks the gate first.
// It works for every tool rather than just bash, and the denial is returned as
// the tool's own result.
//
// `terminate: true` is what makes this match the claude-code behaviour: pi
// documents it as "the agent should stop after the current tool batch", so the
// turn ends cleanly instead of the model looping on a tool it cannot run.
export function gateToolDefinition(def, ctx, toolName, approvalRequestId) {
  return {
    ...def,
    async execute(toolCallId, params, signal, onUpdate, ectx) {
      const req = approvalRequestId(toolName, params);
      if (ctx.approvalState(req) === "granted") {
        // Spend before running: one-shot, so an identical call later asks again
        // rather than riding on a decision the human already spent.
        ctx.useApproval(req);
        return def.execute(toolCallId, params, signal, onUpdate, ectx);
      }
      ctx.requestApproval(req, toolName, params);
      return {
        content: [{
          type: "text",
          text: `This call needs human approval (request ${req}) and is not approved yet. ` +
                `The request has been raised — do NOT retry it this turn. Tell the human ` +
                `exactly what you were about to do and why, then stop.`,
        }],
        details: { approvalRequest: req, gated: true },
        terminate: true,
      };
    },
  };
}

// Turn the hand's MCP tools into pi tools (#73).
//
// Without this, pi runs its OWN bash/read/write inside the brain actor — the
// same process that holds the user's model key — so the brain/hand split the
// platform advertises does not hold for pi, and `credentials:`/`mcp:` never
// reach it. The model key stays in the brain either way (the brain is what
// calls the provider); what changes is that model-authored commands stop
// running next to it.
//
// The same customTools lever the approval gate uses, so the two compose: a
// hand-backed tool wrapped by gateToolDefinition gives pi the approval flow.
async function handToolDefinitions(ctx) {
  const url = handURL();
  if (!url) return null;
  const [{ Client }, { StreamableHTTPClientTransport }] = await Promise.all([
    import("@modelcontextprotocol/sdk/client/index.js"),
    import("@modelcontextprotocol/sdk/client/streamableHttp.js"),
  ]);
  const client = new Client({ name: "agentplane-pi", version: "1.0.0" }, { capabilities: {} });
  // Pass the agent's MCP upstreams to the broker as config — the broker (not the
  // brain) is the MCP client to them, and returns their tools in listTools below.
  const headers = {};
  if (ctx.spec.mcp && Object.keys(ctx.spec.mcp).length) {
    headers["x-agentplane-mcp"] = Buffer.from(JSON.stringify(ctx.spec.mcp)).toString("base64");
  }
  await client.connect(new StreamableHTTPClientTransport(
    new URL(url), Object.keys(headers).length ? { requestInit: { headers } } : undefined));
  const { tools } = await client.listTools();

  const allow = handNames(ctx.spec.allow);
  const deny = handNames(ctx.spec.deny);
  const askHand = handNames(ctx.askList);
  const defs = [];
  for (const t of tools) {
    if (allow.length && !allow.includes(t.name)) continue;
    if (deny.includes(t.name)) continue;
    const def = {
      name: t.name,
      label: t.name,
      description: t.description ?? "",
      // MCP inputSchema is JSON Schema, which is what a TypeBox TSchema is at
      // runtime — pi validates against it directly.
      parameters: t.inputSchema ?? { type: "object", properties: {} },
      async execute(_toolCallId, params) {
        const r = await client.callTool({ name: t.name, arguments: params ?? {} });
        return {
          content: r.content ?? [{ type: "text", text: "" }],
          details: { hand: true, tool: t.name },
        };
      },
    };
    // `ask` must be TRANSLATED like allow/deny, not matched raw. A spec says
    // `ask: [Bash]` (claude vocabulary) while the hand's tool is `bash`, and
    // needsApproval compares exactly — so the untranslated form matched
    // nothing and the gate silently failed OPEN, letting bash run unapproved.
    defs.push(askHand.includes(t.name)
      ? gateToolDefinition(def, ctx, t.name, approvalRequestId)
      : def);
  }
  return { client, defs };
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
    const pkg = await import("@earendil-works/pi-coding-agent");
    const { createAgentSession, SessionManager, DefaultResourceLoader, ModelRuntime, getAgentDir } = pkg;

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
    // Tools come from the HAND when there is one (#73): pi's own bash/read/write
    // execute inside the brain actor, which is where the model key lives, so a
    // split agent must not use them. noTools:"all" removes them outright and the
    // hand's tools are registered in their place under their own names.
    //
    // Falling back to pi's local tools when there is no hand is deliberate: a
    // `hand: false` pi agent is a single-actor agent by declaration, and
    // silently having no tools would look like a broken model.
    const base = this.optionsFromSpec(ctx.spec, ctx.workdir);
    let toolOpts = base, handClient = null;
    try {
      const bridged = await handToolDefinitions(ctx);
      if (bridged && bridged.defs.length) {
        handClient = bridged.client;
        // `tools` is the allowlist, and customTools supplies the definitions
        // for those names. NOT noTools:"all" — that suppressed the custom
        // tools as well, so pi ran with none and the model answered by
        // printing the command it would have run in a markdown block.
        toolOpts = { ...base, tools: bridged.defs.map((d) => d.name),
                     excludeTools: undefined, customTools: bridged.defs };
        console.log(`pi: ${bridged.defs.length} tool(s) via the hand`);
      } else if (bridged) {
        console.error("pi: the hand exposed no tools matching allow/deny — running tool-less");
        toolOpts = { ...base, noTools: "all", tools: undefined, excludeTools: undefined };
      }
    } catch (e) {
      // Fail CLOSED on tools: running pi's local bash because the hand was
      // unreachable would silently execute in the brain, which is the exact
      // thing this change exists to prevent.
      console.error(`pi: cannot reach the hand (${e.message}) — running tool-less this turn`);
      toolOpts = { ...base, noTools: "all", tools: undefined, excludeTools: undefined };
    }

    const sessionManager = ctx.sessionId ? SessionManager.open(ctx.sessionId) : SessionManager.create(ctx.workdir);
    const { session } = await createAgentSession({
      ...toolOpts,
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
