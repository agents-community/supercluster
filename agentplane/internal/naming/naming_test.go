package naming

import (
	"strings"
	"testing"
)

func TestNewSessionID(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id, err := NewSessionID()
		if err != nil {
			t.Fatal(err)
		}
		if !IsSessionID(id) {
			t.Fatalf("minted id fails own validation: %q", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id in 100 draws: %q", id)
		}
		seen[id] = true
	}
}

func TestActorRoundtrip(t *testing.T) {
	id := "sess-abc123xy9z"
	for _, actor := range []string{BrainActor(id), HandActor(id)} {
		got, ok := SessionFromActor(actor)
		if !ok || got != id {
			t.Fatalf("roundtrip %q → %q ok=%v", actor, got, ok)
		}
	}
}

func TestValidation(t *testing.T) {
	for _, bad := range []string{"", "sess-", "sess-ABC", "sess_abc123", "b-sess-abc123xy9z", "sess-ab"} {
		if IsSessionID(bad) {
			t.Errorf("accepted invalid id %q", bad)
		}
	}
}

// TestIsAgentName guards the kubectl argument-injection defense: an agent name
// becomes a kubectl positional argument, so flag-shaped names MUST be rejected.
func TestIsAgentName(t *testing.T) {
	valid := []string{"brain", "coder", "codex2", "my-agent", "a", "a1b2"}
	for _, s := range valid {
		if !IsAgentName(s) {
			t.Errorf("rejected valid agent name %q", s)
		}
	}
	invalid := []string{
		"", "--all", "-x", "x-", "-", "UPPER", "under_score", "dot.name",
		"has space", "a/b", "a;b", "$(x)", "a--b-", strings.Repeat("a", 64),
	}
	for _, s := range invalid {
		if IsAgentName(s) {
			t.Errorf("accepted UNSAFE agent name %q", s)
		}
	}
}

func TestActorDNSIsValidLabelChain(t *testing.T) {
	id, _ := NewSessionID()
	dns := ActorDNS(BrainActor(id), "agents")
	if strings.ContainsAny(dns, "_A") || len(BrainActor(id)) > 63 {
		t.Fatalf("DNS-unsafe name: %q", dns)
	}
}
