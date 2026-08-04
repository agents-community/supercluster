package agentspec

import (
	"encoding/json"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v4"
)

func envSpec(extra string) *AgentSpec {
	var s AgentSpec
	src := "name: t\nharness: claude-code\nimage: gcr.io/p/i@sha256:abc\nhand: true\n" + extra
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		panic(err)
	}
	return &s
}

func TestRepositoryCompilesWithReferenceOnly(t *testing.T) {
	s := envSpec(`environment:
  repositories:
    - url: https://github.com/org/repo
      credential: gh-token
      checkout: {branch: main}
`)
	if err := s.Validate(); err != nil {
		t.Fatalf("valid repository rejected: %v", err)
	}
	var got struct {
		Repositories []Repository `json:"repositories"`
	}
	if err := json.Unmarshal([]byte(s.runtimeSpec()), &got); err != nil {
		t.Fatalf("runtimeSpec invalid: %v", err)
	}
	if len(got.Repositories) != 1 {
		t.Fatalf("repository lost in compile: %+v", got)
	}
	r := got.Repositories[0]
	// mountPath must be resolved at compile time so the runtime cannot derive a
	// different answer than validation checked.
	if r.MountPath != "/workspace/repo" {
		t.Errorf("mountPath not defaulted: %q", r.MountPath)
	}
	if r.Credential != "gh-token" {
		t.Errorf("credential reference lost: %q", r.Credential)
	}
}

// ssh:// cannot be routed through the git proxy, so the token would have to
// enter the sandbox to be usable — the exact thing this design prevents.
func TestNonHTTPSRepositoryRejected(t *testing.T) {
	for _, u := range []string{"ssh://git@github.com/o/r.git", "git://github.com/o/r", "http://github.com/o/r"} {
		s := envSpec("environment:\n  repositories:\n    - url: " + u + "\n")
		if err := s.Validate(); err == nil {
			t.Errorf("SECURITY: %q accepted — the credential could not be kept outside the sandbox", u)
		}
	}
}

func TestCredentialsInRepositoryURLRejected(t *testing.T) {
	s := envSpec("environment:\n  repositories:\n    - url: https://user:ghp_secret@github.com/o/r\n")
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "vault") {
		t.Errorf("a URL carrying a token must be rejected and point at the vault, got: %v", err)
	}
}

// Only /workspace survives suspend/resume; a checkout anywhere else is lost, and
// a path that escapes could overwrite another repo or the harness's own files.
func TestMountPathMustBeCleanAndUnderWorkspace(t *testing.T) {
	for _, mp := range []string{"/etc/passwd", "/workspace/../etc", "/tmp/x", "/workspace/a/../../b"} {
		s := envSpec("environment:\n  repositories:\n    - url: https://github.com/o/r\n      mountPath: " + mp + "\n")
		if err := s.Validate(); err == nil {
			t.Errorf("mountPath %q accepted", mp)
		}
	}
}

func TestTwoReposCannotShareAMount(t *testing.T) {
	s := envSpec(`environment:
  repositories:
    - {url: "https://github.com/o/a", mountPath: /workspace/x}
    - {url: "https://github.com/o/b", mountPath: /workspace/x}
`)
	if err := s.Validate(); err == nil {
		t.Error("two repositories mounting at the same path must be rejected")
	}
}

func TestCheckoutIsExclusive(t *testing.T) {
	s := envSpec("environment:\n  repositories:\n    - url: https://github.com/o/r\n      checkout: {branch: main, tag: v1}\n")
	if err := s.Validate(); err == nil {
		t.Error("branch and tag together are ambiguous and must be rejected")
	}
}

// environment.networking supersedes the legacy top-level egress; specifying
// both is a contradiction, not something to silently resolve.
func TestNetworkingSupersedesEgress(t *testing.T) {
	s := envSpec("environment:\n  networking: {mode: limited, allowedHosts: [github.com]}\n")
	if err := s.Validate(); err != nil {
		t.Fatalf("environment.networking rejected: %v", err)
	}
	if e := s.effectiveEgress(); e == nil || e.Mode != "limited" {
		t.Errorf("effectiveEgress did not resolve environment.networking: %+v", e)
	}
	both := envSpec("egress: {mode: limited, allowedHosts: [a.com]}\nenvironment:\n  networking: {mode: limited, allowedHosts: [b.com]}\n")
	if err := both.Validate(); err == nil {
		t.Error("both locations set must be rejected rather than silently picking one")
	}
}

// Existing specs must keep working unchanged.
func TestLegacyTopLevelEgressStillWorks(t *testing.T) {
	s := envSpec("egress: {mode: limited, allowedHosts: [github.com]}\n")
	if err := s.Validate(); err != nil {
		t.Fatalf("legacy egress rejected: %v", err)
	}
	if e := s.effectiveEgress(); e == nil || len(e.AllowedHosts) != 1 {
		t.Errorf("legacy egress not resolved: %+v", e)
	}
}
