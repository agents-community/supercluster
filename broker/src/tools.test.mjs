import { test } from "node:test";
import assert from "node:assert";
import { runTool } from "./tools.mjs";

// mock exec records argv and returns a scripted result
function mock(result) { const calls=[]; const exec=async(argv,opts)=>{calls.push({argv,opts});return result(argv);}; exec.calls=calls; return exec; }

test("bash formats exit + stdout + stderr", async () => {
  const exec = mock(() => ({ stdout: "hi\n", stderr: "", exitCode: 0 }));
  const r = await runTool("bash", { command: "echo hi" }, exec);
  assert.match(r.content[0].text, /exit 0\nhi/);
  assert.deepEqual(exec.calls[0].argv, ["/bin/sh","-c","echo hi"]);
});

test("write base64-encodes content through argv", async () => {
  const exec = mock(() => ({ stdout:"", stderr:"", exitCode: 0 }));
  await runTool("write", { path: "a.txt", content: "hello\nworld" }, exec);
  const argv = exec.calls[0].argv;
  const b64 = argv[argv.length-1];
  assert.equal(Buffer.from(b64,"base64").toString(), "hello\nworld");
});

test("edit refuses non-unique old_string without replace_all", async () => {
  const exec = mock((argv) => argv[0]==="cat" ? ({stdout:"x x x",stderr:"",exitCode:0}) : ({stdout:"",stderr:"",exitCode:0}));
  const r = await runTool("edit", { path:"f", old_string:"x", new_string:"y" }, exec);
  assert.ok(r.isError); assert.match(r.content[0].text, /not unique/);
});

test("edit replace_all replaces every occurrence", async () => {
  let written=null;
  const exec = async(argv)=>{ if(argv[0]==="cat") return {stdout:"x x x",stderr:"",exitCode:0}; written=Buffer.from(argv[argv.length-1],"base64").toString(); return {stdout:"",stderr:"",exitCode:0}; };
  const r = await runTool("edit", { path:"f", old_string:"x", new_string:"y", replace_all:true }, exec);
  assert.ok(!r.isError); assert.equal(written, "y y y");
});

test("grep treats exit 1 (no matches) as success", async () => {
  const exec = mock(() => ({ stdout:"", stderr:"", exitCode: 1 }));
  const r = await runTool("grep", { pattern:"zzz" }, exec);
  assert.ok(!r.isError);
});
