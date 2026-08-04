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
	"net/url"
	"os"
	"path"
	"regexp"
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
	// Environment describes what the sandbox IS — repositories on disk, what it
	// may reach — as opposed to what the agent DOES (prompt, model, tools). Kept
	// inside the AgentSpec rather than as a second API object: agent versioning
	// already gives safe iteration, and a separate object doubles the surface
	// for a benefit that only appears when one agent spans many repos (#49).
	Environment *Environment `yaml:"environment,omitempty"`
	// Hand: run a paired HAND actor (h-<id>) for this agent. The brain then
	// executes via the hand over MCP; deny local exec/fs tools so the model
	// routes them to the hand (no command runs in the brain).
	Hand bool `yaml:"hand,omitempty"`
	// Credentials names vault entries (see `agentplane cred …`) this agent's
	// sessions need. serve mints a scoped grant so the HAND pulls them at start;
	// the values never enter this spec or the brain. Requires hand:true.
	// Each entry is either a bare name ("gh-token") or an object carrying an
	// egress injection policy — see Credential.
	Credentials []Credential `yaml:"credentials,omitempty"`
	// Egress declares where this agent's sessions may talk. Deny-by-default
	// allowlisting is opt-in per agent; the default (unrestricted) is today's
	// behavior. NOT YET ENFORCED — see the note on Egress.
	Egress *Egress `yaml:"egress,omitempty"`
}

// Egress is the agent's outbound network policy.
//
// NOT YET ENFORCED. These fields are declarative today: they validate and
// compile onto the ActorTemplate so serve can render a gateway policy per
// session, but no gateway is deployed yet (threat-model F9), so actor egress
// is still unrestricted in practice. Documented as aspiration, not reality.
type Egress struct {
	// Mode is "unrestricted" (default — today's behavior) or "limited"
	// (deny-by-default; only AllowedHosts are reachable).
	Mode string `yaml:"mode,omitempty"`
	// AllowedHosts are the destinations reachable under mode: limited.
	// Hostnames only — no scheme, no path, no port.
	AllowedHosts []string `yaml:"allowedHosts,omitempty"`
}

// Credential is a vault entry this agent needs, optionally with an injection
// policy. It accepts either YAML form:
//
//	credentials: [gh-token]                      # bare name: no injection
//	credentials:
//	  - name: gh-token
//	    inject:
//	      hosts: [github.com]
//	      location: {header: true}
type Credential struct {
	Name string `yaml:"name"`
	// Inject, when set, means the egress gateway supplies this credential on
	// requests to Hosts — the sandbox never holds the value. Absent, the
	// credential follows the legacy path: the hand pulls it into actor memory.
	Inject *Inject `yaml:"inject,omitempty"`
}

// Inject binds a credential to the destinations that receive it.
//
// Hosts is deliberately separate from Egress.AllowedHosts: reachability and
// credential scope are different questions. A host must be in BOTH to receive
// the secret — being reachable never implies being trusted with it.
type Inject struct {
	Hosts    []string `yaml:"hosts"`
	Location Location `yaml:"location,omitempty"`
}

// Location is where in the outbound request the secret is substituted.
// The URL path is deliberately absent and unsupported: path-secret endpoints
// (e.g. Slack incoming webhooks) cannot be injected — use header auth.
type Location struct {
	Header bool `yaml:"header,omitempty"`
	Body   bool `yaml:"body,omitempty"`
}

// UnmarshalYAML accepts a bare string or the object form.
func (c *Credential) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind == yaml.ScalarNode {
		return node.Decode(&c.Name)
	}
	type plain Credential // avoid recursing into this method
	var p plain
	if err := node.Decode(&p); err != nil {
		return err
	}
	*c = Credential(p)
	return nil
}

// names returns just the credential names, the shape serve reads.
func (s *AgentSpec) credentialNames() []string {
	out := make([]string, 0, len(s.Credentials))
	for _, c := range s.Credentials {
		out = append(out, c.Name)
	}
	return out
}

type Workspace struct {
	Durable bool `yaml:"durable"`
}

// Environment is the sandbox's shape: what is mounted, and what is reachable.
type Environment struct {
	Repositories []Repository `yaml:"repositories,omitempty"`
	// Networking supersedes the top-level `egress:` block. Both are accepted;
	// specifying both is rejected rather than silently picking one.
	Networking *Egress `yaml:"networking,omitempty"`
}

// Repository is a git checkout placed in the sandbox at session setup.
//
// The credential is a VAULT REFERENCE, never a literal: the token is resolved
// outside the sandbox and attached by the git proxy, so it never lands in actor
// memory and cannot be captured by a checkpoint (snapshots include memory
// pages, not just disk).
type Repository struct {
	URL        string    `yaml:"url" json:"url"`
	Credential string    `yaml:"credential,omitempty" json:"credential,omitempty"`
	MountPath  string    `yaml:"mountPath,omitempty" json:"mountPath,omitempty"`
	Checkout   *Checkout `yaml:"checkout,omitempty" json:"checkout,omitempty"`
}

// Checkout selects what to check out. Branch and tag are mutually exclusive.
type Checkout struct {
	Branch string `yaml:"branch,omitempty" json:"branch,omitempty"`
	Tag    string `yaml:"tag,omitempty" json:"tag,omitempty"`
	Commit string `yaml:"commit,omitempty" json:"commit,omitempty"`
}

// defaultMountPath derives /workspace/<repo> from the URL when unset.
func (r Repository) defaultMountPath() string {
	base := strings.TrimSuffix(path.Base(strings.TrimSuffix(r.URL, "/")), ".git")
	if base == "" || base == "." || base == "/" {
		return ""
	}
	return "/workspace/" + base
}

type MCPSrv struct {
	URL string `yaml:"url"`
	// Headers are literal, and therefore public: the spec is stored on the
	// ActorTemplate, in the agent's append-only version history, and in every
	// snapshot of every session minted from it. Only non-secret headers belong
	// here — validation rejects the auth-bearing ones.
	Headers map[string]string `yaml:"headers,omitempty"`
	// HeadersFrom names a vault credential per header, resolved at session
	// setup for the user who created the session. The reference is what gets
	// stored; the value never enters the spec, the template, the history or a
	// snapshot — and two users of the same agent reach the upstream as
	// themselves rather than sharing one identity.
	HeadersFrom map[string]CredentialRef `yaml:"headersFrom,omitempty"`
}

// CredentialRef points at an entry in the caller's credential vault.
type CredentialRef struct {
	Credential string `yaml:"credential" json:"credential"`
	// Format wraps the secret; "{}" is the placeholder. Defaults to the bare
	// value, so `Bearer {}` or `token {}` are written explicitly.
	Format string `yaml:"format,omitempty" json:"format,omitempty"`
}

// Render returns the header value for a resolved secret.
func (c CredentialRef) Render(secret string) string {
	if c.Format == "" {
		return secret
	}
	return strings.ReplaceAll(c.Format, "{}", secret)
}

// secretHeaders are header names whose value is a credential. A literal here is
// rejected: it would be stored in three places that outlive the session, and
// shared by every user of the agent.
var secretHeaders = map[string]bool{
	"authorization": true, "proxy-authorization": true, "cookie": true,
	"x-api-key": true, "x-auth-token": true, "x-access-token": true,
	"api-key": true, "x-goog-api-key": true,
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
	if err := s.validateMCP(); err != nil {
		return err
	}
	if err := s.validateEnvironment(); err != nil {
		return err
	}
	return s.validateEgress()
}

// hostRe is a plain DNS hostname: labels of [a-z0-9-], dot-separated. No
// scheme, no port, no path — those are the usual ways an allowlist entry ends
// up matching nothing while looking correct.
var hostRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)

func validHost(h string) error {
	switch {
	case strings.Contains(h, "://"):
		return fmt.Errorf("host %q must not include a scheme", h)
	case strings.ContainsAny(h, "/?#"):
		return fmt.Errorf("host %q must not include a path", h)
	case strings.Contains(h, ":"):
		return fmt.Errorf("host %q must not include a port", h)
	case !hostRe.MatchString(h):
		return fmt.Errorf("host %q is not a valid hostname (lowercase, dot-separated labels)", h)
	}
	return nil
}

// effectiveEgress resolves the networking policy from either location, so the
// rest of the code has one place to read it.
func (s *AgentSpec) effectiveEgress() *Egress {
	if s.Environment != nil && s.Environment.Networking != nil {
		return s.Environment.Networking
	}
	return s.Egress
}

// repositories returns the declared repos with mountPath defaulted, so the
// runtime never has to re-derive it and disagree with validation.
func (s *AgentSpec) repositories() []Repository {
	if s.Environment == nil {
		return nil
	}
	out := make([]Repository, 0, len(s.Environment.Repositories))
	for _, r := range s.Environment.Repositories {
		if r.MountPath == "" {
			r.MountPath = r.defaultMountPath()
		}
		out = append(out, r)
	}
	return out
}

// validateEnvironment checks the sandbox description (#49).
func (s *AgentSpec) validateEnvironment() error {
	env := s.Environment
	if env == nil {
		return nil
	}
	if env.Networking != nil && s.Egress != nil {
		return fmt.Errorf("environment.networking and the top-level egress are the same " +
			"setting — specify one (prefer environment.networking)")
	}
	seen := map[string]bool{}
	for i, r := range env.Repositories {
		where := fmt.Sprintf("environment.repositories[%d]", i)
		if r.URL == "" {
			return fmt.Errorf("%s: url is required", where)
		}
		u, err := url.Parse(r.URL)
		if err != nil || u.Host == "" {
			return fmt.Errorf("%s: %q is not a valid repository URL", where, r.URL)
		}
		// https only. An ssh:// or git:// URL cannot be routed through the git
		// proxy, so the token would have to enter the sandbox to be usable —
		// exactly what this design exists to prevent.
		if u.Scheme != "https" {
			return fmt.Errorf("%s: scheme %q is not supported — use https so the proxy can "+
				"attach the credential outside the sandbox", where, u.Scheme)
		}
		if u.User != nil {
			return fmt.Errorf("%s: the URL carries credentials — put the token in the vault "+
				"and reference it with `credential:`", where)
		}
		mount := r.MountPath
		if mount == "" {
			mount = r.defaultMountPath()
			if mount == "" {
				return fmt.Errorf("%s: cannot derive a mountPath from %q — set one", where, r.URL)
			}
		}
		if !strings.HasPrefix(mount, "/workspace/") {
			return fmt.Errorf("%s: mountPath %q must be under /workspace/ — only that path is "+
				"durable across suspend/resume, and writing elsewhere would be lost", where, mount)
		}
		// A checkout escaping its mount would let one repo overwrite another, or
		// land on the harness's own files.
		if cleaned := path.Clean(mount); cleaned != mount || strings.Contains(mount, "..") {
			return fmt.Errorf("%s: mountPath %q must be a clean absolute path", where, mount)
		}
		if seen[mount] {
			return fmt.Errorf("%s: two repositories both mount at %q", where, mount)
		}
		seen[mount] = true
		if c := r.Checkout; c != nil {
			n := 0
			for _, v := range []string{c.Branch, c.Tag, c.Commit} {
				if v != "" {
					n++
				}
			}
			if n > 1 {
				return fmt.Errorf("%s: checkout takes exactly one of branch, tag or commit", where)
			}
		}
	}
	return nil
}

// validateMCP keeps credentials out of the spec. A literal auth header would be
// stored on the ActorTemplate, in the agent's append-only version history, and
// in every snapshot — and rotating the secret would not remove the old one from
// history. It would also be one identity shared by every user of the agent.
func (s *AgentSpec) validateMCP() error {
	for name, srv := range s.MCP {
		if srv.URL == "" {
			return fmt.Errorf("mcp %q: url is required", name)
		}
		for h := range srv.Headers {
			if secretHeaders[strings.ToLower(strings.TrimSpace(h))] {
				return fmt.Errorf("mcp %q: header %q carries a credential and must not be a "+
					"literal — the spec is stored on the template, in version history and in "+
					"every snapshot. Use headersFrom: {%s: {credential: <vault-name>}}", name, h, h)
			}
		}
		for h, ref := range srv.HeadersFrom {
			if ref.Credential == "" {
				return fmt.Errorf("mcp %q: headersFrom %q needs a credential name", name, h)
			}
			if _, dup := srv.Headers[h]; dup {
				return fmt.Errorf("mcp %q: header %q is set both literally and via headersFrom", name, h)
			}
			if ref.Format != "" && !strings.Contains(ref.Format, "{}") {
				return fmt.Errorf("mcp %q: headersFrom %q format %q has no {} placeholder, so the "+
					"secret would be dropped", name, h, ref.Format)
			}
		}
	}
	return nil
}

func (s *AgentSpec) validateEgress() error {
	limited := false
	allowed := map[string]bool{}
	if e := s.effectiveEgress(); e != nil {
		switch e.Mode {
		case "", "unrestricted":
			if len(e.AllowedHosts) > 0 {
				return fmt.Errorf("egress.allowedHosts requires mode: limited (unrestricted reaches everything)")
			}
		case "limited":
			limited = true
			if len(e.AllowedHosts) == 0 {
				return fmt.Errorf("egress.mode: limited requires at least one allowedHosts entry (an empty allowlist reaches nothing)")
			}
		default:
			return fmt.Errorf("unknown egress.mode %q (want unrestricted or limited)", e.Mode)
		}
		for _, h := range e.AllowedHosts {
			if err := validHost(h); err != nil {
				return fmt.Errorf("egress.allowedHosts: %w", err)
			}
			allowed[h] = true
		}
	}
	seen := map[string]bool{}
	for _, c := range s.Credentials {
		if !credNameRe.MatchString(c.Name) {
			return fmt.Errorf("credential name %q must be a lowercase DNS-1123 label", c.Name)
		}
		if seen[c.Name] {
			return fmt.Errorf("duplicate credential %q", c.Name)
		}
		seen[c.Name] = true
		if c.Inject == nil {
			continue
		}
		if len(c.Inject.Hosts) == 0 {
			return fmt.Errorf("credential %q: inject.hosts must name at least one destination", c.Name)
		}
		for _, h := range c.Inject.Hosts {
			if err := validHost(h); err != nil {
				return fmt.Errorf("credential %q: inject.%w", c.Name, err)
			}
			// Two layers, both required: reachable AND trusted with the secret.
			if limited && !allowed[h] {
				return fmt.Errorf("credential %q injects into %q, which is not in egress.allowedHosts — a host must be reachable before it can be trusted with a secret", c.Name, h)
			}
		}
		if !c.Inject.Location.Header && !c.Inject.Location.Body {
			return fmt.Errorf("credential %q: inject.location must enable header and/or body", c.Name)
		}
	}
	return nil
}

// credNameRe matches vault credential names (same charset the vault enforces).
var credNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$`)

// runtimeSpec is the subset shipped to the in-image adapter as AGENTPLANE_SPEC.
func (s *AgentSpec) runtimeSpec() string {
	type mcp struct {
		URL         string                   `json:"url"`
		Headers     map[string]string        `json:"headers,omitempty"`
		HeadersFrom map[string]CredentialRef `json:"headersFrom,omitempty"`
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
			// HeadersFrom carries only vault NAMES, so it is safe to compile
			// onto the template; serve resolves them per session, per user.
			m[k] = mcp{URL: v.URL, Headers: v.Headers, HeadersFrom: v.HeadersFrom}
		}
		rt["mcp"] = m
	}
	if len(s.Credentials) > 0 {
		// Names only — serve reads this shape to mint the hand's grant.
		rt["credentials"] = s.credentialNames()
	}
	if repos := s.repositories(); len(repos) > 0 {
		// Credential is a NAME here, never a value — the git proxy resolves it
		// per user, outside the sandbox (#49).
		rt["repositories"] = repos
	}
	if eg := s.egressPolicy(); eg != nil {
		rt["egress"] = eg
	}
	b, _ := json.Marshal(rt)
	return string(b)
}

const pauseImage = "registry.k8s.io/pause:3.10.2@sha256:f548e0e8e3dc1896ca956272154dde3314e8cc4fde0a57577ee9fa1c63f5baf4"

// CompileTemplate renders the ActorTemplate (as JSON) realizing this agent.
// namespace/pool wiring matches deploy/brain.yaml.tmpl; bucket hosts snapshots.
// CompileTemplate compiles the spec to its ActorTemplate, named for the agent.
func (s *AgentSpec) CompileTemplate(namespace, bucket string) ([]byte, error) {
	return s.CompileVersion(namespace, bucket, s.Name, 0)
}

// CompileVersion compiles the spec as a SPECIFIC template name, stamping the
// logical agent and version as labels (#32). Versions are separate templates —
// ActorTemplate.spec is immutable, so a change is a new artifact — and the
// labels are what group them back into one agent for listing.
func (s *AgentSpec) CompileVersion(namespace, bucket, templateName string, version int) ([]byte, error) {
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
		// The logical agent this template is a version OF. Without it, `starter`
		// and `starter-v2` look like two unrelated agents in a template listing.
		"agentplane.io/agent": s.Name,
	}
	if version > 0 {
		labels["agentplane.io/version"] = fmt.Sprint(version)
	}
	if s.Hand {
		labels["agentplane.io/hand"] = "true" // serve pairs an h-<id> per session
	}
	tmpl := map[string]any{
		"apiVersion": "ate.dev/v1alpha1",
		"kind":       "ActorTemplate",
		"metadata": map[string]any{
			"name":      templateName,
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

// egressPolicy renders the compiled egress policy carried on the template:
// the reachability allowlist plus each credential's injection binding. serve
// turns this into the gateway's per-session policy. nil when the agent
// declares neither.
func (s *AgentSpec) egressPolicy() map[string]any {
	injections := []map[string]any{}
	for _, c := range s.Credentials {
		if c.Inject == nil {
			continue
		}
		loc := map[string]bool{}
		if c.Inject.Location.Header {
			loc["header"] = true
		}
		if c.Inject.Location.Body {
			loc["body"] = true
		}
		if len(loc) == 0 {
			loc["header"] = true // validated default: header-only
		}
		injections = append(injections, map[string]any{
			"credential": c.Name,
			"hosts":      c.Inject.Hosts,
			"location":   loc,
		})
	}
	mode, hosts := "unrestricted", []string(nil)
	if s.effectiveEgress() != nil {
		if s.effectiveEgress().Mode != "" {
			mode = s.effectiveEgress().Mode
		}
		hosts = s.effectiveEgress().AllowedHosts
	}
	if mode == "unrestricted" && len(injections) == 0 {
		return nil // nothing to say
	}
	out := map[string]any{"mode": mode}
	if len(hosts) > 0 {
		out["allowedHosts"] = hosts
	}
	if len(injections) > 0 {
		out["injections"] = injections
	}
	return out
}
