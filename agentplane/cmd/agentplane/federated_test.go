package main

import (
	"reflect"
	"strings"
	"testing"
)

// Naming any tool of an upstream turns THAT upstream into an allow-list; the
// rest of its tools are denied. Upstreams nobody names are untouched, so this
// is inert for specs that do not use the syntax.
func TestResolveFederatedDenyScopesPerUpstream(t *testing.T) {
	federated := map[string][]string{
		"github": {"create_issue", "list_issues", "delete_repo"},
		"slack":  {"post_message"},
	}
	got := resolveFederatedDeny([]string{"github/list_issues"}, federated)
	want := []string{
		"mcp__hand__github__create_issue",
		"mcp__hand__github__delete_repo",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, d := range got {
		if d == "mcp__hand__slack__post_message" {
			t.Error("an upstream that was never scoped must not be restricted")
		}
	}
}

// The common case: no upstream-scoped entries at all. Must produce nothing, or
// every existing agent would silently lose its federated tools.
func TestNoScopedEntriesDeniesNothing(t *testing.T) {
	federated := map[string][]string{"github": {"create_issue", "list_issues"}}
	for _, allow := range [][]string{nil, {}, {"Bash"}, {"Bash", "Read"}} {
		if got := resolveFederatedDeny(allow, federated); len(got) != 0 {
			t.Errorf("allow=%v produced denials %v — existing agents would lose tools", allow, got)
		}
	}
}

func TestWildcardPermitsWholeUpstream(t *testing.T) {
	federated := map[string][]string{"github": {"create_issue", "list_issues"}}
	if got := resolveFederatedDeny([]string{"github/*"}, federated); len(got) != 0 {
		t.Errorf("github/* should deny nothing, got %v", got)
	}
}

// A tool the spec never mentions, on an upstream it does scope, must be denied
// — that is the entire point.
func TestUnnamedToolOnScopedUpstreamIsDenied(t *testing.T) {
	federated := map[string][]string{"github": {"create_issue", "delete_repo"}}
	got := resolveFederatedDeny([]string{"github/create_issue"}, federated)
	found := false
	for _, d := range got {
		if d == "mcp__hand__github__delete_repo" {
			found = true
		}
		if d == "mcp__hand__github__create_issue" {
			t.Error("an explicitly permitted tool was denied")
		}
	}
	if !found {
		t.Error("delete_repo was not denied despite github being scoped")
	}
}

// Nothing to federate means nothing to resolve — no panic, no output.
func TestNoUpstreams(t *testing.T) {
	if got := resolveFederatedDeny([]string{"github/x"}, nil); got != nil {
		t.Errorf("got %v", got)
	}
	if got := resolveFederatedDeny([]string{"github/x"}, map[string][]string{}); got != nil {
		t.Errorf("got %v", got)
	}
}

// Order must be stable, or every session create pushes a "different" policy and
// the logs churn.
func TestDenyOrderIsStable(t *testing.T) {
	federated := map[string][]string{
		"github": {"z_tool", "a_tool", "m_tool"},
	}
	first := resolveFederatedDeny([]string{"github/a_tool"}, federated)
	for i := 0; i < 5; i++ {
		if !reflect.DeepEqual(resolveFederatedDeny([]string{"github/a_tool"}, federated), first) {
			t.Fatal("output is not stable across calls")
		}
	}
}

// --- unmatched allow entries (#58) -----------------------------------------

// The dangerous one: a misspelled upstream leaves the REAL upstream unscoped,
// so every tool on it stays permitted while the spec reads as a restriction.
func TestUnmatchedScopesCatchesMisspelledUpstream(t *testing.T) {
	federated := map[string][]string{"github": {"list_issues", "create_issue"}}
	if got := resolveFederatedDeny([]string{"gihub/list_issues"}, federated); len(got) != 0 {
		t.Fatalf("precondition: expected the typo to deny nothing, got %v", got)
	}
	got := unmatchedScopes([]string{"gihub/list_issues"}, federated)
	if len(got) != 1 || !strings.Contains(got[0], "no such server") {
		t.Errorf("unmatchedScopes = %v, want a no-such-server finding", got)
	}
}

func TestUnmatchedScopesCatchesMisspelledTool(t *testing.T) {
	federated := map[string][]string{"github": {"list_issues"}}
	got := unmatchedScopes([]string{"github/list_isues"}, federated)
	if len(got) != 1 || !strings.Contains(got[0], "no such tool") {
		t.Errorf("unmatchedScopes = %v, want a no-such-tool finding", got)
	}
}

func TestUnmatchedScopesAcceptsValidEntries(t *testing.T) {
	federated := map[string][]string{"github": {"list_issues", "create_issue"}, "docs": {"search"}}
	for _, allow := range [][]string{
		{"github/list_issues"},
		{"github/*"},                          // whole-upstream wildcard
		{"Bash", "Read"},                      // plain tool names are not ours
		{"github/list_issues", "docs/search"}, // several upstreams
		{"github/list_issues", "github/create_issue"},
	} {
		if got := unmatchedScopes(allow, federated); len(got) != 0 {
			t.Errorf("unmatchedScopes(%v) = %v, want none", allow, got)
		}
	}
}

// No upstream connected means the tools do not exist, so nothing is granted.
// Failing a session because a third-party MCP server is down would be worse
// than running without its tools.
func TestUnmatchedScopesSilentWhenNothingFederated(t *testing.T) {
	for _, federated := range []map[string][]string{nil, {}} {
		if got := unmatchedScopes([]string{"github/list_issues"}, federated); got != nil {
			t.Errorf("expected no findings with no upstreams, got %v", got)
		}
	}
}

// An upstream that connected but exposes no tools is not a typo.
func TestUnmatchedScopesWildcardOnToollessUpstream(t *testing.T) {
	federated := map[string][]string{"github": {}}
	if got := unmatchedScopes([]string{"github/*"}, federated); len(got) != 0 {
		t.Errorf("expected no findings, got %v", got)
	}
}
