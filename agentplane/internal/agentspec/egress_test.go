package agentspec

import (
	"encoding/json"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v4"
)

// base is a minimal valid spec; each test layers egress/credentials onto it.
func base(extra string) string {
	return `name: t
harness: claude-code
image: gcr.io/p/i@sha256:abc
hand: true
` + extra
}

func load(t *testing.T, src string) *AgentSpec {
	t.Helper()
	var s AgentSpec
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return &s
}

// The bare-string form is what every existing spec uses — it must keep working
// unchanged, or merging this breaks deployed agents.
func TestCredentialsBareStringStillWorks(t *testing.T) {
	s := load(t, base("credentials: [gh-token]\n"))
	if err := s.Validate(); err != nil {
		t.Fatalf("bare-string credential rejected: %v", err)
	}
	if len(s.Credentials) != 1 || s.Credentials[0].Name != "gh-token" {
		t.Fatalf("got %+v", s.Credentials)
	}
	if s.Credentials[0].Inject != nil {
		t.Error("bare string should carry no injection policy")
	}
	// serve reads credentials as a plain name array — that shape must not drift.
	var rt struct {
		Credentials []string       `json:"credentials"`
		Egress      map[string]any `json:"egress"`
	}
	if err := json.Unmarshal([]byte(s.runtimeSpec()), &rt); err != nil {
		t.Fatalf("runtimeSpec not valid JSON: %v", err)
	}
	if len(rt.Credentials) != 1 || rt.Credentials[0] != "gh-token" {
		t.Errorf("credentials must compile to a name array, got %v", rt.Credentials)
	}
	if rt.Egress != nil {
		t.Errorf("no egress declared, so none should be emitted: %v", rt.Egress)
	}
}

func TestEgressObjectFormCompiles(t *testing.T) {
	s := load(t, base(`egress:
  mode: limited
  allowedHosts: [github.com, api.github.com]
credentials:
  - name: gh-token
    inject:
      hosts: [github.com]
      location: {header: true}
`))
	if err := s.Validate(); err != nil {
		t.Fatalf("valid spec rejected: %v", err)
	}
	var rt struct {
		Credentials []string `json:"credentials"`
		Egress      struct {
			Mode         string   `json:"mode"`
			AllowedHosts []string `json:"allowedHosts"`
			Injections   []struct {
				Credential string          `json:"credential"`
				Hosts      []string        `json:"hosts"`
				Location   map[string]bool `json:"location"`
			} `json:"injections"`
		} `json:"egress"`
	}
	if err := json.Unmarshal([]byte(s.runtimeSpec()), &rt); err != nil {
		t.Fatalf("runtimeSpec not valid JSON: %v", err)
	}
	if rt.Egress.Mode != "limited" || len(rt.Egress.AllowedHosts) != 2 {
		t.Errorf("egress policy lost in compile: %+v", rt.Egress)
	}
	if len(rt.Credentials) != 1 || rt.Credentials[0] != "gh-token" {
		t.Errorf("object-form credential must still compile to its name: %v", rt.Credentials)
	}
	if len(rt.Egress.Injections) != 1 {
		t.Fatalf("want 1 injection, got %+v", rt.Egress.Injections)
	}
	in := rt.Egress.Injections[0]
	if in.Credential != "gh-token" || len(in.Hosts) != 1 || in.Hosts[0] != "github.com" {
		t.Errorf("injection binding wrong: %+v", in)
	}
	if !in.Location["header"] || in.Location["body"] {
		t.Errorf("location should be header-only, got %v", in.Location)
	}
}

// The two-layer rule: reachable and trusted-with-the-secret are separate
// questions, and injecting into an unreachable host is a config error, not a
// silently-dead rule.
func TestInjectHostMustBeReachable(t *testing.T) {
	s := load(t, base(`egress:
  mode: limited
  allowedHosts: [github.com]
credentials:
  - name: gh-token
    inject:
      hosts: [evil.example.com]
      location: {header: true}
`))
	err := s.Validate()
	if err == nil {
		t.Fatal("SECURITY: injecting into a host outside the allowlist was accepted")
	}
	if !strings.Contains(err.Error(), "allowedHosts") {
		t.Errorf("error should explain the two-layer rule, got: %v", err)
	}
}

func TestEgressRejections(t *testing.T) {
	for _, tc := range []struct{ name, spec, want string }{
		{"unknown mode", "egress:\n  mode: sideways\n", "unknown egress.mode"},
		{"limited with no hosts", "egress:\n  mode: limited\n", "at least one allowedHosts"},
		{"hosts without limited", "egress:\n  allowedHosts: [github.com]\n", "requires mode: limited"},
		{"scheme in host", "egress:\n  mode: limited\n  allowedHosts: [https://github.com]\n", "scheme"},
		{"path in host", "egress:\n  mode: limited\n  allowedHosts: [github.com/org]\n", "path"},
		{"port in host", "egress:\n  mode: limited\n  allowedHosts: [github.com:443]\n", "port"},
		{"inject with no hosts", "credentials:\n  - name: gh-token\n    inject:\n      location: {header: true}\n", "at least one destination"},
		{"inject with no location", "credentials:\n  - name: gh-token\n    inject:\n      hosts: [github.com]\n      location: {}\n", "header and/or body"},
		{"duplicate credential", "credentials: [gh-token, gh-token]\n", "duplicate"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := load(t, base(tc.spec)).Validate()
			if err == nil {
				t.Fatalf("accepted invalid spec: %s", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should mention %q", err, tc.want)
			}
		})
	}
}

// Unrestricted is the default so existing agents are untouched.
func TestUnrestrictedIsDefault(t *testing.T) {
	s := load(t, base(""))
	if err := s.Validate(); err != nil {
		t.Fatalf("spec with no egress block rejected: %v", err)
	}
	if strings.Contains(s.runtimeSpec(), "egress") {
		t.Error("an agent that declares no egress should compile no egress policy")
	}
}
