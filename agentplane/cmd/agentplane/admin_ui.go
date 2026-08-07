// The fleet console (#70), served by serve itself.
//
// Why here rather than a separate static host: the page must call
// /v1/admin/actors, and serving it from the same origin means no CORS, no
// second deployment, and no way for the console and the API it reads to drift
// to different versions — they ship in one binary.
//
// The page itself carries no data and needs no auth. Everything it shows comes
// from an authenticated fetch the browser makes, so the admin allowlist stays
// the single gate; a curl of /admin returns markup and nothing else.
package main

import (
	_ "embed"
	"net/http"
)

//go:embed admin.html
var adminConsoleHTML []byte

func (s *server) handleAdminConsole(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// No caching: the console and the API travel together, so a browser holding
	// yesterday's page against today's binary is a shape of bug worth refusing.
	w.Header().Set("Cache-Control", "no-store")
	// The page is self-contained — inline CSS and script, no external origins —
	// so it can be locked down hard. This is also what makes the token safe to
	// keep in sessionStorage: nothing third-party can run here.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; "+
			"connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	_, _ = w.Write(adminConsoleHTML)
}
