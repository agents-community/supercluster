// The broker's tool implementations. Each tool is realized by running standard
// toolchain commands (sh, cat, ls, grep, base64, bash globs) in the hand actor
// via ExecActor — so the actor needs NONE of our code, just the toolchain. This
// replaces the in-actor executor: the runtime now lives here, outside gVisor.
//
// Schemas mirror the hand's former OWN_TOOLS so the model sees the same tools.

export const TOOL_DEFS = [
  { name: "bash", description: "Run a shell command in the workspace.",
    inputSchema: { type: "object", properties: { command: { type: "string", description: "the shell command to run (executed via sh -c in the workspace)" } }, required: ["command"] } },
  { name: "write", description: "Write a file (creates parent dirs).",
    inputSchema: { type: "object", properties: { path: { type: "string", description: "file path, absolute under the workspace or relative to it" }, content: { type: "string", description: "the full file contents to write" } }, required: ["path", "content"] } },
  { name: "read", description: "Read a file.",
    inputSchema: { type: "object", properties: { path: { type: "string", description: "file path to read" } }, required: ["path"] } },
  { name: "list", description: "List a directory.",
    inputSchema: { type: "object", properties: { path: { type: "string", description: "directory to list; defaults to the workspace root" } } } },
  { name: "edit", description: "Replace text in a file.",
    inputSchema: { type: "object", properties: { path: { type: "string" }, old_string: { type: "string", description: "exact text to find (must be unique unless replace_all)" }, new_string: { type: "string", description: "text to replace it with" }, replace_all: { type: "boolean", description: "replace every occurrence (default false)" } }, required: ["path", "old_string", "new_string"] } },
  { name: "glob", description: "Find files matching a glob.",
    inputSchema: { type: "object", properties: { pattern: { type: "string", description: "glob pattern, e.g. **/*.js" }, path: { type: "string", description: "base directory; defaults to the workspace root" } }, required: ["pattern"] } },
  { name: "grep", description: "Search file contents with a regular expression.",
    inputSchema: { type: "object", properties: { pattern: { type: "string", description: "extended regular expression" }, path: { type: "string", description: "subdirectory to search; defaults to the workspace root" }, glob: { type: "string", description: "only search files matching this glob, e.g. *.py" } }, required: ["pattern"] } },
];

export const TOOL_NAMES = new Set(TOOL_DEFS.map((t) => t.name));

const ok = (text) => ({ content: [{ type: "text", text }] });
const err = (text) => ({ isError: true, content: [{ type: "text", text }] });

// write via base64 so arbitrary content (quotes, newlines, binary-ish) is safe
// through argv without needing stdin: $1=path, $2=base64(content).
function writeFile(exec, path, content) {
  const b64 = Buffer.from(String(content), "utf8").toString("base64");
  return exec(["/bin/sh", "-c", 'mkdir -p "$(dirname -- "$1")" && printf %s "$2" | base64 -d > "$1"', "sh", path, b64]);
}

/**
 * Run a tool. `exec(argv, opts)` runs argv in the hand via ExecActor.
 * @returns MCP tool result.
 */
export async function runTool(name, args, exec) {
  switch (name) {
    case "bash": {
      if (!args.command) return err("command is required");
      const r = await exec(["/bin/sh", "-c", String(args.command)]);
      return ok(`exit ${r.exitCode}\n${r.stdout || ""}${r.stderr || ""}`);
    }
    case "read": {
      if (!args.path) return err("path is required");
      const r = await exec(["cat", "--", String(args.path)]);
      if (r.exitCode !== 0) return err(`read ${args.path}: ${(r.stderr || "").trim() || "failed"}`);
      return ok(r.stdout);
    }
    case "write": {
      if (!args.path) return err("path is required");
      const r = await writeFile(exec, String(args.path), args.content ?? "");
      if (r.exitCode !== 0) return err(`write ${args.path}: ${(r.stderr || "").trim() || "failed"}`);
      return ok(`wrote ${args.path}`);
    }
    case "list": {
      const r = await exec(["ls", "-la", "--", String(args.path || ".")]);
      if (r.exitCode !== 0) return err(`list ${args.path || "."}: ${(r.stderr || "").trim() || "failed"}`);
      return ok(r.stdout);
    }
    case "glob": {
      if (!args.pattern) return err("pattern is required");
      const r = await exec(["/bin/bash", "-c",
        'shopt -s globstar nullglob dotglob; cd "$1" 2>/dev/null || exit 0; for f in $2; do printf "%s\\n" "$f"; done',
        "bash", String(args.path || "."), String(args.pattern)]);
      return ok(r.stdout || "(no matches)");
    }
    case "grep": {
      if (!args.pattern) return err("pattern is required");
      const argv = ["grep", "-rn", "-E"];
      if (args.glob) argv.push("--include", String(args.glob));
      argv.push("--", String(args.pattern), String(args.path || "."));
      const r = await exec(argv);
      // grep exits 1 when there are no matches — that is not an error here.
      if (r.exitCode > 1) return err(`grep: ${(r.stderr || "").trim() || "failed"}`);
      return ok(r.stdout || "(no matches)");
    }
    case "edit": {
      if (!args.path || args.old_string == null || args.new_string == null) return err("path, old_string, new_string are required");
      const rd = await exec(["cat", "--", String(args.path)]);
      if (rd.exitCode !== 0) return err(`read ${args.path}: ${(rd.stderr || "").trim() || "failed"}`);
      const content = rd.stdout;
      const parts = content.split(args.old_string);
      const count = parts.length - 1;
      if (count === 0) return err(`old_string not found in ${args.path}`);
      if (count > 1 && !args.replace_all) return err(`old_string is not unique in ${args.path} (${count} matches); pass replace_all or add surrounding context`);
      const updated = args.replace_all ? parts.join(args.new_string) : content.replace(args.old_string, args.new_string);
      const wr = await writeFile(exec, String(args.path), updated);
      if (wr.exitCode !== 0) return err(`write ${args.path}: ${(wr.stderr || "").trim() || "failed"}`);
      const n = args.replace_all ? count : 1;
      return ok(`edited ${args.path} (${n} replacement${n > 1 ? "s" : ""})`);
    }
    default:
      return err(`unknown tool: ${name}`);
  }
}
