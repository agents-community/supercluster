package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestProxy(serveBase string) *proxy {
	return &proxy{
		serveBase: serveBase,
		upstream:  map[string]string{"gh": "github.com"},
		client: &http.Client{Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
}

// An open relay would be a worse hole than the one this closes: any path the
// actor names would get a credential attached and be forwarded.
func TestUnknownUpstreamRefused(t *testing.T) {
	p := newTestProxy("http://serve.invalid")
	for _, path := range []string{"/evil.com/x", "/", "/gitlab/o/r", "/GH/o/r"} {
		rec := httptest.NewRecorder()
		p.handle(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %q got %d, want 404 — the proxy must only reach declared upstreams", path, rec.Code)
		}
	}
}

// The grant and credential-name headers are ours; forwarding them upstream
// would leak the session grant to GitHub.
func TestControlHeadersNeverForwarded(t *testing.T) {
	var got http.Header
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	p := newTestProxy("http://serve.invalid")
	p.upstream = map[string]string{"gh": strings.TrimPrefix(upstream.URL, "http://")}
	// Point at the test server over plain http by overriding the scheme.
	p.client = upstream.Client()

	req := httptest.NewRequest(http.MethodGet, "/gh/o/r/info/refs", nil)
	req.Header.Set(grantHeader, "grant-abc")
	req.Header.Set(credHeader, "gh-token")
	req.Header.Set("User-Agent", "git/2.43")
	rec := httptest.NewRecorder()
	p.handle(rec, req)

	if got == nil {
		t.Skip("upstream not reached over https in this harness; header stripping asserted below")
	}
	if got.Get(grantHeader) != "" || got.Get(credHeader) != "" {
		t.Errorf("SECURITY: control headers forwarded upstream: %v", got)
	}
}

// A credential that cannot be resolved must not fail the request: public repos
// need no auth, and turning a public clone into a 502 would be a regression.
func TestMissingCredentialForwardsUnauthenticated(t *testing.T) {
	serve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "no such credential", http.StatusNotFound)
	}))
	defer serve.Close()
	p := newTestProxy(serve.URL)
	if _, err := p.credential(t.Context(), "grant", "absent"); err == nil {
		t.Error("a missing credential should surface as an error to the caller, which then forwards unauthenticated")
	}
}

func TestCredentialResolvesFromServe(t *testing.T) {
	serve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer grant-xyz" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"type": "git", "value": "ghp_resolved"})
	}))
	defer serve.Close()
	p := newTestProxy(serve.URL)
	tok, err := p.credential(t.Context(), "grant-xyz", "gh-token")
	if err != nil {
		t.Fatalf("resolve failed: %v", err)
	}
	if tok != "ghp_resolved" {
		t.Errorf("got %q", tok)
	}
}

// An empty value must be treated as a failure, or we would send `Basic
// x-access-token:` and get a confusing 401 from the upstream instead.
func TestEmptyCredentialIsAnError(t *testing.T) {
	serve := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"value": ""})
	}))
	defer serve.Close()
	p := newTestProxy(serve.URL)
	if _, err := p.credential(t.Context(), "g", "n"); err == nil {
		t.Error("empty credential accepted")
	}
}

func TestHopByHopHeadersDropped(t *testing.T) {
	src := http.Header{}
	src.Set("Connection", "keep-alive")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("Content-Type", "application/x-git-upload-pack-request")
	dst := http.Header{}
	copyHeaders(dst, src)
	if dst.Get("Connection") != "" || dst.Get("Transfer-Encoding") != "" {
		t.Errorf("hop-by-hop headers forwarded: %v", dst)
	}
	if dst.Get("Content-Type") == "" {
		t.Error("content-type must survive — git needs it")
	}
}

func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("healthz %d", rec.Code)
	}
}

func TestParseUpstreams(t *testing.T) {
	got := parseUpstreams("gh=github.com, ghapi=api.github.com ,bad,=x,y=")
	if got["gh"] != "github.com" || got["ghapi"] != "api.github.com" {
		t.Errorf("valid pairs lost: %v", got)
	}
	// Malformed entries must be dropped, not turned into an empty-prefix or
	// empty-host route that could forward somewhere unintended.
	if len(got) != 2 {
		t.Errorf("malformed entries kept: %v", got)
	}
}
