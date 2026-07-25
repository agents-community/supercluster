package agentspec

import (
	"encoding/json"
	"strings"
	"testing"
)

const validSpec = `
name: coder
harness: claude-code
image: gcr.io/p/agentplane-brain@sha256:abc123
systemPrompt: You are a coder.
turnDeadlineSeconds: 300
allow: [Bash, Read]
workspace:
  durable: true
`

func TestParseValid(t *testing.T) {
	s, err := Parse([]byte(validSpec))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.Name != "coder" || s.Harness != "claude-code" || !s.Workspace.Durable {
		t.Fatalf("unexpected spec: %+v", s)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"bad name uppercase":  strings.Replace(validSpec, "name: coder", "name: Coder", 1),
		"bad name underscore": strings.Replace(validSpec, "name: coder", "name: my_agent", 1),
		"unknown harness":     strings.Replace(validSpec, "harness: claude-code", "harness: gemini", 1),
		"unpinned image":      strings.Replace(validSpec, "image: gcr.io/p/agentplane-brain@sha256:abc123", "image: gcr.io/p/agentplane-brain:latest", 1),
	}
	for name, spec := range cases {
		if _, err := Parse([]byte(spec)); err == nil {
			t.Errorf("%s: expected validation error, got nil", name)
		}
	}
}

func compile(t *testing.T, spec string) map[string]any {
	t.Helper()
	s, err := Parse([]byte(spec))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	raw, err := s.CompileTemplate("agentplane", "test-bucket")
	if err != nil {
		t.Fatalf("CompileTemplate: %v", err)
	}
	var tmpl map[string]any
	if err := json.Unmarshal(raw, &tmpl); err != nil {
		t.Fatalf("compiled template is not JSON: %v", err)
	}
	return tmpl
}

func TestCompileDurableWorkspace(t *testing.T) {
	tmpl := compile(t, validSpec)
	spec := tmpl["spec"].(map[string]any)
	vols, ok := spec["volumes"].([]any)
	if !ok || len(vols) != 1 {
		t.Fatalf("want 1 volume, got %v", spec["volumes"])
	}
	v := vols[0].(map[string]any)
	if v["name"] != "workspace" || v["durableDir"] == nil {
		t.Fatalf("bad volume: %v", v)
	}
	c := spec["containers"].([]any)[0].(map[string]any)
	m := c["volumeMounts"].([]any)[0].(map[string]any)
	if m["name"] != "workspace" || m["mountPath"] != "/workspace" {
		t.Fatalf("bad mount: %v", m)
	}
}

func TestCompileWithoutWorkspace(t *testing.T) {
	tmpl := compile(t, strings.Replace(validSpec, "workspace:\n  durable: true", "", 1))
	spec := tmpl["spec"].(map[string]any)
	if _, has := spec["volumes"]; has {
		t.Fatal("volumes present without workspace.durable")
	}
	c := spec["containers"].([]any)[0].(map[string]any)
	if _, has := c["volumeMounts"]; has {
		t.Fatal("volumeMounts present without workspace.durable")
	}
	// runtime spec must carry the harness-relevant subset
	var env []map[string]any
	b, _ := json.Marshal(c["env"])
	_ = json.Unmarshal(b, &env)
	for _, e := range env {
		if e["name"] == "AGENTPLANE_SPEC" {
			var rt map[string]any
			if err := json.Unmarshal([]byte(e["value"].(string)), &rt); err != nil {
				t.Fatalf("AGENTPLANE_SPEC not JSON: %v", err)
			}
			if rt["systemPrompt"] != "You are a coder." || rt["turnDeadlineSeconds"] != float64(300) {
				t.Fatalf("runtime spec wrong: %v", rt)
			}
			return
		}
	}
	t.Fatal("AGENTPLANE_SPEC env missing")
}

func TestCompileHarnessKeyWiring(t *testing.T) {
	cases := []struct{ harness, envVar, secret string }{
		{"claude-code", "ANTHROPIC_API_KEY", "anthropic-api-key"},
		{"codex", "OPENAI_API_KEY", "openai-api-key"},
		{"pi", "ANTHROPIC_API_KEY", "anthropic-api-key"}, // no model → anthropic default
	}
	for _, tc := range cases {
		tmpl := compile(t, strings.Replace(validSpec, "harness: claude-code", "harness: "+tc.harness, 1))
		c := tmpl["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		found := map[string]any{}
		for _, e := range c["env"].([]any) {
			em := e.(map[string]any)
			found[em["name"].(string)] = em
		}
		if h := found["AGENTPLANE_HARNESS"].(map[string]any)["value"]; h != tc.harness {
			t.Errorf("%s: AGENTPLANE_HARNESS = %v", tc.harness, h)
		}
		key, ok := found[tc.envVar].(map[string]any)
		if !ok {
			t.Fatalf("%s: env %s missing (have %v)", tc.harness, tc.envVar, found)
		}
		ref := key["valueFrom"].(map[string]any)["secretKeyRef"].(map[string]any)
		if ref["name"] != tc.secret {
			t.Errorf("%s: secret = %v, want %s", tc.harness, ref["name"], tc.secret)
		}
	}
}

// Pi is model-agnostic: the key env is derived from the model's provider.
func TestPiProviderDerivedKey(t *testing.T) {
	cases := []struct{ model, envVar, secret string }{
		{"anthropic/claude-haiku-4-5", "ANTHROPIC_API_KEY", "anthropic-api-key"},
		{"openai/gpt-5", "OPENAI_API_KEY", "openai-api-key"},
		{"google/gemini-2.5-pro", "GEMINI_API_KEY", "gemini-api-key"},
	}
	for _, tc := range cases {
		spec := strings.Replace(validSpec, "harness: claude-code", "harness: pi", 1)
		spec += "\nmodel: " + tc.model + "\n"
		tmpl := compile(t, spec)
		c := tmpl["spec"].(map[string]any)["containers"].([]any)[0].(map[string]any)
		var got keyRef
		for _, e := range c["env"].([]any) {
			em := e.(map[string]any)
			if vf, ok := em["valueFrom"].(map[string]any); ok {
				got.EnvVar = em["name"].(string)
				got.DefaultSecret = vf["secretKeyRef"].(map[string]any)["name"].(string)
			}
		}
		if got.EnvVar != tc.envVar || got.DefaultSecret != tc.secret {
			t.Errorf("%s: wired %+v, want {%s %s}", tc.model, got, tc.envVar, tc.secret)
		}
	}
	// unsupported provider and bare model id are rejected
	for _, bad := range []string{"model: groq/llama-4", "model: claude-haiku-4-5"} {
		spec := strings.Replace(validSpec, "harness: claude-code", "harness: pi", 1) + "\n" + bad + "\n"
		if _, err := Parse([]byte(spec)); err == nil {
			t.Errorf("accepted invalid pi spec with %q", bad)
		}
	}
}

func TestCompileRequiresBucket(t *testing.T) {
	s, _ := Parse([]byte(validSpec))
	if _, err := s.CompileTemplate("agentplane", ""); err == nil {
		t.Fatal("expected error without bucket")
	}
}
