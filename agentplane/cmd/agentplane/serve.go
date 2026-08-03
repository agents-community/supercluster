package main

// `agentplane serve` — the HTTP control-plane API (strategy deliverable #6).
// Everything the CLI does over port-forwards, exposed as one hardened endpoint
// so clients need no kubectl, no Host-header tricks, no gRPC.
//
//	GET    /healthz
//	GET    /v1/agents                     agents + phase + live session counts
//	POST   /v1/sessions                   {"agent":"brain"} → 201 {"id":"sess-…"}
//	GET    /v1/sessions                   derived-status list
//	GET    /v1/sessions/{id}              session object (status, busy, queued, …)
//	DELETE /v1/sessions/{id}              cascade: escrow → delete → snapshots
//	POST   /v1/sessions/{id}/events       send (5xx retry — finding #3)
//	GET    /v1/sessions/{id}/events       persisted log  [?since=evt_…]
//	GET    /v1/sessions/{id}/events/stream  SSE (proxied; Last-Event-ID honored)
//
// Hardening: optional bearer auth (AGENTPLANE_TOKEN), body caps, per-request
// timeouts, JSON errors, graceful shutdown — and OpenTelemetry from birth:
// serve is the TRACE ROOT. Every request gets a span; outbound calls carry
// traceparent, which atenet's Envoy joins (its OTLP tracer) and forwards to
// the brain actor. Export via the standard OTEL_EXPORTER_OTLP_ENDPOINT.

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"

	"github.com/quantumnode/agentplane/internal/agentspec"
	"github.com/quantumnode/agentplane/internal/naming"
)

const maxBody = 1 << 20 // 1 MiB — a user.message, not an upload API

// tokenStore holds the valid bearer tokens, each mapped to a user label.
// Tokens are opaque random strings; verification is a constant-time lookup
// ("is this token in the set?"), the same model as an API key. The set is
// loaded from a JSON file (a mounted Secret, `{"user":"token",…}`) and
// hot-reloaded, so issuing/revoking a token takes effect without a restart.
// A single AGENTPLANE_TOKEN (labeled "default") is also honored for dev.
type tokenStore struct {
	mu      sync.RWMutex
	byToken map[string]string // token → user
	file    string
}

func newTokenStore() *tokenStore {
	ts := &tokenStore{byToken: map[string]string{}, file: os.Getenv("AGENTPLANE_TOKENS_FILE")}
	ts.reload()
	return ts
}

// reload rebuilds the token set from the file (+ the single-token env var).
func (ts *tokenStore) reload() {
	next := map[string]string{}
	if ts.file != "" {
		if data, err := os.ReadFile(ts.file); err == nil {
			var m map[string]string // user → token
			if json.Unmarshal(data, &m) == nil {
				for user, tok := range m {
					if tok != "" {
						next[tok] = user
					}
				}
			}
		}
	}
	if t := os.Getenv("AGENTPLANE_TOKEN"); t != "" {
		next[t] = "default"
	}
	ts.mu.Lock()
	ts.byToken = next
	ts.mu.Unlock()
}

// enabled reports whether any token is configured (auth on).
func (ts *tokenStore) enabled() bool {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	return len(ts.byToken) > 0
}

// user returns the label for a presented token, or ("", false). Constant-time
// against every entry (no early exit) so timing doesn't leak which matched.
func (ts *tokenStore) user(presented string) (string, bool) {
	ts.mu.RLock()
	defer ts.mu.RUnlock()
	user, ok := "", false
	for tok, u := range ts.byToken {
		if subtle.ConstantTimeCompare([]byte(presented), []byte(tok)) == 1 {
			user, ok = u, true
		}
	}
	return user, ok
}

type server struct {
	sc       sessionCtx
	tokens   *tokenStore  // per-user bearer auth
	client   *http.Client // outbound to atenet, trace-propagating
	log      *slog.Logger
	wakeHist metric.Float64Histogram // session wake/accept latency (cold-start KPI); nil when metrics off
	vault    *vault                  // credential vault (Secret Manager); nil when unconfigured
	grantKey []byte                  // HMAC key for stateless hand-pull grants (= HAND_ADMIN_TOKEN)
	emails   *emailAllowlist         // self-service /v1/access: emails allowed to self-issue a token
	owners   sessionStore            // session ownership + durable metadata (F1, #36)
	agents   *fsStore                // agent versioning (#32); nil without Firestore
	limiter  *accessLimiter          // throttles unauthenticated /v1/access (F7)
}

func runServe(args []string) {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", ":7433", "listen address")
	_ = fs.Parse(args)

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	shutdownTracing := initTracing(logger)
	shutdownMetrics, wakeHist := initMetrics(logger)

	tokens := newTokenStore()
	emails := newEmailAllowlist()
	// Hot-reload the token set and the email allowlist (mounted Secret/ConfigMap
	// files refresh ~every 60s), so issue/revoke and allowlist edits take effect
	// without a restart.
	go func() {
		for range time.Tick(15 * time.Second) {
			tokens.reload()
			emails.reload()
		}
	}()

	// Credential vault (Secret Manager) — optional; its endpoints return 503
	// until AGENTPLANE_PROJECT is set and the serve SA can access secrets.
	var v *vault
	if vv, err := newVault(context.Background(), env("AGENTPLANE_PROJECT", os.Getenv("GOOGLE_CLOUD_PROJECT"))); err != nil {
		logger.Warn("credential vault disabled", "err", err)
	} else {
		v = vv
		logger.Info("credential vault enabled", "project", vv.project)
	}

	s := &server{
		sc:     newSessionCtx(),
		tokens: tokens,
		// Propagates traceparent so atenet's Envoy (and the actor) join our trace.
		client:   &http.Client{Transport: otelhttp.NewTransport(http.DefaultTransport)},
		log:      logger,
		wakeHist: wakeHist,
		vault:    v,
		grantKey: grantSigningKey(),
		emails:   emails,
		owners: newSessionStore(context.Background(),
			env("AGENTPLANE_PROJECT", os.Getenv("GOOGLE_CLOUD_PROJECT")),
			env("BRAIN_TEMPLATE_NS", "agentplane"), logger),
		limiter: newAccessLimiter(),
	}

	// Versioning needs the concrete store; on the ConfigMap fallback it stays
	// nil and agent creation keeps its original create-once behavior.
	if fs, ok := s.owners.(*fsStore); ok {
		s.agents = fs
	}
	// Let the suspend/delete paths persist usage before a mind sleeps or dies
	// (#42). Package-level because those paths are shared with the CLI, which
	// has no store.
	captureUsage = func(ctx context.Context, sid string, u json.RawMessage) {
		s.owners.touch(ctx, sid, u)
	}

	mux := http.NewServeMux()
	// Liveness: this process is up. Deliberately dependency-free — a failing
	// dependency must not get the pod killed and restarted, which fixes nothing.
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	})
	// Readiness: can we actually serve? /healthz returned 200 throughout a total
	// outage (valkey CLUSTERDOWN → every actor call failing), because listening
	// is not the same as working. This exercises the real chain — serve → ateapi
	// → valkey — and names the component that failed.
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	// Self-service onboarding: an allowlisted email exchanges itself for a token.
	// Unauthenticated (it's how you get your first token); gated by the allowlist.
	mux.HandleFunc("POST /v1/access", s.handleAccess)
	mux.HandleFunc("GET /v1/agents", s.auth(s.handleAgentList))
	mux.HandleFunc("POST /v1/agents", s.auth(s.handleAgentCreate))
	mux.HandleFunc("GET /v1/agents/{id}/versions", s.auth(s.handleAgentVersions))
	mux.HandleFunc("DELETE /v1/agents/{id}", s.auth(s.handleAgentDelete))
	mux.HandleFunc("POST /v1/sessions", s.auth(s.handleSessionCreate))
	mux.HandleFunc("GET /v1/sessions", s.auth(s.handleSessionList))
	mux.HandleFunc("GET /v1/sessions/{id}", s.auth(s.handleSessionGet))
	mux.HandleFunc("DELETE /v1/sessions/{id}", s.auth(s.handleSessionDelete))
	mux.HandleFunc("POST /v1/sessions/{id}/suspend", s.auth(s.handleSessionSuspend))
	mux.HandleFunc("PUT /v1/sessions/{id}/key", s.auth(s.handleSessionKey))
	// Credential vault (client-facing; value is write-only).
	mux.HandleFunc("PUT /v1/credentials/{name}", s.auth(s.handleCredPut))
	mux.HandleFunc("GET /v1/credentials", s.auth(s.handleCredList))
	mux.HandleFunc("DELETE /v1/credentials/{name}", s.auth(s.handleCredDelete))
	// Hand-pull: the HAND presents its session grant (not a user token) to fetch
	// a credential value. Grant-authed inside the handler, so NOT wrapped in auth.
	mux.HandleFunc("GET /v1/hand/credentials/{name}", s.handleHandCredPull)
	// Talk to a session: POST a message, GET /message/stream to watch it work
	// (assistant text + tool activity, harness-neutral). GET /message is history.
	mux.HandleFunc("POST /v1/sessions/{id}/message", s.auth(s.handleSend))
	mux.HandleFunc("GET /v1/sessions/{id}/message", s.auth(s.handleEvents))
	mux.HandleFunc("GET /v1/sessions/{id}/message/stream", s.auth(s.handleStream))
	// Deprecated aliases (kept for compatibility; prefer /message).
	mux.HandleFunc("POST /v1/sessions/{id}/events", s.auth(s.handleSend))
	mux.HandleFunc("GET /v1/sessions/{id}/events", s.auth(s.handleEvents))
	mux.HandleFunc("GET /v1/sessions/{id}/events/stream", s.auth(s.handleStream))

	srv := &http.Server{
		Addr: *addr,
		// otelhttp names the root span after the mux route (http.route).
		Handler:           otelhttp.NewHandler(mux, "agentplane.serve"),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		// No WriteTimeout: /events/stream is long-lived SSE by design;
		// non-stream handlers bound themselves with request-context timeouts.
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shCtx)
	}()

	logger.Info("serve: listening", "addr", *addr, "auth", s.tokens.enabled(),
		"otlp", os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"))
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("serve: %v", err)
	}
	shutdownMetrics()
	shutdownTracing()
	logger.Info("serve: stopped cleanly")
}

// initTracing wires the OTLP exporter when OTEL_EXPORTER_OTLP_ENDPOINT is set;
// otherwise tracing is a no-op. serve is the edge, so it samples ALWAYS —
// Substrate's services run ParentBased(NeverSample) and follow our decision.
func initTracing(logger *slog.Logger) func() {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		otel.SetTextMapPropagator(propagation.TraceContext{})
		return func() {}
	}
	exp, err := otlptracegrpc.New(context.Background(), otlptracegrpc.WithInsecure())
	if err != nil {
		logger.Warn("serve: OTLP exporter init failed — tracing disabled", "err", err)
		return func() {}
	}
	res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL, semconv.ServiceName("agentplane-serve")))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.AlwaysSample())),
	)
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = tp.Shutdown(ctx)
	}
}

// initMetrics wires an OTLP metric exporter and the cold-start histogram when
// OTEL_EXPORTER_OTLP_ENDPOINT is set; otherwise metrics are a no-op. This is
// the PRODUCT cold-start signal — Substrate's atenet.router.route.duration
// covers request→worker-resolved, but stops before the mind actually responds.
func initMetrics(logger *slog.Logger) (func(), metric.Float64Histogram) {
	if os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT") == "" {
		return func() {}, nil
	}
	exp, err := otlpmetricgrpc.New(context.Background(), otlpmetricgrpc.WithInsecure())
	if err != nil {
		logger.Warn("serve: OTLP metric exporter init failed — metrics disabled", "err", err)
		return func() {}, nil
	}
	res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(
		semconv.SchemaURL, semconv.ServiceName("agentplane-serve")))
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithReader(sdkmetric.NewPeriodicReader(exp)),
		sdkmetric.WithResource(res),
	)
	otel.SetMeterProvider(mp)
	hist, err := mp.Meter("agentplane-serve").Float64Histogram(
		"agentplane.session.wake_latency",
		metric.WithUnit("s"),
		metric.WithDescription("Seconds for a session to ACCEPT a message. High values (and cold=true) are wakes from a checkpoint — the durable-mind cold-start KPI."),
		metric.WithExplicitBucketBoundaries(0.05, 0.1, 0.25, 0.5, 1, 2, 3, 5, 8, 13, 21, 30),
	)
	if err != nil {
		logger.Warn("serve: wake histogram init failed", "err", err)
		hist = nil
	}
	return func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = mp.Shutdown(ctx)
	}, hist
}

// handleReadyz reports whether serve can actually reach the control plane.
// Unauthenticated on purpose: it exposes no data, and a probe that needs a
// token is one more thing that can fail for reasons unrelated to health.
func (s *server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	ctrl, closeFn, err := s.sc.dial()
	if err != nil {
		s.notReady(w, "ateapi", "cannot dial the control plane", err)
		return
	}
	defer closeFn()

	// ListActors is the cheapest call that proves the whole chain: it
	// authenticates to ateapi and reads the registry, which lives in valkey.
	if _, err := ctrl.ListActors(ctx, &ateapipb.ListActorsRequest{}); err != nil {
		s.notReady(w, failedComponent(err), "control plane is not serving", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ready": true})
}

// failedComponent maps a control-plane error to the thing an operator should
// go look at, so a 503 points somewhere instead of needing a bisect.
func failedComponent(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "CLUSTERDOWN"), strings.Contains(msg, "shard"):
		return "valkey"
	case strings.Contains(msg, "Unauthenticated"):
		return "ateapi-auth"
	case strings.Contains(msg, "protojson"), strings.Contains(msg, "unknown field"):
		return "schema-drift" // stored records the running binary cannot parse
	}
	return "ateapi"
}

// notReady answers 503 naming the broken component; the underlying error is
// logged server-side and never returned, like every other error here.
func (s *server) notReady(w http.ResponseWriter, component, msg string, err error) {
	s.log.Error("readiness check failed", "component", component, "err", err)
	writeJSON(w, http.StatusServiceUnavailable, map[string]any{
		"ready": false, "component": component, "message": msg,
	})
}

// ---------- middleware & helpers ----------

type userKey struct{}

// userOf returns the authenticated caller label for logging/attribution.
func userOf(r *http.Request) string {
	if u, ok := r.Context().Value(userKey{}).(string); ok {
		return u
	}
	return "-"
}

func (s *server) auth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.tokens.enabled() {
			presented := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			user, ok := s.tokens.user(presented)
			if !ok {
				writeErr(w, http.StatusUnauthorized, "missing or invalid bearer token")
				return
			}
			r = r.WithContext(context.WithValue(r.Context(), userKey{}, user))
		}
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// codeForStatus gives a stable machine-readable slug per HTTP status, so
// clients can branch on `error.code` without parsing prose.
func codeForStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusBadGateway:
		return "backend_error"
	case http.StatusGatewayTimeout:
		return "timeout"
	default:
		return "error"
	}
}

// writeErr sends a sanitized, stable error: {"error":{"code","message"}}. `msg`
// MUST be safe for clients — never a raw internal error string.
func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]string{"code": codeForStatus(status), "message": msg}})
}

// fail logs the internal detail server-side and returns a sanitized response,
// so clients never see kubectl/gcloud/gRPC internals. Use for backend errors.
func (s *server) fail(w http.ResponseWriter, r *http.Request, status int, clientMsg string, internal error) {
	s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "status", status, "err", internal)
	writeErr(w, status, clientMsg)
}

// pathSession validates {id} and returns (sid, brain). Writes the error itself.
func pathSession(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	sid := r.PathValue("id")
	if !naming.IsSessionID(sid) {
		writeErr(w, http.StatusBadRequest, "invalid session id (want sess-…)")
		return "", "", false
	}
	return sid, naming.BrainActor(sid), true
}

// ownedSession is pathSession + the ownership check every session-scoped route
// must pass (threat-model F1). A session owned by someone else answers 404,
// exactly like a nonexistent one, so ids cannot be enumerated by probing.
func (s *server) ownedSession(w http.ResponseWriter, r *http.Request) (string, string, bool) {
	sid, brain, ok := pathSession(w, r)
	if !ok {
		return "", "", false
	}
	if s.tokens.enabled() && !s.owners.mine(sid, userOf(r)) {
		s.log.Warn("session access denied", "session", sid, "user", userOf(r)) // audit
		writeErr(w, http.StatusNotFound, "no such session")
		return "", "", false
	}
	return sid, brain, true
}

// brainReq builds a trace-propagating request routed to a brain via atenet.
func (s *server) brainReq(ctx context.Context, method, brain, path string, body io.Reader) *http.Request {
	req, _ := http.NewRequestWithContext(ctx, method,
		fmt.Sprintf("http://%s%s", s.sc.atenet, path), body)
	req.Host = naming.ActorDNS(brain, s.sc.atespace)
	return req
}

func span(r *http.Request) trace.Span { return trace.SpanFromContext(r.Context()) }

// ---------- handlers ----------

func (s *server) handleAgentList(w http.ResponseWriter, r *http.Request) {
	agents, err := listAgents(r.Context(), s.sc)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to list agents", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

// handleAgentCreate accepts an AgentSpec (YAML or JSON) and applies it.
// Returns 202 immediately — the golden bake takes ~30s; poll GET /v1/agents
// until phase is Ready. Sync-wait here would be a worse API than polling.
func (s *server) handleAgentCreate(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read request body")
		return
	}
	// Spec-parse/validation messages are safe (they describe the user's input),
	// so they are surfaced directly.
	spec, err := agentspec.Parse(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// A name that looks like a versioned template would collide with a real
	// version of the same agent — same template name, different spec.
	if versionSuffixRe.MatchString(spec.Name) {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf(
			"agent name %q ends in a version suffix, which is reserved for agent versions", spec.Name))
		return
	}
	if s.agents == nil {
		s.createAgentUnversioned(w, r, spec, raw)
		return
	}

	// Versioned path: re-creating an existing agent mints the NEXT version
	// rather than failing. Prior templates are left alone, so every running
	// session keeps answering on the template it was minted from — updating an
	// agent used to require a cascade delete that destroyed all of them (#32).
	version, err := s.agents.nextVersion(r.Context(), spec.Name, userOf(r))
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to reserve an agent version", err)
		return
	}
	template := templateFor(spec.Name, version)
	tmpl, err := spec.CompileVersion(s.sc.templateNS, os.Getenv("AGENTPLANE_BUCKET"), template, version)
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "failed to compile agent spec", err)
		return
	}
	if err := applyAgent(r.Context(), s.sc, template, tmpl); err != nil {
		// v1 already existing means an agent created before versioning: adopt it
		// as version 1 instead of failing, so pre-existing agents stay usable.
		if version == 1 && strings.Contains(err.Error(), "already exists") {
			s.log.Info("adopted pre-existing agent as version 1", "agent", spec.Name)
		} else {
			s.fail(w, r, http.StatusBadGateway, "failed to create agent", err)
			return
		}
	}
	// Record only AFTER the template applies: a version in the history that has
	// no template behind it would resolve to a session that cannot start.
	if err := s.agents.recordVersion(r.Context(), spec.Name, agentVersion{
		Version: version, Spec: string(raw), Template: template,
		Harness: spec.Harness, CreatedBy: userOf(r), CreatedAt: time.Now().UTC(),
	}); err != nil {
		s.log.Error("agent version recorded in Substrate but not in the store",
			"agent", spec.Name, "version", version, "err", err)
	}
	span(r).SetAttributes(
		attribute.String("agentplane.agent", spec.Name),
		attribute.Int("agentplane.agent_version", version))
	s.log.Info("agent version created", "agent", spec.Name, "version", version,
		"template", template, "harness", spec.Harness)
	writeJSON(w, http.StatusAccepted, map[string]any{
		"name": spec.Name, "harness": spec.Harness, "version": version,
		"template": template, "phase": "Pending",
		"note": "golden bake in progress — poll GET /v1/agents until Ready. " +
			"Existing sessions keep running on their own version.",
	})
}

// createAgentUnversioned is the original create-once behavior, used when no
// Firestore project is configured (local/dev clusters).
func (s *server) createAgentUnversioned(w http.ResponseWriter, r *http.Request, spec *agentspec.AgentSpec, _ []byte) {
	tmpl, err := spec.CompileTemplate(s.sc.templateNS, os.Getenv("AGENTPLANE_BUCKET"))
	if err != nil {
		s.fail(w, r, http.StatusInternalServerError, "failed to compile agent spec", err)
		return
	}
	if err := applyAgent(r.Context(), s.sc, spec.Name, tmpl); err != nil {
		if strings.Contains(err.Error(), "already exists") {
			writeErr(w, http.StatusConflict, fmt.Sprintf(
				"agent %q already exists (versioning needs AGENTPLANE_PROJECT)", spec.Name))
			return
		}
		s.fail(w, r, http.StatusBadGateway, "failed to create agent", err)
		return
	}
	span(r).SetAttributes(attribute.String("agentplane.agent", spec.Name))
	s.log.Info("agent created", "agent", spec.Name, "harness", spec.Harness)
	writeJSON(w, http.StatusAccepted, map[string]string{
		"name": spec.Name, "harness": spec.Harness,
		"phase": "Pending", "note": "golden bake in progress — poll GET /v1/agents until Ready",
	})
}

// handleAgentVersions lists an agent's version history, newest first.
func (s *server) handleAgentVersions(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("id")
	if !naming.IsAgentName(name) {
		writeErr(w, http.StatusBadRequest, "invalid agent name")
		return
	}
	if s.agents == nil {
		writeErr(w, http.StatusNotImplemented, "agent versioning requires AGENTPLANE_PROJECT")
		return
	}
	vs, err := s.agents.versions(r.Context(), name)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to list agent versions", err)
		return
	}
	if len(vs) == 0 {
		writeErr(w, http.StatusNotFound, fmt.Sprintf("no agent %q", name))
		return
	}
	type item struct {
		Version   int    `json:"version"`
		Template  string `json:"template"`
		Harness   string `json:"harness"`
		CreatedBy string `json:"created_by"`
		CreatedAt string `json:"created_at"`
		Spec      string `json:"spec"`
	}
	out := make([]item, 0, len(vs))
	for _, v := range vs {
		out = append(out, item{
			Version: v.Version, Template: v.Template, Harness: v.Harness,
			CreatedBy: v.CreatedBy, CreatedAt: v.CreatedAt.Format(time.RFC3339),
			Spec: v.Spec,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": name, "versions": out})
}

// handleAgentDelete mirrors `agent delete`: 409 with the stranded-session list
// unless ?cascade=true (finding #4 — deleting the template strands sessions).
// {id} is the agent's name — same convention as /v1/sessions/{id}.
func (s *server) handleAgentDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("id")
	if !naming.IsAgentName(name) {
		writeErr(w, http.StatusBadRequest, "invalid agent name")
		return
	}
	cascade := r.URL.Query().Get("cascade") == "true"

	// Every version is its own template, and sessions may be pinned to ANY of
	// them — an older version still serving minds is precisely why its template
	// survived the update. Delete newest-first so the agent's own name goes
	// last: if an intermediate delete fails, `starter` still exists and the
	// whole operation stays retryable instead of half-applied.
	var templates []string
	if s.agents != nil {
		if vs, err := s.agents.versions(r.Context(), name); err == nil {
			for _, v := range vs { // already newest-first
				if v.Template != name {
					templates = append(templates, v.Template)
				}
			}
		}
	}
	var cascaded []string
	for _, t := range templates {
		sids, err := deleteAgent(r.Context(), s.sc, t, cascade)
		var hs errHasSessions
		if errors.As(err, &hs) {
			// Report against the agent the caller asked about, not the internal
			// version template they never named.
			writeJSON(w, http.StatusConflict, map[string]any{
				"error": map[string]string{"code": "conflict",
					"message": "agent has live sessions on an earlier version — retry with ?cascade=true"},
				"sessions": hs.Sessions, "version_template": t,
			})
			return
		}
		if err != nil && !strings.Contains(err.Error(), "no agent") {
			s.fail(w, r, http.StatusBadGateway, "failed to delete an agent version", err)
			return
		}
		cascaded = append(cascaded, sids...)
	}

	sids, err := deleteAgent(r.Context(), s.sc, name, cascade)
	sids = append(sids, cascaded...)
	if err == nil && s.agents != nil {
		s.agents.forgetAgent(r.Context(), name) // history goes with the agent
	}
	var hasSess errHasSessions
	switch {
	case errors.As(err, &hasSess):
		writeJSON(w, http.StatusConflict, map[string]any{
			"error":    map[string]string{"code": "conflict", "message": "agent has live sessions — retry with ?cascade=true"},
			"sessions": hasSess.Sessions,
		})
	case err != nil && strings.Contains(err.Error(), "no agent"):
		writeErr(w, http.StatusNotFound, fmt.Sprintf("no agent %q", name))
	case err != nil:
		s.fail(w, r, http.StatusBadGateway, "failed to delete agent", err)
	default:
		s.log.Info("agent deleted", "agent", name, "cascaded", len(sids))
		writeJSON(w, http.StatusOK, map[string]any{"deleted": name, "sessions_removed": sids})
	}
}

func (s *server) handleSessionCreate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Agent   string `json:"agent"`
		Version int    `json:"version"` // pin a specific agent version (#32); 0 = latest
		APIKey  string `json:"apiKey"`  // BYO-key: ephemeral, forwarded to the brain, never stored/logged
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil && !errors.Is(err, io.EOF) {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if in.Agent == "" {
		in.Agent = s.sc.template
	}
	if !naming.IsAgentName(in.Agent) {
		writeErr(w, http.StatusBadRequest, "invalid agent name")
		return
	}
	// Resolve the agent to a concrete template. New sessions take the latest
	// version; an explicit version reproduces an earlier definition exactly.
	// Templates other than the newest are never deleted while sessions pin
	// them, which is what lets an agent be updated without killing minds.
	template, version := in.Agent, 0
	if s.agents != nil {
		if in.Version > 0 {
			t, err := s.agents.templateForVersion(r.Context(), in.Agent, in.Version)
			if err != nil {
				writeErr(w, http.StatusNotFound, fmt.Sprintf(
					"agent %q has no version %d", in.Agent, in.Version))
				return
			}
			template, version = t, in.Version
		} else {
			template, version = s.agents.latestTemplate(r.Context(), in.Agent)
		}
	}
	sid, err := createSession(r.Context(), s.sc, template, userOf(r), s.vault)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to create session", err)
		return
	}
	if in.APIKey != "" {
		if err := s.putBrainKey(r.Context(), naming.BrainActor(sid), in.APIKey); err != nil {
			s.fail(w, r, http.StatusBadGateway, "session created but setting the key failed — retry PUT /v1/sessions/{id}/key", err)
			return
		}
	}
	// Grant the hand a scoped pull for this agent's declared credentials (if any).
	// Best-effort: the session is usable without it; the hand just won't have the
	// credential until re-granted. Must precede the first message.
	// Claim ownership BEFORE anything else can touch the session (F1).
	if err := s.owners.claim(r.Context(), sid, userOf(r), in.Agent, version); err != nil {
		s.log.Error("owner claim failed — session left unowned", "session", sid, "user", userOf(r), "err", err)
	}
	if err := s.grantHandCredentials(r.Context(), sid, userOf(r), in.Agent); err != nil {
		s.log.Warn("hand credential grant failed", "session", sid, "err", err)
	}
	harness := ""
	if hm, err := templateHarnesses(r.Context(), s.sc); err == nil {
		harness = hm[in.Agent]
	}
	span(r).SetAttributes(attribute.String("agentplane.session", sid), attribute.String("agentplane.agent", in.Agent))
	s.log.Info("session created", "session", sid, "agent", in.Agent, "harness", harness, "user", userOf(r))
	writeJSON(w, http.StatusCreated, map[string]string{"id": sid, "agent": in.Agent, "harness": harness})
}

func (s *server) handleSessionList(w http.ResponseWriter, r *http.Request) {
	// Only the caller's own sessions (F1). Filtering happens below, per actor.
	me, filtering := userOf(r), s.tokens.enabled()
	actors, err := listSessionActors(r.Context(), s.sc, "")
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to list sessions", err)
		return
	}
	harnesses, _ := templateHarnesses(r.Context(), s.sc) // best-effort enrich
	type item struct {
		ID      string `json:"id"`
		Agent   string `json:"agent"`
		Harness string `json:"harness"`
		Status  string `json:"status"`
	}
	logicalAgents, _ := templateAgents(r.Context(), s.sc) // best-effort enrich
	items := []item{}
	for _, a := range actors {
		name := a.GetMetadata().GetName()
		sid, _ := naming.SessionFromActor(name)
		if filtering && !s.owners.mine(sid, me) {
			continue // not yours — not listed (F1)
		}
		// Same rule as GET: the logical agent, not the version template.
		tmplName := a.GetActorTemplateName()
		agent := tmplName
		if logical, ok := logicalAgents[tmplName]; ok && logical != "" {
			agent = logical
		}
		items = append(items, item{ID: sid, Agent: agent, Harness: harnesses[tmplName],
			Status: derivedStatus(s.sc, name, a.GetStatus().String())})
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": items})
}

func (s *server) handleSessionGet(w http.ResponseWriter, r *http.Request) {
	sid, brain, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	actors, err := listSessionActors(r.Context(), s.sc, "")
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to look up session", err)
		return
	}
	harnesses, _ := templateHarnesses(r.Context(), s.sc) // best-effort enrich
	for _, a := range actors {
		if a.GetMetadata().GetName() != brain {
			continue
		}
		// Report the LOGICAL agent, never the version template. A session minted
		// from `starter` runs on `starter-v2`, and a client filtering its own
		// sessions by agent name would match nothing (#32 regression).
		tmplName := a.GetActorTemplateName()
		agentName := tmplName
		if agents, err := templateAgents(r.Context(), s.sc); err == nil {
			if logical, ok := agents[tmplName]; ok && logical != "" {
				agentName = logical
			}
		}
		out := map[string]any{"id": sid, "agent": agentName, "harness": harnesses[tmplName]}
		// Stored metadata first: it is readable while the mind SLEEPS, which the
		// live probe below is not (probing resumes a suspended actor, undoing
		// the auto-sleep that just saved the worker). A sleeping session used to
		// report neither last_event_at nor usage at all — see #36.
		if m, found := s.owners.get(r.Context(), sid); found {
			if !m.LastActiveAt.IsZero() {
				out["last_event_at"] = m.LastActiveAt.Format(time.RFC3339)
			}
			if len(m.Usage) > 0 {
				out["usage"] = m.Usage
			}
			if !m.CreatedAt.IsZero() {
				out["created_at"] = m.CreatedAt.Format(time.RFC3339)
			}
			if m.Title != "" {
				out["title"] = m.Title
			}
			if m.Version > 0 {
				out["version"] = m.Version // the agent version this mind runs
			}
			if m.Agent != "" {
				agentName = m.Agent // the store knows the name the user asked for
				out["agent"] = agentName
			}
		}
		// Probe /healthz at most ONCE, and only when awake (probing wakes a
		// sleeping mind). Live values supersede the stored ones for an awake
		// mind, since the harness is authoritative while it is running.
		if a.GetStatus().String() == "STATUS_RUNNING" {
			if h, ok := probeHealth(s.sc, brain); ok {
				out["busy"], out["queued"] = h.Busy, h.Queued
				if h.LastEventAt != "" {
					out["last_event_at"] = h.LastEventAt
				}
				if len(h.Usage) > 0 {
					out["usage"] = h.Usage
					// Persist so this survives the next suspend, and the delete
					// after it — usage used to die with the actor.
					s.owners.touch(r.Context(), sid, h.Usage)
				}
				if h.Busy {
					out["status"] = "running"
				} else {
					out["status"] = "idle"
				}
			} else {
				out["status"] = "unreachable"
			}
		} else {
			out["status"] = derivedStatus(s.sc, brain, a.GetStatus().String())
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	writeErr(w, http.StatusNotFound, "no such session")
}

func (s *server) handleSessionDelete(w http.ResponseWriter, r *http.Request) {
	sid, brain, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	if err := deleteSession(r.Context(), s.sc, sid, brain); err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to delete session", err)
		return
	}
	s.owners.release(r.Context(), sid) // forget ownership with the session (F1)
	s.log.Info("session deleted", "session", sid, "user", userOf(r))
	writeJSON(w, http.StatusOK, map[string]string{"deleted": sid})
}

// handleSessionSuspend checkpoints a mind over plain HTTP (the TUI's Ctrl+S).
// Same safe-suspend guard as the CLI: refuses mid-turn unless ?force=true
// (finding #1 — a mid-turn checkpoint can wedge the turn).
func (s *server) handleSessionSuspend(w http.ResponseWriter, r *http.Request) {
	sid, brain, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	if r.URL.Query().Get("force") != "true" {
		if h, ok := probeHealth(s.sc, brain); ok && h.Busy {
			writeErr(w, http.StatusConflict, "turn in flight — wait for idle or retry with ?force=true")
			return
		}
	}
	ctrl, closeFn, err := s.sc.dial()
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to suspend session", err)
		return
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	// Capture cost while the mind is still awake — after the checkpoint it can
	// only be read by resuming it (#42).
	recordUsageIfAwake(ctx, ctrl, s.sc, sid, brain)
	escrowTranscript(s.sc, sid, brain) // escrow-before-checkpoint, best-effort
	if _, err := ctrl.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
		Actor: &ateapipb.ObjectRef{Atespace: s.sc.atespace, Name: brain},
	}); err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to suspend session", err)
		return
	}
	s.log.Info("session suspended", "session", sid)
	writeJSON(w, http.StatusOK, map[string]string{"suspended": sid})
}

// handleSessionKey sets/replaces the ephemeral BYO-key on an existing session.
func (s *server) handleSessionKey(w http.ResponseWriter, r *http.Request) {
	sid, brain, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	var in struct {
		APIKey string `json:"apiKey"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, maxBody)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if err := s.putBrainKey(r.Context(), brain, in.APIKey); err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to set session key", err)
		return
	}
	s.log.Info("session key set", "session", sid) // logs the ACT, never the key
	writeJSON(w, http.StatusOK, map[string]string{"keyed": sid})
}

// putBrainKey forwards an ephemeral key to a brain over atenet. The key is
// never logged, never persisted server-side — it lives only in the actor's
// memory (BYO-key, Option 1). Retries wake races (finding #3).
func (s *server) putBrainKey(ctx context.Context, brain, apiKey string) error {
	body, _ := json.Marshal(map[string]string{"apiKey": apiKey})
	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		req := s.brainReq(ctx, http.MethodPost, brain, "/v1/sessions/"+brain+"/key", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.client.Do(req)
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			if resp.StatusCode >= 400 {
				return fmt.Errorf("brain rejected key: HTTP %d", resp.StatusCode)
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
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return lastErr
}

func (s *server) handleSend(w http.ResponseWriter, r *http.Request) {
	sid, brain, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read request body")
		return
	}
	span(r).SetAttributes(attribute.String("agentplane.session", sid))
	// Finding #3: sends during a mind's first resume can 5xx; the append is
	// idempotent-safe to retry. Keep the SUCCESSFUL response open to relay;
	// close only responses we're discarding (avoids reading a closed body).
	// The wall-time to ACCEPT is our cold-start KPI: a sleeping mind's first
	// send blocks on the checkpoint restore (and may retry a wake race).
	start := time.Now()
	waking := false
	var resp *http.Response
	for attempt := 1; attempt <= 4; attempt++ {
		req := s.brainReq(r.Context(), http.MethodPost, brain,
			"/v1/sessions/"+brain+"/events", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		resp, err = s.client.Do(req)
		if err == nil && resp.StatusCode < 500 {
			break // relayable (success or client error)
		}
		if resp != nil {
			resp.Body.Close()
			resp = nil
		}
		if attempt < 4 {
			waking = true
			s.log.Warn("send retry", "session", sid, "attempt", attempt)
			select {
			case <-time.After(5 * time.Second):
			case <-r.Context().Done():
				writeErr(w, http.StatusGatewayTimeout, "cancelled during retry")
				return
			}
		}
	}
	if s.wakeHist != nil && err == nil && resp != nil {
		// cold = the send observably waited on a wake (retried a race, or the
		// single accept took longer than a warm actor ever would).
		cold := waking || time.Since(start) > time.Second
		s.wakeHist.Record(r.Context(), time.Since(start).Seconds(), metric.WithAttributes(attribute.Bool("cold", cold)))
	}
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to deliver message to the session", err)
		return
	}
	if resp == nil { // every attempt returned 5xx
		s.fail(w, r, http.StatusBadGateway, "session did not accept the message", nil)
		return
	}
	defer resp.Body.Close()
	// Stamp activity now that the message is accepted. Detached from the request
	// context so the write survives the client hanging up, and off the response
	// path so a slow store never delays the turn — losing a timestamp must not
	// cost a message (#36).
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		s.owners.touch(ctx, sid, nil)
	}()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, maxBody))
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	_, brain, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	path := "/v1/sessions/" + brain + "/events"
	if since := r.URL.Query().Get("since"); since != "" {
		path += "?since=" + url.QueryEscape(since) // escape: user-controlled
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	resp, err := s.client.Do(s.brainReq(ctx, http.MethodGet, brain, path, nil))
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to fetch events", err)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 16<<20))
}

// handleStream proxies the brain's SSE stream, flushing per chunk. Cursor
// resume: browser reconnects send Last-Event-ID; we forward it as ?since=.
func (s *server) handleStream(w http.ResponseWriter, r *http.Request) {
	_, brain, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	path := "/v1/sessions/" + brain + "/events/stream"
	if since := r.URL.Query().Get("since"); since != "" {
		path += "?since=" + url.QueryEscape(since)
	} else if last := r.Header.Get("Last-Event-ID"); last != "" {
		path += "?since=" + url.QueryEscape(last) // escape: client-controlled header
	}
	resp, err := s.client.Do(s.brainReq(r.Context(), http.MethodGet, brain, path, nil))
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "failed to open event stream", err)
		return
	}
	defer resp.Body.Close()
	fl, canFlush := w.(http.Flusher)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(resp.StatusCode)
	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if canFlush {
				fl.Flush()
			}
		}
		if err != nil {
			return // upstream closed or client gone
		}
	}
}

// derivedStatus maps actor status → user-facing state. Never probes SUSPENDED
// minds (probing wakes them). Same ladder as `session list`.
func derivedStatus(sc sessionCtx, brain, actorStatus string) string {
	st := strings.ToLower(strings.TrimPrefix(actorStatus, "STATUS_"))
	switch st {
	case "suspended":
		return "sleeping"
	case "running":
		if h, ok := probeHealth(sc, brain); !ok {
			return "unreachable"
		} else if h.Busy {
			return "running"
		}
		return "idle"
	}
	return st
}
