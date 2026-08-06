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

// The ingress routes `/` to the public port, so a route on the main mux is on
// the internet. This asserts the admin surface is not registered there —
// a one-line mistake would silently publish every user's session list.
func TestAdminRoutesAreNotOnThePublicMux(t *testing.T) {
	src, err := os.ReadFile("serve.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, route := range []string{`mux.HandleFunc("GET /admin"`, `mux.HandleFunc("GET /v1/admin/actors"`} {
		// adminMux.HandleFunc(...) is the intended form; plain mux.HandleFunc is not.
		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, route) {
				t.Errorf("admin route registered on the PUBLIC mux: %s", trimmed)
			}
		}
	}
	if !strings.Contains(string(src), `adminMux.HandleFunc("GET /admin"`) {
		t.Error("the console is not registered on the admin mux either — it would 404 everywhere")
	}
}
