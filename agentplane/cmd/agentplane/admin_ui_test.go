package main

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestAdminConsoleServesSelfContainedPage(t *testing.T) {
	s := &server{}
	w := httptest.NewRecorder()
	s.handleAdminConsole(w, httptest.NewRequest("GET", "/admin", nil))
	if w.Code != 200 {
		t.Fatalf("status %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, "<!doctype html>") || !strings.Contains(body, "/v1/admin/actors") {
		t.Error("page did not render or does not call the API")
	}
	// A dashboard that reaches offsite would leak which cluster is being watched.
	for _, bad := range []string{"http://", "https://", "//cdn", "<link"} {
		if strings.Contains(body, bad) {
			t.Errorf("page references an external origin (%q) — it must be self-contained", bad)
		}
	}
	csp := w.Header().Get("Content-Security-Policy")
	// The page relies on inline style+script, so the CSP must permit exactly
	// those and nothing else. Getting this wrong renders a blank console.
	for _, need := range []string{"default-src 'none'", "style-src 'unsafe-inline'", "script-src 'unsafe-inline'", "connect-src 'self'"} {
		if !strings.Contains(csp, need) {
			t.Errorf("CSP missing %q — got %q", need, csp)
		}
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Error("console must not be cached against a newer binary")
	}
}

// serve is reachable from the internet — its ingress routes `/` — so the fleet
// view lives in a separate process with no Service of its own. This asserts the
// split holds: one careless line in serve.go would publish every user's session
// list, and it would look like any other route registration.
func TestAdminRoutesLiveOnlyInTheConsole(t *testing.T) {
	serveSrc, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`"GET /admin"`, `"GET /v1/admin/actors"`} {
		if strings.Contains(string(serveSrc), bad) {
			t.Errorf("serve.go registers %s — that puts the fleet view on the public endpoint", bad)
		}
	}
	consoleSrc, err := os.ReadFile("console.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"GET /admin"`, `"GET /v1/admin/actors"`} {
		if !strings.Contains(string(consoleSrc), want) {
			t.Errorf("console.go does not register %s — it would 404 everywhere", want)
		}
	}
	// The data route must stay behind auth even though nothing routes to it.
	if !strings.Contains(string(consoleSrc), `s.auth(s.handleAdminActors)`) {
		t.Error("the fleet API is not wrapped in auth")
	}
}
