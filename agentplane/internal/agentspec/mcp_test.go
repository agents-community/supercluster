package agentspec

import (
	"encoding/json"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v4"
)

func mcpSpec(extra string) *AgentSpec {
	src := "name: t\nharness: claude-code\nimage: gcr.io/p/i@sha256:abc\n" + extra
	var s AgentSpec
	if err := yaml.Unmarshal([]byte(src), &s); err != nil {
		panic(err)
	}
	return &s
}

// The point of #46: a literal auth header is stored on the template, in the
// agent's append-only version history, and in every snapshot — and rotating the
// secret cannot remove it from history. It must be refused at create.
func TestLiteralAuthHeadersRejected(t *testing.T) {
	for _, h := range []string{"Authorization", "authorization", "X-API-Key", "Cookie", "x-goog-api-key"} {
		s := mcpSpec("mcp:\n  gh:\n    url: https://x/mcp\n    headers: {\"" + h + "\": \"secret\"}\n")
		err := s.Validate()
		if err == nil {
			t.Errorf("SECURITY: literal %q accepted — it would persist in version history", h)
			continue
		}
		if !strings.Contains(err.Error(), "headersFrom") {
			t.Errorf("%s: error should point at the fix, got: %v", h, err)
		}
	}
}

// Non-secret headers are still useful and must keep working.
func TestNonSecretHeadersStillAllowed(t *testing.T) {
	s := mcpSpec("mcp:\n  gh:\n    url: https://x/mcp\n    headers: {X-Tenant-Id: acme, Accept: application/json}\n")
	if err := s.Validate(); err != nil {
		t.Fatalf("non-secret headers rejected: %v", err)
	}
}

func TestHeadersFromCompilesAsReferenceOnly(t *testing.T) {
	s := mcpSpec("mcp:\n  gh:\n    url: https://x/mcp\n    headersFrom:\n      Authorization: {credential: gh-token, format: \"Bearer {}\"}\n")
	if err := s.Validate(); err != nil {
		t.Fatalf("valid headersFrom rejected: %v", err)
	}
	rt := s.runtimeSpec()
	if strings.Contains(rt, "secret") {
		t.Error("a secret leaked into the compiled spec")
	}
	var got struct {
		MCP map[string]struct {
			HeadersFrom map[string]CredentialRef `json:"headersFrom"`
			Headers     map[string]string        `json:"headers"`
		} `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(rt), &got); err != nil {
		t.Fatalf("runtimeSpec invalid: %v", err)
	}
	ref := got.MCP["gh"].HeadersFrom["Authorization"]
	if ref.Credential != "gh-token" {
		t.Errorf("credential reference lost: %+v", ref)
	}
	// Only the NAME is compiled — never a value.
	if len(got.MCP["gh"].Headers) != 0 {
		t.Errorf("headersFrom must not materialize into headers at compile time: %v", got.MCP["gh"].Headers)
	}
}

func TestCredentialRefRender(t *testing.T) {
	for _, tc := range []struct{ format, secret, want string }{
		{"", "abc", "abc"},
		{"Bearer {}", "abc", "Bearer abc"},
		{"token {}", "abc", "token abc"},
	} {
		if got := (CredentialRef{Format: tc.format}).Render(tc.secret); got != tc.want {
			t.Errorf("format %q: got %q want %q", tc.format, got, tc.want)
		}
	}
}

// A format with no placeholder would silently drop the secret and send a
// constant string — an upstream that fails in a confusing way.
func TestFormatWithoutPlaceholderRejected(t *testing.T) {
	s := mcpSpec("mcp:\n  gh:\n    url: https://x/mcp\n    headersFrom:\n      Authorization: {credential: gh-token, format: \"Bearer\"}\n")
	err := s.Validate()
	if err == nil || !strings.Contains(err.Error(), "{}") {
		t.Errorf("want a placeholder error, got: %v", err)
	}
}

func TestHeaderSetBothWaysRejected(t *testing.T) {
	s := mcpSpec("mcp:\n  gh:\n    url: https://x/mcp\n    headers: {X-Tenant-Id: acme}\n    headersFrom:\n      X-Tenant-Id: {credential: c}\n")
	if err := s.Validate(); err == nil {
		t.Error("a header set both literally and by reference is ambiguous and must be rejected")
	}
}
