// Package naming defines agentplane's session/actor naming convention.
//
// A session id is `sess-<10 lowercase alphanumerics>`. The actors serving a
// session are derived, never stored:
//
//	brain: b-<session-id>     e.g. b-sess-x7k2m9qw4a
//	hand:  h-<session-id>     e.g. h-sess-x7k2m9qw4a
//
// Names are DNS labels (atenet routes by <name>.<atespace>.<suffix>), so the
// alphabet is [a-z0-9-] and total length stays well under 63. Identity ⇒
// address is a pure function — the convention IS the service discovery.
package naming

import (
	"crypto/rand"
	"fmt"
	"regexp"
	"strings"
)

const (
	sessionPrefix = "sess-"
	idLength      = 10
	alphabet      = "abcdefghijklmnopqrstuvwxyz0123456789"
	// ActorDNSSuffix mirrors Substrate's internal/resources.ActorDNSSuffix.
	ActorDNSSuffix = "actors.resources.substrate.ate.dev"
)

var sessionRe = regexp.MustCompile(`^sess-[a-z0-9]{4,40}$`)

// agentNameRe is a strict DNS-1123 label: lowercase alphanumerics and internal
// hyphens only, no leading/trailing hyphen. This is a SECURITY boundary, not
// just cosmetics — agent names become kubectl positional arguments, so a name
// like "--all" must be rejected before it can be read as a flag.
var agentNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// IsAgentName reports whether s is a safe agent (ActorTemplate) name.
func IsAgentName(s string) bool { return agentNameRe.MatchString(s) }

// NewSessionID mints a fresh session id (crypto-random, DNS-safe).
func NewSessionID() (string, error) {
	buf := make([]byte, idLength)
	// Rejection sampling to avoid modulo bias (alphabet is 36 chars; accept
	// bytes < 252 = 7*36 and take mod 36).
	out := make([]byte, 0, idLength)
	for len(out) < idLength {
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("naming: entropy: %w", err)
		}
		for _, b := range buf {
			if b < 252 {
				out = append(out, alphabet[int(b)%len(alphabet)])
				if len(out) == idLength {
					break
				}
			}
		}
	}
	return sessionPrefix + string(out), nil
}

// IsSessionID reports whether s is a well-formed session id.
func IsSessionID(s string) bool { return sessionRe.MatchString(s) }

// BrainActor returns the brain actor name for a session.
func BrainActor(sessionID string) string { return "b-" + sessionID }

// HandActor returns the hand actor name for a session.
func HandActor(sessionID string) string { return "h-" + sessionID }

// SessionFromActor extracts the session id from a brain/hand actor name.
func SessionFromActor(actor string) (string, bool) {
	for _, p := range []string{"b-", "h-"} {
		if s, ok := strings.CutPrefix(actor, p); ok && IsSessionID(s) {
			return s, true
		}
	}
	return "", false
}

// ActorDNS returns the atenet Host header / DNS name for an actor.
func ActorDNS(actor, atespace string) string {
	return actor + "." + atespace + "." + ActorDNSSuffix
}
