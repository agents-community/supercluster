package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Admin grants replace HAND_ADMIN_TOKEN, which was one static secret mounted
// into every hand (#74). These pin the properties that make the replacement
// worth anything: scoped to one session, separate from credential grants, and
// short-lived.

func serverWithGrants(t *testing.T) *server {
	t.Helper()
	t.Setenv("AGENTPLANE_GRANT_KEY", "test-signing-key-not-a-real-one")
	return &server{grantKey: grantSigningKey()}
}

func TestAdminGrantIsScopedToOneSession(t *testing.T) {
	s := serverWithGrants(t)
	g := mintAdminGrant("sess-aaa")
	if g == "" {
		t.Fatal("no grant minted despite a signing key")
	}
	claims, ok := s.verifyGrant(g)
	if !ok || !claims.Admin {
		t.Fatalf("admin grant did not verify: ok=%v admin=%v", ok, claims.Admin)
	}
	if claims.Sid != "sess-aaa" {
		t.Errorf("sid = %q, want sess-aaa", claims.Sid)
	}
	// The hand compares this against its own identity; if the sid did not
	// travel, every hand would accept every grant and we would have rebuilt
	// the shared token with extra steps.
	if claims.Sid == "" {
		t.Error("a grant with no session is a fleet-wide credential again")
	}
}

// The whole point of a separate Admin claim: a credential grant, which a hand
// legitimately holds and which model-written code may be able to read, must not
// be replayable against /admin.
func TestCredentialGrantIsNotAnAdminGrant(t *testing.T) {
	s := serverWithGrants(t)
	cred := s.mintGrant("sess-aaa", "user@example.com", []string{"gh-token"}, grantTTL)
	claims, ok := s.verifyGrant(cred)
	if !ok {
		t.Fatal("credential grant should verify as a grant")
	}
	if claims.Admin {
		t.Fatal("a credential grant must NOT carry the admin claim — it would be replayable against /admin")
	}

	// And the endpoint must refuse it.
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/hand/admin-verify", nil)
	r.Header.Set("Authorization", "Bearer "+cred)
	s.handleHandAdminVerify(w, r)
	if w.Code != 401 {
		t.Errorf("replayed credential grant got %d, want 401", w.Code)
	}
}

func TestAdminVerifyAcceptsAndReportsTheSession(t *testing.T) {
	s := serverWithGrants(t)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v1/hand/admin-verify", nil)
	r.Header.Set("Authorization", "Bearer "+mintAdminGrant("sess-bbb"))
	s.handleHandAdminVerify(w, r)
	if w.Code != 200 {
		t.Fatalf("status %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), "sess-bbb") {
		t.Errorf("body must name the session so the hand can bind it: %s", w.Body.String())
	}
}

func TestAdminVerifyRejectsGarbageAndTampering(t *testing.T) {
	s := serverWithGrants(t)
	good := mintAdminGrant("sess-ccc")
	// Flip the payload but keep the signature — the classic forgery attempt.
	tampered := "eyJzaWQiOiJzZXNzLXp6eiIsImFkbWluIjp0cnVlfQ." + strings.SplitN(good, ".", 2)[1]

	for name, tok := range map[string]string{
		"empty":       "",
		"not a grant": "just-a-string",
		"tampered":    tampered,
		"wrong sig":   strings.SplitN(good, ".", 2)[0] + ".AAAA",
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("GET", "/v1/hand/admin-verify", nil)
		if tok != "" {
			r.Header.Set("Authorization", "Bearer "+tok)
		}
		s.handleHandAdminVerify(w, r)
		if w.Code != 401 {
			t.Errorf("%s: got %d, want 401", name, w.Code)
		}
	}
}

// Grants disabled must fail closed, not open.
func TestNoSigningKeyMintsNothing(t *testing.T) {
	t.Setenv("AGENTPLANE_GRANT_KEY", "")
	if g := mintAdminGrant("sess-aaa"); g != "" {
		t.Error("minted a grant with no signing key — it could not be verified by anyone")
	}
	s := &server{grantKey: nil}
	if _, ok := s.verifyGrant("anything"); ok {
		t.Error("verified a grant with no signing key")
	}
}

// Short life is the revocation: grants are stateless, so the TTL is the only
// thing bounding a leaked one.
func TestAdminGrantExpiresQuicklyAndSoonerThanCredentialGrants(t *testing.T) {
	if adminGrantTTL > grantTTL {
		t.Errorf("admin TTL %v should not exceed the credential TTL %v", adminGrantTTL, grantTTL)
	}
	if adminGrantTTL > 5*time.Minute {
		t.Errorf("admin TTL %v is too long — it is used within seconds of minting", adminGrantTTL)
	}
}
