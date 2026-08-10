import { test } from "node:test";
import assert from "node:assert";
import { handURL, setHandURL } from "./identity.mjs";

test("serve-pushed hand URL overrides the computed default", () => {
  // No SESSION_NAME/actor-id here, so computed default is null for a non-brain id.
  assert.equal(handURL(), null);
  setHandURL("http://broker.agentplane.svc/mcp?hand=h-x");
  assert.equal(handURL(), "http://broker.agentplane.svc/mcp?hand=h-x");
});

test("HAND_MCP_URL env beats a pushed value (operator/test override)", () => {
  setHandURL("http://broker/mcp?hand=h-y");
  process.env.HAND_MCP_URL = "http://manual/mcp";
  try { assert.equal(handURL(), "http://manual/mcp"); }
  finally { delete process.env.HAND_MCP_URL; }
});

test("setHandURL ignores empty/non-string (never clears to a bad value)", () => {
  setHandURL("http://broker/mcp?hand=h-z");
  setHandURL("");        // ignored
  setHandURL(undefined); // ignored
  assert.equal(handURL(), "http://broker/mcp?hand=h-z");
});
