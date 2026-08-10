// Approval-gate helpers shared by every harness (#67).
//
// Deliberately its own module with NO vendor imports. These first lived in the
// claude-code harness, and importing them from the pi harness pulled
// @anthropic-ai/claude-agent-sdk into the pi image — which does not install it,
// so the pi harness would have failed to load at runtime rather than at build.
//
// Anything here must stay pure and dependency-free for that reason.

import { createHash } from "node:crypto";

// Does this tool need a human yes? Entries are matched two ways so the `ask`
// list uses the same vocabulary as `allow`/`deny`: a bare tool name (`Bash`),
// or a federated `server/tool` pair, which reaches the model as
// `mcp__hand__<server>__<tool>` when the agent has a hand.
export function needsApproval(tool, askList) {
  if (!Array.isArray(askList) || askList.length === 0) return false;
  for (const entry of askList) {
    if (typeof entry !== "string" || !entry) continue;
    if (entry === tool) return true;
    // A bare tool name (`Bash`) must also match the SAME tool served through the
    // hand, which reaches the model as `mcp__hand__bash`. Without this the gate
    // fails OPEN for hand-backed agents: `ask: [Bash]` matched the harness's own
    // `Bash` but never `mcp__hand__bash`, so bash ran unapproved. Case-insensitive
    // so the `ask` vocabulary (`Bash`) lines up with the hand tool name (`bash`).
    if (!entry.includes("/") && tool.toLowerCase() === `mcp__hand__${entry.toLowerCase()}`) return true;
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
