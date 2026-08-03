package main

import "testing"

// Version 1 keeps the bare agent name so agents created before versioning —
// and every session already running on them — stay valid without a migration.
// Renumbering v1 to `starter-v1` would strand those sessions on a template
// that no longer exists.
func TestVersionOneKeepsTheBareName(t *testing.T) {
	if got := templateFor("starter", 1); got != "starter" {
		t.Errorf("v1 must keep the agent name, got %q", got)
	}
	// A zero/unknown version resolves the same way: callers that have no
	// version information still address the original template.
	if got := templateFor("starter", 0); got != "starter" {
		t.Errorf("unknown version must fall back to the agent name, got %q", got)
	}
}

func TestLaterVersionsGetDistinctTemplates(t *testing.T) {
	seen := map[string]int{}
	for v := 1; v <= 5; v++ {
		name := templateFor("starter", v)
		if prev, dup := seen[name]; dup {
			t.Fatalf("v%d and v%d both compile to %q — the second could not apply", prev, v, name)
		}
		seen[name] = v
	}
	if got := templateFor("starter", 2); got != "starter-v2" {
		t.Errorf("got %q", got)
	}
	if got := templateFor("starter", 17); got != "starter-v17" {
		t.Errorf("multi-digit versions must not truncate, got %q", got)
	}
}

// An agent literally named `foo-v2` would collide with version 2 of `foo`:
// same template name, different spec, and the second one silently unable to
// apply. Creation must reject the name rather than discover this later.
func TestVersionSuffixNamesAreRejected(t *testing.T) {
	for _, bad := range []string{"starter-v2", "foo-v1", "agent-v10", "x-v99"} {
		if !versionSuffixRe.MatchString(bad) {
			t.Errorf("%q looks like a version template and must be rejected", bad)
		}
	}
	// Names that merely contain a v, or end in a non-numeric v-word, are fine.
	for _, ok := range []string{"starter", "v2-agent", "review-v", "vector", "my-vpn", "starter-verbose"} {
		if versionSuffixRe.MatchString(ok) {
			t.Errorf("%q is a legitimate agent name and must be accepted", ok)
		}
	}
}

// The suffix must survive the DNS-1123 rules that gate every template name,
// or a valid agent could produce an unappliable version.
func TestVersionedTemplateNamesStayDNSSafe(t *testing.T) {
	for _, agent := range []string{"a", "starter", "my-long-agent-name"} {
		for _, v := range []int{2, 10, 100} {
			name := templateFor(agent, v)
			if !isDNS1123Label(name) {
				t.Errorf("version template %q is not a valid DNS-1123 label", name)
			}
		}
	}
}

// isDNS1123Label mirrors the constraint Kubernetes applies to object names;
// naming.IsAgentName enforces the same shape for agents.
func isDNS1123Label(s string) bool {
	if len(s) == 0 || len(s) > 63 {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i != 0 && i != len(s)-1:
		default:
			return false
		}
	}
	return true
}
