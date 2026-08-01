package main

// Self-service access: an allowlisted email exchanges itself for an API token.
// POST /v1/access {email} — UNAUTHENTICATED by design (it's how you get your
// first token), but gated by an email allowlist the operator controls.
//
// TRIAL-GRADE: the token is returned directly in the response, so anyone who
// knows an allowlisted email can obtain that user's token. That's acceptable for
// a closed trial with a handful of known addresses; for production, deliver the
// token by email (magic link) instead so only the inbox owner receives it. The
// allowlist + handler here are the seam for that upgrade.

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"
)

var emailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// emailAllowlist is the set of addresses permitted to self-issue a token. It is
// loaded from a mounted file (a ConfigMap, one email per line or comma/space
// separated) and/or the AGENTPLANE_ALLOWED_EMAILS env var, and hot-reloaded so
// the operator can add a tester without a redeploy.
type emailAllowlist struct {
	mu   sync.RWMutex
	set  map[string]bool
	file string
}

func newEmailAllowlist() *emailAllowlist {
	a := &emailAllowlist{set: map[string]bool{}, file: os.Getenv("AGENTPLANE_ALLOWED_EMAILS_FILE")}
	a.reload()
	return a
}

func (a *emailAllowlist) reload() {
	next := map[string]bool{}
	consume := func(s string) {
		for _, line := range strings.Split(s, "\n") {
			if i := strings.IndexByte(line, '#'); i >= 0 { // strip trailing/whole-line comments
				line = line[:i]
			}
			for _, part := range strings.FieldsFunc(line, func(r rune) bool {
				return r == ',' || r == '\r' || r == ' ' || r == '\t'
			}) {
				e := strings.ToLower(strings.TrimSpace(part))
				if emailRe.MatchString(e) { // only real addresses enter the set
					next[e] = true
				}
			}
		}
	}
	if a.file != "" {
		if data, err := os.ReadFile(a.file); err == nil {
			consume(string(data))
		}
	}
	consume(os.Getenv("AGENTPLANE_ALLOWED_EMAILS"))
	a.mu.Lock()
	a.set = next
	a.mu.Unlock()
}

func (a *emailAllowlist) allowed(email string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.set[strings.ToLower(strings.TrimSpace(email))]
}

func (a *emailAllowlist) count() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.set)
}

// add makes a freshly issued token usable immediately, without waiting for the
// mounted-secret file to refresh (~1 min) and the store to hot-reload.
func (ts *tokenStore) add(token, user string) {
	ts.mu.Lock()
	ts.byToken[token] = user
	ts.mu.Unlock()
}

// handleAccess exchanges an allowlisted email for a token (issuing one on first
// request, returning the same one on repeat). Not wrapped in s.auth.
// accessLimiter throttles the unauthenticated /v1/access endpoint
// (threat-model F7): email alone is the credential there, so unbounded
// attempts let an attacker sweep for allowlisted addresses. Fixed-window
// counters per client IP and per email — small, dependency-free, and reset
// on restart (serve is stateless by design).
type accessLimiter struct {
	mu     sync.Mutex
	window time.Time
	byIP   map[string]int
	byMail map[string]int
}

const (
	accessWindow   = time.Minute
	accessPerIP    = 10 // attempts/min from one address
	accessPerEmail = 5  // attempts/min for one email (legit use is once)
)

func newAccessLimiter() *accessLimiter {
	return &accessLimiter{window: time.Now(), byIP: map[string]int{}, byMail: map[string]int{}}
}

// allow reports whether this attempt may proceed, rolling the window as needed.
func (l *accessLimiter) allow(ip, email string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if time.Since(l.window) > accessWindow {
		l.window = time.Now()
		l.byIP = map[string]int{}
		l.byMail = map[string]int{}
	}
	l.byIP[ip]++
	l.byMail[email]++
	return l.byIP[ip] <= accessPerIP && l.byMail[email] <= accessPerEmail
}

// clientIP prefers the LB's forwarded address over the socket peer.
func clientIP(r *http.Request) string {
	if f := r.Header.Get("X-Forwarded-For"); f != "" {
		if i := strings.IndexByte(f, ','); i > 0 {
			return strings.TrimSpace(f[:i])
		}
		return strings.TrimSpace(f)
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *server) handleAccess(w http.ResponseWriter, r *http.Request) {
	if s.emails == nil || s.emails.count() == 0 {
		writeErr(w, http.StatusServiceUnavailable, "self-service access is not enabled")
		return
	}
	var in struct {
		Email string `json:"email"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	email := strings.ToLower(strings.TrimSpace(in.Email))
	if !emailRe.MatchString(email) {
		writeErr(w, http.StatusBadRequest, "provide a valid email address")
		return
	}
	if s.limiter != nil && !s.limiter.allow(clientIP(r), email) {
		s.log.Warn("access rate-limited", "ip", clientIP(r), "email", email) // audit
		writeErr(w, http.StatusTooManyRequests, "too many attempts — try again in a minute")
		return
	}
	if !s.emails.allowed(email) {
		s.log.Info("access denied", "email", email) // audit
		writeErr(w, http.StatusForbidden, "this email isn't on the allowlist — ask your host to add it")
		return
	}

	ns := env("BRAIN_TEMPLATE_NS", "agentplane")
	m := loadTokens(ns)
	tok, existing := m[email]
	if !existing {
		tok = newToken()
		m[email] = tok
		if err := saveTokens(ns, m); err != nil {
			s.fail(w, r, http.StatusBadGateway, "could not issue token", err)
			return
		}
	}
	s.tokens.add(tok, email) // usable now, not after the secret refresh
	s.log.Info("access granted", "email", email, "reissued", existing)
	writeJSON(w, http.StatusOK, map[string]any{"user": email, "token": tok, "reissued": existing})
}
