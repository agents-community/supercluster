// Tool policy pushed by serve (#58) must be a one-way ratchet, and must still
// be in force when the harness restarts.
//
// The restart case is the one the issue called out as needing a test rather
// than an assumption: the brain self-heals by ending the harness stream and
// starting it again (resume-by-id). If the second start rebuilt its options
// from the static spec alone, the session would silently lose its restrictions
// on the next turn — the worst shape of failure, because the first turn would
// look correct.
//
// The brain has no package.json — its dependencies are installed in the
// Dockerfile — so running this outside the image needs the ones runtime.mjs
// reaches through otel.mjs:
//
//   cd agentplane/brain
//   npm install '@opentelemetry/api@^1' @opentelemetry/sdk-node \
//               @opentelemetry/exporter-trace-otlp-grpc
//   node --test .

import { test, after } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { createRuntime } from "./runtime.mjs";

function newRuntime(harness) {
  return createRuntime({
    workdir: mkdtempSync(join(tmpdir(), "brain-test-")),
    spec: { harness: "stub", disallowedTools: ["SpecDenied"] },
    harness,
    identity: () => "sess-test",
  });
}

const inert = { name: "stub", async *run() {} };

test("setResolvedOptions unions and never replaces", () => {
  const rt = newRuntime(inert);

  let r = rt.setResolvedOptions({ disallowedTools: ["mcp__hand__github__delete_repo"] });
  assert.equal(r.disallowedTools, 1);
  assert.equal(r.added, 1);

  // A second push must ADD. Replacing would let a later, narrower push silently
  // re-enable a tool an earlier one took away.
  r = rt.setResolvedOptions({ disallowedTools: ["mcp__hand__github__create_issue"] });
  assert.equal(r.disallowedTools, 2);
  assert.equal(r.added, 1);

  // Re-pushing something already denied is a no-op, not a duplicate.
  r = rt.setResolvedOptions({ disallowedTools: ["mcp__hand__github__delete_repo"] });
  assert.equal(r.disallowedTools, 2);
  assert.equal(r.added, 0);
});

test("a malformed push cannot clear an existing restriction", () => {
  const rt = newRuntime(inert);
  rt.setResolvedOptions({ disallowedTools: ["Real"] });
  for (const junk of [undefined, null, [], [""], [null, 42, {}]]) {
    rt.setResolvedOptions({ disallowedTools: junk });
  }
  const { disallowedTools } = rt.setResolvedOptions({ disallowedTools: [] });
  assert.equal(disallowedTools, 1, "a junk push cleared a real restriction");
});

test("the harness sees pushed policy, including what arrives between starts", async () => {
  // Captures the real ctx the runtime hands the harness, then blocks — so the
  // restart loop does not spin while the assertions run.
  let ctx;
  let captured;
  const ready = new Promise((r) => { captured = r; });
  const harness = {
    name: "stub",
    async *run(_inputs, handed) {
      ctx = handed;
      captured();
      await new Promise(() => {}); // hold the turn open
    },
  };

  const rt = newRuntime(harness);
  rt.setResolvedOptions({ disallowedTools: ["mcp__hand__github__delete_repo"] });
  rt.acceptUserMessage("hello");
  await ready;

  assert.deepEqual(ctx.resolvedDisallow, ["mcp__hand__github__delete_repo"],
    "the harness did not receive the policy serve pushed");

  // resolvedDisallow is exposed as a live getter rather than copied in when the
  // session began, so a push arriving between two harness starts is in force on
  // the next start — which is exactly the self-heal path.
  rt.setResolvedOptions({ disallowedTools: ["mcp__hand__github__create_issue"] });
  assert.deepEqual([...ctx.resolvedDisallow].sort(), [
    "mcp__hand__github__create_issue",
    "mcp__hand__github__delete_repo",
  ], "a policy pushed between starts would be lost on restart");
});

// The last test deliberately leaves a turn open, so the runtime's watchdog
// interval keeps the loop alive. Nothing is left to assert once the tests pass.
after(() => process.exit(0));
