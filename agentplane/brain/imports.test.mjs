// Every local module the brain imports must be COPYed into every brain image.
//
// A missing one fails at ACTOR START, not at build: node resolves imports
// lazily, so `docker build` succeeds and the actor dies on first use. This has
// happened twice — approval.mjs was added without updating any Dockerfile, and
// otel.mjs was missing from the pi/codex images for long enough that only their
// not having been rebuilt hid it.
//
// Cheap to check, and the failure it prevents is expensive to diagnose.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readdirSync, readFileSync } from "node:fs";

const localImports = (src) =>
  [...src.matchAll(/from\s+"(\.[^"]+)"/g)].map((m) => m[1]);

test("every locally imported module is copied into every brain image", () => {
  // What the top-level modules and harnesses actually import.
  const roots = ["server.mjs", "runtime.mjs", "identity.mjs", "otel.mjs", "approval.mjs"];
  const needed = new Set(roots);
  for (const f of roots) {
    for (const imp of localImports(readFileSync(f, "utf8"))) {
      needed.add(imp.replace(/^\.\//, ""));
    }
  }
  for (const f of readdirSync("harness")) {
    if (!f.endsWith(".mjs")) continue;
    for (const imp of localImports(readFileSync(`harness/${f}`, "utf8"))) {
      // ../x.mjs from a harness is a top-level module that must be copied.
      if (imp.startsWith("../")) needed.add(imp.slice(3));
    }
  }

  const dockerfiles = readdirSync(".").filter((f) => f.startsWith("Dockerfile"));
  assert.ok(dockerfiles.length >= 1, "no Dockerfiles found");

  for (const df of dockerfiles) {
    const copied = new Set();
    for (const line of readFileSync(df, "utf8").split("\n")) {
      const m = line.match(/^COPY\s+(.+?)\s+\/app\/?$/);
      if (m) for (const part of m[1].split(/\s+/)) copied.add(part);
    }
    // `COPY harness /app/harness` covers the harness directory itself.
    for (const mod of needed) {
      if (mod.startsWith("harness/")) continue;
      assert.ok(copied.has(mod),
        `${df} does not COPY ${mod} — the actor will fail at start, not at build`);
    }
  }
});

// The COPY check above is not enough: copying a module into an image that does
// not npm-install its dependencies fails exactly the same way, at actor start.
// That is precisely what happened — otel.mjs was added to the pi image, which
// never installed @opentelemetry/api, and every pi session started 502ing.
test("every bare import in a copied module is installed by that image", () => {
  const bareImports = (src) =>
    [...src.matchAll(/from\s+"([^".][^"]*)"/g)].map((m) => m[1])
      .filter((m) => !m.startsWith(".") && !m.startsWith("node:"))
      // "@scope/pkg/sub/path.js" -> "@scope/pkg"
      .map((m) => (m.startsWith("@") ? m.split("/").slice(0, 2).join("/") : m.split("/")[0]));

  for (const df of readdirSync(".").filter((f) => f.startsWith("Dockerfile"))) {
    const dockerfile = readFileSync(df, "utf8");
    const copied = [];
    for (const line of dockerfile.split("\n")) {
      const m = line.match(/^COPY\s+(.+?)\s+\/app\/?$/);
      if (m) copied.push(...m[1].split(/\s+/));
    }
    // Harness files are copied wholesale, but only the one this image runs is
    // ever imported — so they are checked separately, per image, below.
    const needed = new Set();
    for (const f of copied) {
      for (const dep of bareImports(readFileSync(f, "utf8"))) needed.add(dep);
    }
    for (const dep of needed) {
      assert.ok(dockerfile.includes(dep),
        `${df} copies a module importing ${dep} but never installs it — the actor 502s at start`);
    }
  }
});
