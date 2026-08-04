// gitproxy attaches a user's git credential to outbound git traffic, outside
// the sandbox (#49).
//
// The problem it solves: to clone or push a private repo, the hand needed the
// token IN the sandbox — in actor memory and in `~/.git-credentials`. Substrate
// checkpoints capture memory pages (checkpoint.img, pages.img, pages_meta.img),
// so a snapshot taken during a push captures the token too. Anything inside the
// sandbox is reachable by model-generated code and by anyone who can read a
// snapshot.
//
// How it avoids TLS interception entirely: git in the hand is configured with
//
//	url.http://git-proxy.agentplane.svc/gh/.insteadOf = https://github.com/
//
// so the actor speaks PLAIN HTTP to this in-cluster service, and this service
// makes the real HTTPS call outbound with the credential attached. No CA has to
// be provisioned into the sandbox, and no TLS is terminated — which is what
// made the general egress gateway hard.
//
// What the sandbox still holds: the session GRANT (minutes-long, scoped to that
// session's declared credentials), never the credential itself. A grant is
// resolved here against serve, which owns the vault.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// grantHeader carries the session's grant. git sends it via
	// `http.extraHeader`, which the hand configures at session setup.
	grantHeader = "X-Agentplane-Grant"
	// credHeader names which of the session's credentials to attach. The grant
	// bounds this: it is scoped to the credentials the agent declared, so naming
	// a different one cannot reach another user's secret.
	credHeader = "X-Agentplane-Credential"

	maxBody = 256 << 20 // git packfiles are large; cap rather than stream unbounded
)

type proxy struct {
	serveBase string
	client    *http.Client
	// upstream maps a path prefix to the real host. Only these are proxied —
	// an open relay that forwards anywhere would be a far worse hole than the
	// one this closes.
	upstream map[string]string
}

func main() {
	p := &proxy{
		serveBase: env("AGENTPLANE_SERVE_BASE", "http://agentplane-serve.agentplane.svc:7433"),
		upstream:  parseUpstreams(env("GITPROXY_UPSTREAMS", "gh=github.com,ghapi=api.github.com")),
		client: &http.Client{
			Timeout: 5 * time.Minute, // clones are slow; this is not a chat request
			Transport: &http.Transport{
				DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
				MaxIdleConnsPerHost: 8,
			},
			// Git follows redirects itself; following them here would silently
			// forward the credential to whatever host the redirect names.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	mux.HandleFunc("/", p.handle)

	addr := env("GITPROXY_ADDR", ":8080")
	log.Printf("gitproxy listening on %s → serve %s", addr, p.serveBase)
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 15 * time.Second}
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}

// parseUpstreams reads "prefix=host,prefix=host". Configurable so a GitHub
// Enterprise or self-hosted host can be added without a rebuild — but still a
// closed set, because forwarding anywhere would make this an open relay that
// attaches credentials to arbitrary destinations.
func parseUpstreams(spec string) map[string]string {
	out := map[string]string{}
	for _, pair := range strings.Split(spec, ",") {
		prefix, host, ok := strings.Cut(strings.TrimSpace(pair), "=")
		if !ok || prefix == "" || host == "" {
			continue
		}
		out[prefix] = host
	}
	return out
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// handle rewrites /<prefix>/<path> to https://<upstream>/<path>, attaching the
// caller's credential.
func (p *proxy) handle(w http.ResponseWriter, r *http.Request) {
	prefix, rest, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	host, known := p.upstream[prefix]
	if !known {
		// Refusing unknown prefixes is what keeps this from being an open relay.
		http.Error(w, "unknown upstream", http.StatusNotFound)
		return
	}

	target := &url.URL{Scheme: "https", Host: host, Path: "/" + rest, RawQuery: r.URL.RawQuery}
	out, err := http.NewRequestWithContext(r.Context(), r.Method, target.String(),
		io.LimitReader(r.Body, maxBody))
	if err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	copyHeaders(out.Header, r.Header)
	out.Header.Del(grantHeader)
	out.Header.Del(credHeader)
	out.Host = host

	// Resolve the credential for THIS session. A failure is not fatal: public
	// repos need no auth, so forward unauthenticated and let the upstream
	// decide, rather than turning a public clone into a proxy error.
	authed := "none"
	// Only attempt a lookup when the session actually named a credential.
	// Defaulting to a well-known name meant a PUBLIC repo clone did a doomed
	// vault lookup and logged `failed:gh-token`, which reads as an auth problem
	// when nothing is wrong — the agent simply declared no credential.
	name := r.Header.Get(credHeader)
	if grant := r.Header.Get(grantHeader); grant != "" && name != "" {
		if tok, err := p.credential(r.Context(), grant, name); err != nil {
			authed = "failed:" + name
			log.Printf("credential %q unavailable (%v) — forwarding unauthenticated to %s", name, err, host)
		} else {
			// Git over HTTPS uses Basic auth; the username is ignored by GitHub
			// when the password is a token.
			out.SetBasicAuth("x-access-token", tok)
			authed = name
		}
	} else if name == "" {
		// No credential declared: a public repo, which needs none.
		authed = "public"
	} else {
		// A credential was named but no grant arrived, so nothing identified the
		// caller. Worth logging: a private repo will 401 and git will report
		// something unhelpful about usernames.
		authed = "no-grant"
	}

	resp, err := p.client.Do(out)
	// One line per request: without it, "did my credential get attached?" is
	// unanswerable, which is exactly where the first live test got stuck.
	// Never logs the value — only which credential was used.
	if err == nil {
		log.Printf("%s https://%s%s → %d (credential=%s)", r.Method, host, target.Path, resp.StatusCode, authed)
	}
	if err != nil {
		log.Printf("upstream %s: %v (credential=%s)", host, err, authed)
		http.Error(w, "upstream unreachable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	copyHeaders(w.Header(), resp.Header)
	// The upstream may 401 with a challenge; passing it through unchanged would
	// make git prompt for a password it can never supply, and hang.
	w.Header().Del("WWW-Authenticate")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// credential exchanges a session grant for a secret via serve, which owns the
// vault. The value exists only in this process's memory, for one request.
func (p *proxy) credential(ctx context.Context, grant, name string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/v1/hand/credentials/%s", p.serveBase, url.PathEscape(name)), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+grant)
	resp, err := p.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("serve returned %d", resp.StatusCode)
	}
	var payload struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&payload); err != nil {
		return "", err
	}
	if payload.Value == "" {
		return "", errors.New("empty credential")
	}
	return payload.Value, nil
}

// hopByHop headers are per-connection and must not be forwarded (RFC 7230).
var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailer": true,
	"transfer-encoding": true, "upgrade": true,
}

func copyHeaders(dst, src http.Header) {
	for k, vs := range src {
		if hopByHop[strings.ToLower(k)] {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}
