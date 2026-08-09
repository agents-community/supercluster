// The approval gate (#67).
//
// Two properties carry the whole feature, and both fail silently if broken:
// a gated tool must not run before a human says yes, and a grant must cover
// exactly the call that was shown — not the tool in general.
//
// See runtime.test.mjs for the dependency install this needs.

import { test, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createRuntime } from "./runtime.mjs";
import { claudeCode } from "./harness/claude-code.mjs";
import { needsApproval, approvalRequestId } from "./approval.mjs";

function newRuntime(spec, workdir = mkdtempSync(join(tmpdir(), "approval-"))) {
  return {
    workdir,
    rt: createRuntime({
      workdir, spec, identity: () => "sess-test",
      harness: { name: "stub", async *run() {} },
    }),
  };
}

// ---- which tools the list matches -----------------------------------------

test("ask matches bare names and federated server/tool", () => {
  const ask = ["Bash", "github/create_issue", "docs/*"];
  // bare
  assert.equal(needsApproval("Bash", ask), true);
  assert.equal(needsApproval("Read", ask), false);
  // bare name served through the hand: `ask:[Bash]` must gate `mcp__hand__bash`
  // (case-insensitive) — the regression that let hand-backed bash run unapproved
  assert.equal(needsApproval("mcp__hand__bash", ask), true);
  assert.equal(needsApproval("mcp__hand__read", ask), false);
  // federated through a hand — the name the model actually sees
  assert.equal(needsApproval("mcp__hand__github__create_issue", ask), true);
  assert.equal(needsApproval("mcp__hand__github__list_issues", ask), false);
  // and without a hand, where servers connect straight to the brain
  assert.equal(needsApproval("mcp__github__create_issue", ask), true);
  // whole-server wildcard
  assert.equal(needsApproval("mcp__hand__docs__search", ask), true);
  assert.equal(needsApproval("mcp__hand__other__search", ask), false);
});

test("an empty ask list gates nothing", () => {
  for (const ask of [undefined, null, [], ["", null]]) {
    assert.equal(needsApproval("Bash", ask), false);
  }
});

// ---- the request id is per-CALL, not per-tool ------------------------------

test("approving one call does not approve a different one", () => {
  const a = approvalRequestId("Bash", { command: "rm -rf build" });
  const b = approvalRequestId("Bash", { command: "rm -rf /" });
  assert.notEqual(a, b, "different inputs must not share an approval id");
  assert.equal(a, approvalRequestId("Bash", { command: "rm -rf build" }), "same call must be stable");
  assert.match(a, /^apr_[0-9a-f]{16}$/);
});

// ---- the gate itself -------------------------------------------------------

const hookInput = (tool, input) => ({ tool_name: tool, tool_input: input });

test("a gated tool is denied until approved, then allowed exactly once", async () => {
  const { rt } = newRuntime({ ask: ["Bash"] });
  const ctx = {
    askList: ["Bash"],
    approvalState: (r) => rt.approvalState(r),
    useApproval: (r) => rt.useApproval(r),
    requestApproval: (r, t, i) => rt.requestApproval(r, t, i),
  };
  const hook = claudeCode.approvalHook(ctx);
  const call = hookInput("Bash", { command: "npm publish" });
  const req = approvalRequestId("Bash", { command: "npm publish" });

  // 1. first attempt: denied, request raised
  let out = await hook(call);
  assert.equal(out.hookSpecificOutput.permissionDecision, "deny");
  assert.match(out.hookSpecificOutput.permissionDecisionReason, new RegExp(req));
  assert.deepEqual(rt.pendingApprovals().map((a) => a.request), [req]);

  // 2. retrying before an answer must NOT slip through, and must not pile up
  out = await hook(call);
  assert.equal(out.hookSpecificOutput.permissionDecision, "deny");
  assert.equal(rt.pendingApprovals().length, 1, "a retry raised a duplicate request");

  // 3. human approves
  assert.deepEqual(rt.resolveApproval(req, true), { ok: true, decision: "granted" });
  assert.equal(rt.pendingApprovals().length, 0);

  // 4. now it runs
  out = await hook(call);
  assert.equal(out.hookSpecificOutput.permissionDecision, "allow");

  // 5. and the grant is SPENT — the same call asks again
  out = await hook(call);
  assert.equal(out.hookSpecificOutput.permissionDecision, "deny",
    "a one-shot grant was reusable — approving once would approve forever");
});

test("an ungated tool is never touched by the hook", async () => {
  const ctx = { askList: ["Bash"], approvalState: () => "none", useApproval: () => {}, requestApproval: () => { throw new Error("must not request"); } };
  const out = await claudeCode.approvalHook(ctx)(hookInput("Read", { file_path: "/x" }));
  assert.deepEqual(out, { continue: true });
  assert.equal(out.hookSpecificOutput, undefined);
});

// ---- answering -------------------------------------------------------------

test("denying records it and does not leave the request open", () => {
  const { rt } = newRuntime({ ask: ["Bash"] });
  const req = rt.requestApproval(approvalRequestId("Bash", { command: "x" }), "Bash", { command: "x" });
  assert.equal(rt.pendingApprovals().length, 1);
  assert.deepEqual(rt.resolveApproval(req, false, "not this one"), { ok: true, decision: "denied" });
  assert.equal(rt.pendingApprovals().length, 0);
  assert.equal(rt.approvalState(req), "none", "a denial must not leave a grant behind");
});

test("answering an unknown or already-answered request fails rather than silently succeeding", () => {
  const { rt } = newRuntime({ ask: ["Bash"] });
  assert.equal(rt.resolveApproval("apr_nope", true).ok, false);

  const req = rt.requestApproval("apr_dup", "Bash", {});
  assert.equal(rt.resolveApproval(req, true).ok, true);
  const second = rt.resolveApproval(req, true);
  assert.equal(second.ok, false, "a double answer reported success twice");
  assert.match(second.reason, /already granted/);
});

// ---- durability: the fold ---------------------------------------------------

test("state is rebuilt from the event log after a cold start", () => {
  const workdir = mkdtempSync(join(tmpdir(), "approval-cold-"));
  const log = [
    { id: "evt_000001", type: "tool.approval_requested", request: "apr_open", tool: "Bash", input: {} },
    { id: "evt_000002", type: "tool.approval_requested", request: "apr_granted", tool: "Bash", input: {} },
    { id: "evt_000003", type: "tool.approval_granted", request: "apr_granted" },
    { id: "evt_000004", type: "tool.approval_requested", request: "apr_spent", tool: "Bash", input: {} },
    { id: "evt_000005", type: "tool.approval_granted", request: "apr_spent" },
    { id: "evt_000006", type: "tool.approval_used", request: "apr_spent" },
    { id: "evt_000007", type: "tool.approval_requested", request: "apr_denied", tool: "Bash", input: {} },
    { id: "evt_000008", type: "tool.approval_denied", request: "apr_denied" },
  ];
  writeFileSync(join(workdir, "events.jsonl"), log.map((e) => JSON.stringify(e)).join("\n") + "\n");

  const { rt } = newRuntime({ ask: ["Bash"] }, workdir);
  assert.equal(rt.approvalState("apr_granted"), "granted", "a grant was forgotten on cold start");
  assert.equal(rt.approvalState("apr_open"), "open");
  assert.equal(rt.approvalState("apr_denied"), "none");
  assert.equal(rt.approvalState("apr_spent"), "none",
    "a spent one-shot grant came back to life after a restart");
  assert.deepEqual(rt.pendingApprovals().map((a) => a.request), ["apr_open"]);
});

test("a large tool input is capped in the log", () => {
  const { rt } = newRuntime({ ask: ["Write"] });
  const req = rt.requestApproval("apr_big", "Write", { content: "x".repeat(50_000) });
  const [pending] = rt.pendingApprovals();
  assert.equal(pending.request, req);
  assert.equal(pending.input.truncated, true, "a 50KB input went into the log whole");
  assert.ok(pending.input.preview.length <= 4000);
});

// The runtime's drive loop and watchdog keep the event loop alive, so the
// process needs a push. It MUST carry the runner's exit code: a bare
// process.exit(0) here silently turned every failing assertion into a pass.
after(() => process.exit(process.exitCode ?? 0));
