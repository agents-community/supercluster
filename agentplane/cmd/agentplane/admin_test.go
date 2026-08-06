package main

import "testing"

// The fleet view crosses the ownership boundary every other route enforces, so
// who counts as an admin is the security-relevant part of this feature.

func TestAdminAllowlistEmptyGrantsNobody(t *testing.T) {
	t.Setenv("AGENTPLANE_ADMIN_EMAILS", "")
	t.Setenv("AGENTPLANE_ADMIN_EMAILS_FILE", "")
	a := newAdminAllowlist()
	if a.count() != 0 {
		t.Fatalf("expected an empty allowlist, got %d", a.count())
	}
	// A missing config must not read as "everyone" — that would turn a
	// deployment that forgot to set the var into a cross-user data leak.
	for _, u := range []string{"", "anyone@example.com", "root", "admin"} {
		if a.is(u) {
			t.Errorf("empty allowlist admitted %q", u)
		}
	}
}

func TestAdminAllowlistParsesAndNormalises(t *testing.T) {
	t.Setenv("AGENTPLANE_ADMIN_EMAILS_FILE", "")
	t.Setenv("AGENTPLANE_ADMIN_EMAILS", " Ops@Example.COM, sre@example.com\n# a comment\nlead@example.com # trailing\n")
	a := newAdminAllowlist()

	for _, u := range []string{"ops@example.com", "OPS@EXAMPLE.COM", "  sre@example.com  ", "lead@example.com"} {
		if !a.is(u) {
			t.Errorf("%q should be an admin", u)
		}
	}
	for _, u := range []string{"other@example.com", "a comment", "#", ""} {
		if a.is(u) {
			t.Errorf("%q should NOT be an admin", u)
		}
	}
	if a.count() != 3 {
		t.Errorf("count = %d, want 3", a.count())
	}
}

// Reload is what makes granting an operator possible without a redeploy.
func TestAdminAllowlistReloads(t *testing.T) {
	t.Setenv("AGENTPLANE_ADMIN_EMAILS_FILE", "")
	t.Setenv("AGENTPLANE_ADMIN_EMAILS", "first@example.com")
	a := newAdminAllowlist()
	if !a.is("first@example.com") || a.is("second@example.com") {
		t.Fatal("initial state wrong")
	}
	t.Setenv("AGENTPLANE_ADMIN_EMAILS", "second@example.com")
	a.reload()
	if a.is("first@example.com") {
		t.Error("a revoked admin still has access after reload")
	}
	if !a.is("second@example.com") {
		t.Error("a newly granted admin was not picked up")
	}
}

// Template → agent+version must use the same rule templateFor() composed with,
// or the fleet view attributes actors to the wrong agent.
func TestAgentFromTemplate(t *testing.T) {
	for _, tc := range []struct {
		template string
		agent    string
		version  int
	}{
		{"starter", "starter", 0}, // v1 keeps the bare name
		{"starter-v2", "starter", 2},
		{"starter-v17", "starter", 17},
		{"hand-v6", "hand", 6},
		{"my-v2-agent", "my-v2-agent", 0}, // suffix only counts at the END
		{"codex", "codex", 0},
	} {
		agent, version := agentFromTemplate(tc.template)
		if agent != tc.agent || version != tc.version {
			t.Errorf("agentFromTemplate(%q) = (%q, %d), want (%q, %d)",
				tc.template, agent, version, tc.agent, tc.version)
		}
	}
}

// Round-trips against the composer, so the two cannot drift apart.
func TestAgentFromTemplateRoundTripsTemplateFor(t *testing.T) {
	for _, agent := range []string{"starter", "codex", "my-agent"} {
		for _, v := range []int{1, 2, 9, 42} {
			got, gotV := agentFromTemplate(templateFor(agent, v))
			if got != agent {
				t.Errorf("templateFor(%q,%d) did not round-trip: got agent %q", agent, v, got)
			}
			// v1 keeps the bare name, so it round-trips as version 0 (unknown).
			want := v
			if v <= 1 {
				want = 0
			}
			if gotV != want {
				t.Errorf("templateFor(%q,%d) round-tripped version %d, want %d", agent, v, gotV, want)
			}
		}
	}
}
