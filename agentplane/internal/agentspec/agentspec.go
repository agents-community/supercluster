// Package agentspec defines the AgentSpec file format (D1: our config, adapters
// translate) and compiles it to the Substrate ActorTemplate that realizes it
// (D3: Agent = ActorTemplate).
//
// The compiler emits the template as JSON — valid input for kubectl and immune
// to YAML quoting issues around the embedded harness config.
package agentspec

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	yaml "go.yaml.in/yaml/v4"

	"github.com/quantumnode/agentplane/internal/naming"
)

// AgentSpec is the portable agent definition. Everything harness-specific is
// derived from it by the in-image adapter (brain/server.mjs) at runtime.
type AgentSpec struct {
	Name    string `yaml:"name"`
	Harness string `yaml:"harness"` // claude-code (codex, opencode: planned)
	// Image must be digest-pinned (@sha256:…) — Substrate invalidates snapshots
	// on image change, so mutable tags are forbidden by construction.
	Image               string            `yaml:"image"`
	SystemPrompt        string            `yaml:"systemPrompt"`
	Model               string            `yaml:"model,omitempty"`
	MCP                 map[string]MCPSrv `yaml:"mcp,omitempty"`
	Allow               []string          `yaml:"allow,omitempty"`
	Deny                []string          `yaml:"deny,omitempty"`
	TurnDeadlineSeconds int               `yaml:"turnDeadlineSeconds,omitempty"`
	// APIKeySecret names the namespace Secret holding the vendor key
	// (key "api-key"). Defaults to the shared "anthropic-api-key".
	APIKeySecret string `yaml:"apiKeySecret,omitempty"`
	// Workspace configures /workspace. With durable:true it becomes a
	// Substrate DurableDir volume: filesystem state (repos, artifacts)
	// persists across resumes as FS data, outside the memory image.
	Workspace *Workspace `yaml:"workspace,omitempty"`
	// Hand: run a paired HAND actor (h-<id>) for this agent. The brain then
	// executes via the hand over MCP; deny local exec/fs tools so the model
	// routes them to the hand (no command runs in the brain).
	Hand bool `yaml:"hand,omitempty"`
	// Credentials names vault entries (see `agentplane cred …`) this agent's
	// sessions need. serve mints a scoped grant so the HAND pulls them at start;
	// the values never enter this spec or the brain. Requires hand:true.
	Credentials []string `yaml:"credentials,omitempty"`
}

type Workspace struct {
	Durable bool `yaml:"durable"`
}

type MCPSrv struct {
	URL     string            `yaml:"url"`
	Headers map[string]string `yaml:"headers,omitempty"`
}

type keyRef struct{ EnvVar, DefaultSecret string }

// harnessKey maps each single-vendor harness to its key env var and the
// default Secret holding it (spec.apiKeySecret overrides the secret name).
// The "pi" harness is model-agnostic — its key is derived from the MODEL's
// provider prefix instead (see keyForSpec / providerKey).
var harnessKey = map[string]keyRef{
	"claude-code": {"ANTHROPIC_API_KEY", "anthropic-api-key"},
	"codex":       {"OPENAI_API_KEY", "openai-api-key"},
	"pi":          {}, // provider-derived; presence marks the harness as known
}

// providerKey maps a pi model's provider prefix ("provider/model-id") to its
// key wiring. These env names are what pi-ai resolves from the environment.
var providerKey = map[string]keyRef{
	"anthropic": {"ANTHROPIC_API_KEY", "anthropic-api-key"},
	"openai":    {"OPENAI_API_KEY", "openai-api-key"},
	"google":    {"GEMINI_API_KEY", "gemini-api-key"},
}

// keyForSpec resolves the key wiring for a spec: fixed per harness, except pi
// where the model's provider decides (default anthropic when model is unset).
func keyForSpec(s *AgentSpec) (keyRef, error) {
	if s.Harness != "pi" {
		return harnessKey[s.Harness], nil
	}
	provider := "anthropic"
	if s.Model != "" {
		provider, _, _ = strings.Cut(s.Model, "/")
	}
	kr, ok := providerKey[provider]
	if !ok {
		return keyRef{}, fmt.Errorf("pi: unsupported model provider %q (known: anthropic, openai, google) — set apiKeySecret and file an issue to add it", provider)
	}
	return kr, nil
}

// Load reads and validates a spec file.
func Load(path string) (*AgentSpec, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	s, err := Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return s, nil
}

// Parse validates a spec from raw bytes (YAML; JSON works too — it's a subset).
func Parse(raw []byte) (*AgentSpec, error) {
	var s AgentSpec
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse spec: %w", err)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *AgentSpec) Validate() error {
	_, harnessKnown := harnessKey[s.Harness]
	switch {
	case !naming.IsAgentName(s.Name):
		// Strict DNS-1123 label; also a security boundary (see naming.IsAgentName).
		return fmt.Errorf("name must be a lowercase DNS-1123 label [a-z0-9-], no leading/trailing hyphen, got %q", s.Name)
	case !harnessKnown:
		return fmt.Errorf("unknown harness %q (known: claude-code, codex, pi)", s.Harness)
	case !strings.Contains(s.Image, "@sha256:"):
		return fmt.Errorf("image must be digest-pinned (@sha256:…) — snapshots break otherwise")
	case s.Harness == "pi" && s.Model != "" && !strings.Contains(s.Model, "/"):
		return fmt.Errorf(`pi models are "provider/model-id" (e.g. anthropic/claude-haiku-4-5), got %q`, s.Model)
	case len(s.Credentials) > 0 && !s.Hand:
		return fmt.Errorf("credentials require hand:true (the hand pulls and holds them; the brain never sees them)")
	}
	if _, err := keyForSpec(s); err != nil {
		return err
	}
	return nil
}

// runtimeSpec is the subset shipped to the in-image adapter as AGENTPLANE_SPEC.
func (s *AgentSpec) runtimeSpec() string {
	type mcp struct {
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	rt := map[string]any{}
	if s.SystemPrompt != "" {
		rt["systemPrompt"] = s.SystemPrompt
	}
	if s.Model != "" {
		rt["model"] = s.Model
	}
	if s.TurnDeadlineSeconds > 0 {
		rt["turnDeadlineSeconds"] = s.TurnDeadlineSeconds
	}
	if len(s.Allow) > 0 {
		rt["allow"] = s.Allow
	}
	if len(s.Deny) > 0 {
		rt["deny"] = s.Deny
	}
	if len(s.MCP) > 0 {
		m := map[string]mcp{}
		for k, v := range s.MCP {
			m[k] = mcp{URL: v.URL, Headers: v.Headers}
		}
		rt["mcp"] = m
	}
	if len(s.Credentials) > 0 {
		rt["credentials"] = s.Credentials
	}
	b, _ := json.Marshal(rt)
	return string(b)
}

const pauseImage = "registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4"

// CompileTemplate renders the ActorTemplate (as JSON) realizing this agent.
// namespace/pool wiring matches deploy/brain.yaml.tmpl; bucket hosts snapshots.
func (s *AgentSpec) CompileTemplate(namespace, bucket string) ([]byte, error) {
	if bucket == "" {
		return nil, fmt.Errorf("bucket required (set AGENTPLANE_BUCKET)")
	}
	hk, err := keyForSpec(s)
	if err != nil {
		return nil, err
	}
	secret := s.APIKeySecret
	if secret == "" {
		secret = hk.DefaultSecret
	}
	env := []map[string]any{
		{"name": "PORT", "value": "80"}, // atenet forwards to :80
		{"name": "AGENTPLANE_ATESPACE", "value": "agents"},
		{"name": "AGENTPLANE_HARNESS", "value": s.Harness},
		{"name": "AGENTPLANE_SPEC", "value": s.runtimeSpec()},
		{"name": hk.EnvVar, "valueFrom": map[string]any{
			"secretKeyRef": map[string]any{"name": secret, "key": "api-key"},
		}},
	}
	// Claude Code telemetry (token/cost metrics + per-LLM-call trace spans) →
	// the OTLP collector, when the operator sets AGENTPLANE_OTEL_ENDPOINT.
	// Only claude-code exports this; logs (prompt content) stay OFF for privacy.
	if s.Harness == "claude-code" {
		if ep := os.Getenv("AGENTPLANE_OTEL_ENDPOINT"); ep != "" {
			for _, kv := range [][2]string{
				{"CLAUDE_CODE_ENABLE_TELEMETRY", "1"},
				{"CLAUDE_CODE_ENHANCED_TELEMETRY_BETA", "1"},
				{"OTEL_METRICS_EXPORTER", "otlp"},
				{"OTEL_TRACES_EXPORTER", "otlp"},
				{"OTEL_LOGS_EXPORTER", "none"}, // prompts OFF (privacy / BYO-key)
				{"OTEL_EXPORTER_OTLP_PROTOCOL", "grpc"},
				{"OTEL_EXPORTER_OTLP_ENDPOINT", ep},
			} {
				env = append(env, map[string]any{"name": kv[0], "value": kv[1]})
			}
		}
	}
	container := map[string]any{
		"name":    "brain",
		"image":   s.Image,
		"command": []string{"/entrypoint.sh"}, // atelet ignores ENTRYPOINT (#189)
		"env":     env,
	}
	labels := map[string]string{
		"agentplane.io/harness": s.Harness,
		"agentplane.io/managed": "true",
	}
	if s.Hand {
		labels["agentplane.io/hand"] = "true" // serve pairs an h-<id> per session
	}
	tmpl := map[string]any{
		"apiVersion": "ate.dev/v1alpha1",
		"kind":       "ActorTemplate",
		"metadata": map[string]any{
			"name":      s.Name,
			"namespace": namespace,
			"labels":    labels,
		},
		"spec": map[string]any{
			"pauseImage": pauseImage,
			"containers": []map[string]any{container},
			"workerSelector": map[string]any{
				"matchLabels": map[string]string{"workload": "agentplane-brain"},
			},
			"snapshotsConfig": map[string]any{
				"location": fmt.Sprintf("gs://%s/agentplane/", bucket),
			},
		},
	}
	// Durable workspace (Phase 1.5): /workspace as a Substrate DurableDir —
	// repos/artifacts persist across resumes as filesystem data instead of
	// inflating the memory image.
	if s.Workspace != nil && s.Workspace.Durable {
		container["volumeMounts"] = []map[string]any{{"name": "workspace", "mountPath": "/workspace"}}
		tmpl["spec"].(map[string]any)["volumes"] = []map[string]any{{"name": "workspace", "durableDir": map[string]any{}}}
	}
	return json.MarshalIndent(tmpl, "", "  ")
}
