package main

import (
	"reflect"
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
