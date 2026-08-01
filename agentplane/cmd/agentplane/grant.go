package main

// Stateless, session-scoped GRANTS that let a session's HAND pull specific
// credentials from the vault. A grant is a signed token (HMAC over its claims),
// so serve needs no storage and grants survive a serve restart. The hand
// presents it to GET /v1/hand/credentials/{name}; serve verifies, then reads the
// vault and streams the value. The raw secret never touches the spec or brain.

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/quantumnode/agentplane/internal/naming"
)

// grantTTL bounds how long a hand may redeem its grant (threat-model F4). The
// hand pulls once, immediately after session create, so minutes is generous;
// a grant leaked from actor memory or a checkpoint expires almost at once.
// Grants are stateless (no revocation) — the short life IS the revocation.
const grantTTL = 10 * time.Minute

// grantSigningKey returns the HMAC key for grants (threat-model F3). It MUST
// differ from HAND_ADMIN_TOKEN: that token is mounted into every hand, so
// reusing it would let model-generated code that reads its own env forge a
// grant for any user's credentials. AGENTPLANE_GRANT_KEY never leaves serve.
// Absent, grants are disabled outright rather than silently insecure — a hand
// then simply gets no credentials, which fails closed.
func grantSigningKey() []byte {
	if k := os.Getenv("AGENTPLANE_GRANT_KEY"); k != "" {
		return []byte(k)
	}
	return nil
}

type grantClaims struct {
	Sid   string   `json:"sid"`
	User  string   `json:"user"`
	Names []string `json:"names"`
	Exp   int64    `json:"exp"`
}

func (s *server) signGrant(payload []byte) string {
	mac := hmac.New(sha256.New, s.grantKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// mintGrant returns a signed token authorizing `names` for this session's user.
func (s *server) mintGrant(sid, user string, names []string, ttl time.Duration) string {
	if len(s.grantKey) == 0 {
		return "" // no signing key configured — grants disabled (fail closed)
	}
	c := grantClaims{Sid: sid, User: user, Names: names, Exp: time.Now().Add(ttl).Unix()}
	payload, _ := json.Marshal(c)
	b64 := base64.RawURLEncoding.EncodeToString(payload)
	return b64 + "." + s.signGrant([]byte(b64))
}

func (s *server) verifyGrant(token string) (grantClaims, bool) {
	var c grantClaims
	if len(s.grantKey) == 0 || token == "" {
		return c, false // grants disabled: nothing verifies
	}
	b64, sig, ok := strings.Cut(token, ".")
	if !ok {
		return c, false
	}
	if !hmac.Equal([]byte(sig), []byte(s.signGrant([]byte(b64)))) {
		return c, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(b64)
	if err != nil || json.Unmarshal(payload, &c) != nil {
		return c, false
	}
	if time.Now().Unix() > c.Exp {
		return c, false
	}
	return c, true
}

// handleHandCredPull is called by the HAND (not a user): it presents its grant
// and a credential name; serve verifies the grant covers that name, reads the
// vault, and returns the payload. NOT wrapped in s.auth — the grant IS the auth.
func (s *server) handleHandCredPull(w http.ResponseWriter, r *http.Request) {
	if s.vault == nil {
		s.fail(w, r, http.StatusServiceUnavailable, "vault not configured", nil)
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	claims, ok := s.verifyGrant(tok)
	if !ok {
		writeErr(w, http.StatusUnauthorized, "invalid or expired grant")
		return
	}
	name := r.PathValue("name")
	allowed := false
	for _, n := range claims.Names {
		if n == name {
			allowed = true
			break
		}
	}
	if !allowed {
		writeErr(w, http.StatusForbidden, "grant does not cover this credential")
		return
	}
	p, err := s.vault.access(r.Context(), claims.User, name)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "read credential", err)
		return
	}
	s.log.Info("hand pulled credential", "user", claims.User, "session", claims.Sid, "name", name) // never the value
	writeJSON(w, http.StatusOK, p)
}

// grantHandCredentials mints a grant for the agent's declared credentials and
// pushes it (with serve's in-cluster URL) to the paired hand over atenet. The
// hand then PULLS each value from serve using the grant. No secret value is sent
// here — only the grant + the names. Retries the hand's cold-start wake race.
func (s *server) grantHandCredentials(ctx context.Context, sid, user, agent string) error {
	names, err := templateCredentials(ctx, s.sc, agent)
	if err != nil || len(names) == 0 {
		return err
	}
	grant := s.mintGrant(sid, user, names, grantTTL)
	serveBase := env("AGENTPLANE_SELF_URL", "http://agentplane-serve.agentplane.svc:7433")
	body, _ := json.Marshal(map[string]any{"serveBase": serveBase, "grant": grant, "credentials": names})

	hand := naming.HandActor(sid)
	url := fmt.Sprintf("http://%s/admin/grant", s.sc.atenet)
	adminTok := os.Getenv("HAND_ADMIN_TOKEN")

	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		req.Host = naming.ActorDNS(hand, s.sc.atespace)
		req.Header.Set("Content-Type", "application/json")
		if adminTok != "" {
			req.Header.Set("Authorization", "Bearer "+adminTok)
		}
		resp, err := s.client.Do(req)
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			if resp.StatusCode >= 400 {
				return fmt.Errorf("hand rejected grant: HTTP %d", resp.StatusCode)
			}
			return nil
		}
		if resp != nil {
			resp.Body.Close()
			lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		if attempt < 4 {
			select {
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return lastErr
}
