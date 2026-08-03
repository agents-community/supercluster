package main

// The `agentplane session` command group — v0 of the control plane. It owns the
// b-sess-<id> naming convention (internal/naming): sessions are minted here,
// actors are derived, and clients never invent names.
//
//	agentplane session new                      mint a session + create its brain actor
//	agentplane session list                     sessions (brain actors) + status
//	agentplane session send -id sess-… -m "…"   send a user.message
//	agentplane session tail -id sess-… [-since evt_…] [-for 30s]   SSE stream
//	agentplane session suspend -id sess-…       checkpoint the mind (dispatcher duty)
//	agentplane session delete -id sess-…        suspend + delete the brain actor

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/quantumnode/agentplane/internal/naming"
)

type sessionCtx struct {
	atespace   string
	templateNS string
	template   string
	ateapi     string
	atenet     string
}

func newSessionCtx() sessionCtx {
	return sessionCtx{
		atespace:   env("SUBSTRATE_ATESPACE", "agents"),
		templateNS: env("BRAIN_TEMPLATE_NS", "agentplane"),
		template:   env("BRAIN_TEMPLATE", "brain"),
		ateapi:     env("SUBSTRATE_ATEAPI", "localhost:8080"),
		atenet:     env("SUBSTRATE_ATENET", "localhost:8000"),
	}
}

// ateapiTLS builds the client TLS config for the Substrate control plane
// (threat-model F10). When AGENTPLANE_ATEAPI_CA points at a PEM bundle the
// server certificate is verified against it; without one we fall back to
// skipping verification (the historical behavior) and say so loudly, since
// that defeats the mTLS Substrate now runs between its components.
func ateapiTLS() *tls.Config {
	if p := os.Getenv("AGENTPLANE_ATEAPI_CA"); p != "" {
		if pem, err := os.ReadFile(p); err == nil {
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pem) {
				return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
			}
			log.Printf("WARN ateapi CA %s parsed no certificates — falling back to unverified TLS", p)
		} else {
			log.Printf("WARN ateapi CA %s unreadable (%v) — falling back to unverified TLS", p, err)
		}
	}
	insecureAteapiOnce.Do(func() {
		log.Printf("WARN ateapi TLS is UNVERIFIED (set AGENTPLANE_ATEAPI_CA to a PEM bundle to fix — threat-model F10)")
	})
	return &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12}
}

var insecureAteapiOnce sync.Once

// ateapiToken presents a Kubernetes projected ServiceAccount token as a gRPC
// bearer credential. Since Substrate added component authentication, the
// control plane rejects unauthenticated clients with
// `Unauthenticated: missing bearer token` — the server validates the JWT
// against the cluster issuer and the audience api.ate-system.svc.
//
// The file is re-read on every RPC: kubelet rotates a projected token well
// before its hour is up, and caching it would start failing after ~1h in a way
// that looks like an intermittent control-plane fault.
type ateapiToken struct{ path string }

func (t ateapiToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	b, err := os.ReadFile(t.path)
	if err != nil {
		return nil, fmt.Errorf("read ateapi token %s: %w", t.path, err)
	}
	return map[string]string{"authorization": "Bearer " + strings.TrimSpace(string(b))}, nil
}

// RequireTransportSecurity is true: the token must never cross a plaintext hop.
func (ateapiToken) RequireTransportSecurity() bool { return true }

// ateapiCreds returns the per-RPC credential when a projected token is mounted
// (AGENTPLANE_ATEAPI_TOKEN_FILE, default the standard mount path), or nil when
// it is absent — a CLI run outside the cluster has no ServiceAccount, and
// should fail on the server's terms rather than on a missing file here.
func ateapiCreds() []grpc.DialOption {
	path := env("AGENTPLANE_ATEAPI_TOKEN_FILE", "/run/ateapi-token/token")
	if _, err := os.Stat(path); err != nil {
		return nil
	}
	return []grpc.DialOption{grpc.WithPerRPCCredentials(ateapiToken{path: path})}
}

func (s sessionCtx) dial() (ateapipb.ControlClient, func(), error) {
	opts := []grpc.DialOption{
		grpc.WithTransportCredentials(credentials.NewTLS(ateapiTLS())),
		// Client spans for CreateActor/SuspendActor/…: no-op unless a tracer
		// provider is installed (i.e. under `serve` with OTLP configured).
		grpc.WithStatsHandler(otelgrpc.NewClientHandler()),
	}
	opts = append(opts, ateapiCreds()...)
	conn, err := grpc.NewClient(s.ateapi, opts...)
	if err != nil {
		return nil, nil, fmt.Errorf("dial ateapi: %w", err)
	}
	return ateapipb.NewControlClient(conn), func() { _ = conn.Close() }, nil
}

func runSession(args []string) {
	if len(args) < 1 {
		sessionUsage()
	}
	sc := newSessionCtx()
	switch args[0] {
	case "new":
		sessionNew(sc, args[1:])
	case "list":
		sessionList(sc)
	case "send":
		sessionSend(sc, args[1:])
	case "tail":
		sessionTail(sc, args[1:])
	case "events":
		sessionEvents(sc, args[1:])
	case "suspend":
		sessionSuspend(sc, args[1:], false)
	case "delete":
		sessionSuspend(sc, args[1:], true)
	default:
		sessionUsage()
	}
}

func sessionUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  agentplane session new [-agent <name>]
  agentplane session list
  agentplane session send -id sess-… -m "message"
  agentplane session tail -id sess-… [-since evt_…] [-for 30s]
  agentplane session events -id sess-… [-since evt_…]
  agentplane session suspend -id sess-…
  agentplane session delete  -id sess-…`)
	os.Exit(2)
}

func sessionID(fs *flag.FlagSet, args []string) string {
	id := fs.String("id", "", "session id (sess-…)")
	_ = fs.Parse(args)
	if !naming.IsSessionID(*id) {
		fmt.Fprintf(os.Stderr, "session: -id must be a valid session id (sess-…), got %q\n", *id)
		os.Exit(2)
	}
	return *id
}

func sessionNew(sc sessionCtx, args []string) {
	fs := flag.NewFlagSet("session new", flag.ExitOnError)
	agent := fs.String("agent", sc.template, "agent to mint from (ActorTemplate name)")
	_ = fs.Parse(args)
	sc.template = *agent
	sid, err := createSession(context.Background(), sc, *agent, "", nil)
	if err != nil {
		log.Fatal(err)
	}
	brain := naming.BrainActor(sid)
	dns := naming.ActorDNS(brain, sc.atespace)
	fmt.Printf("session: %s\nbrain:   %s (template %s/%s)\n\n", sid, brain, sc.templateNS, sc.template)
	fmt.Printf("talk to it:\n  agentplane session send -id %s -m \"hello\"\n  agentplane session tail -id %s\n", sid, sid)
	fmt.Printf("raw:\n  curl -sS -X POST http://%s/v1/sessions/%s/events -H \"Host: %s\" -H 'content-type: application/json' -d '{\"message\":\"hello\"}'\n", sc.atenet, brain, dns)
}

func sessionList(sc sessionCtx) {
	ctrl, closeFn, err := sc.dial()
	if err != nil {
		log.Fatal(err)
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	harnesses, _ := templateHarnesses(ctx, sc) // best-effort enrich
	fmt.Printf("%-20s %-16s %-12s %-14s %s\n", "SESSION", "AGENT", "HARNESS", "STATUS", "VERSION")
	token := ""
	for {
		resp, err := ctrl.ListActors(ctx, &ateapipb.ListActorsRequest{PageToken: token})
		if err != nil {
			log.Fatalf("list actors: %v", err)
		}
		for _, a := range resp.GetActors() {
			name := a.GetMetadata().GetName()
			sid, ok := naming.SessionFromActor(name)
			if !ok || !strings.HasPrefix(name, "b-") {
				continue
			}
			if a.GetMetadata().GetAtespace() != sc.atespace {
				continue
			}
			// Derived status: never probe SUSPENDED minds (probing wakes them).
			status := strings.ToLower(strings.TrimPrefix(a.GetStatus().String(), "STATUS_"))
			switch status {
			case "suspended":
				status = "sleeping"
			case "running":
				if h, ok := probeHealth(sc, name); !ok {
					status = "unreachable" // zombie candidate (case C/D)
				} else if h.Busy {
					status = "running"
				} else {
					status = "idle"
				}
			}
			agent := a.GetActorTemplateName()
			fmt.Printf("%-20s %-16s %-12s %-14s %d\n", sid, agent, harnesses[agent], status, a.GetMetadata().GetVersion())
		}
		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
	}
}

func sessionSend(sc sessionCtx, args []string) {
	fs := flag.NewFlagSet("session send", flag.ExitOnError)
	msg := fs.String("m", "", "message text")
	id := fs.String("id", "", "session id (sess-…)")
	_ = fs.Parse(args)
	if !naming.IsSessionID(*id) || *msg == "" {
		sessionUsage()
	}
	brain := naming.BrainActor(*id)
	req, _ := http.NewRequest(http.MethodPost,
		fmt.Sprintf("http://%s/v1/sessions/%s/events", sc.atenet, brain),
		strings.NewReader(fmt.Sprintf(`{"message":%q}`, *msg)))
	req.Host = naming.ActorDNS(brain, sc.atespace)
	req.Header.Set("Content-Type", "application/json")
	// Finding #3: a send during the mind's first resume can 5xx and the message
	// would be lost. A user.message append is safe to retry.
	hc := &http.Client{Timeout: 60 * time.Second} // first send may wake the mind
	var resp *http.Response
	var err error
	for attempt := 1; attempt <= 4; attempt++ {
		r2, _ := http.NewRequest(http.MethodPost, req.URL.String(),
			strings.NewReader(fmt.Sprintf(`{"message":%q}`, *msg)))
		r2.Host, r2.Header = req.Host, req.Header
		resp, err = hc.Do(r2)
		if err == nil && resp.StatusCode < 500 {
			break // relayable; keep this body open to read below
		}
		if resp != nil {
			resp.Body.Close()
			resp = nil
		}
		if attempt < 4 {
			fmt.Fprintf(os.Stderr, "send: transient failure (attempt %d), retrying…\n", attempt)
			time.Sleep(5 * time.Second)
		}
	}
	if err != nil {
		log.Fatalf("send: %v", err)
	}
	if resp == nil {
		log.Fatal("send: session did not accept the message after retries")
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	fmt.Printf("%s %s\n", resp.Status, strings.TrimSpace(string(rb)))
}

// healthInfo probes the brain's /healthz. ok=false when unreachable.
type healthInfo struct {
	Busy        bool            `json:"busy"`
	Queued      int             `json:"queued"`
	LastEventAt string          `json:"last_event_at"`
	Usage       json.RawMessage `json:"usage,omitempty"` // harness-reported session totals, relayed verbatim
}

func probeHealth(sc sessionCtx, brain string) (h healthInfo, ok bool) {
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/healthz", sc.atenet), nil)
	req.Host = naming.ActorDNS(brain, sc.atespace)
	hc := &http.Client{Timeout: 15 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return h, false
	}
	defer resp.Body.Close()
	if json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&h) != nil {
		return h, false
	}
	return h, true
}

func sessionTail(sc sessionCtx, args []string) {
	fs := flag.NewFlagSet("session tail", flag.ExitOnError)
	id := fs.String("id", "", "session id (sess-…)")
	since := fs.String("since", "", "cursor: only events after this id (evt_…)")
	dur := fs.Duration("for", 0, "stop after this duration (0 = forever)")
	_ = fs.Parse(args)
	if !naming.IsSessionID(*id) {
		sessionUsage()
	}
	brain := naming.BrainActor(*id)
	u := fmt.Sprintf("http://%s/v1/sessions/%s/events/stream", sc.atenet, brain)
	if *since != "" {
		u += "?since=" + url.QueryEscape(*since)
	}
	ctx := context.Background()
	if *dur > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *dur)
		defer cancel()
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Host = naming.ActorDNS(brain, sc.atespace)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Fatalf("tail: %v", err)
	}
	defer resp.Body.Close()
	sc2 := bufio.NewScanner(resp.Body)
	sc2.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc2.Scan() {
		line := sc2.Text()
		if strings.HasPrefix(line, "data: ") || strings.HasPrefix(line, ": hb") {
			fmt.Println(line)
		}
	}
}

func sessionSuspend(sc sessionCtx, args []string, alsoDelete bool) {
	fs := flag.NewFlagSet("session suspend/delete", flag.ExitOnError)
	force := fs.Bool("force", false, "suspend even if a turn is in flight (RISK: wedges the turn — see RELIABILITY finding #1)")
	sid := sessionID(fs, args)
	brain := naming.BrainActor(sid)

	// SAFE-SUSPEND GUARD: checkpointing mid-turn freezes an in-flight API
	// request; on restore the turn is wedged (busy forever, no error). Verified
	// live 2026-07-19 on sess-yt3i9ebp9f. Only suspend at turn boundaries.
	if !*force {
		if h, ok := probeHealth(sc, brain); ok && h.Busy {
			fmt.Fprintf(os.Stderr, "session %s has a turn IN FLIGHT (busy=true) — refusing to checkpoint mid-turn.\nWait for session.status_idle, or use -force to accept a wedged turn.\n", sid)
			os.Exit(1)
		}
	}
	ctrl, closeFn, err := sc.dial()
	if err != nil {
		log.Fatal(err)
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	// Read usage while the mind is still awake: once suspended it can only be
	// read by resuming it, which costs a restore and defeats auto-sleep (#42).
	recordUsageIfAwake(ctx, ctrl, sc, sid, brain)
	// Deletion-lifecycle rule: escrow before destroy, always. Best-effort —
	// never blocks the checkpoint.
	escrowTranscript(sc, sid, brain)

	ref := &ateapipb.ObjectRef{Atespace: sc.atespace, Name: brain}
	if _, err := ctrl.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: ref}); err != nil {
		// Suspending an already-suspended actor is fine for our purposes.
		if !alsoDelete {
			log.Printf("suspend: %v (may already be suspended)", err)
		}
	} else if !alsoDelete {
		fmt.Printf("session %s suspended (mind checkpointed)\n", sid)
	}
	if alsoDelete {
		if _, err := ctrl.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref}); err != nil {
			log.Fatalf("delete: %v", err)
		}
		// Cascade: nothing runtime survives the session. The escrowed
		// transcript is the ONE deliberate survivor (audit record).
		cleanupSnapshots(sc, brain)
	}

	// Paired hand (h-<id>): keep it in lockstep with the brain so the pair
	// checkpoints / rolls back at the same logical point (state consistency).
	// Best-effort — absent for non-hand agents. Suspend at the brain's turn
	// boundary; the hand is idle between turns, so this is a consistent pair.
	handName := naming.HandActor(sid)
	handRef := &ateapipb.ObjectRef{Atespace: sc.atespace, Name: handName}
	_, _ = ctrl.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: handRef})
	if alsoDelete {
		_, _ = ctrl.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: handRef})
		cleanupSnapshots(sc, handName)
		fmt.Printf("session %s deleted (brain + hand + snapshots removed; transcript escrowed)\n", sid)
	} else {
		fmt.Printf("session %s suspended (mind + hand checkpointed)\n", sid)
	}
}

// fetchEvents pulls the session event log (wakes the mind if suspended).
func fetchEvents(sc sessionCtx, brain string) ([]byte, error) {
	req, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/v1/sessions/%s/events", sc.atenet, brain), nil)
	req.Host = naming.ActorDNS(brain, sc.atespace)
	hc := &http.Client{Timeout: 45 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("events: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// escrowTranscript writes the event log to GCS as a READABLE object — the
// audit record that outlives the actor (unlike snapshots, which are opaque
// memory images). Skips with a warning when AGENTPLANE_BUCKET is unset.
func escrowTranscript(sc sessionCtx, sid, brain string) {
	bucket := os.Getenv("AGENTPLANE_BUCKET")
	if bucket == "" {
		fmt.Fprintln(os.Stderr, "escrow: AGENTPLANE_BUCKET unset — transcript NOT escrowed")
		return
	}
	data, err := fetchEvents(sc, brain)
	if err != nil {
		fmt.Fprintf(os.Stderr, "escrow: fetch failed (%v) — continuing\n", err)
		return
	}
	obj := fmt.Sprintf("gs://%s/transcripts/%s/events-%d.json", bucket, sid, time.Now().Unix())
	// Bounded: escrow runs on the delete path and inside the dispatcher's sweep,
	// so an unbounded `gcloud` (auth prompt, network stall) would wedge the
	// caller indefinitely — in the dispatcher that means auto-sleep stops for
	// every session, silently. Better to lose one transcript than the loop.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gcloud", "storage", "cp", "-", obj)
	cmd.Stdin = strings.NewReader(string(data))
	if out, err := cmd.CombinedOutput(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			fmt.Fprintf(os.Stderr, "escrow: upload timed out after 60s — transcript NOT escrowed: %s\n", obj)
			return
		}
		fmt.Fprintf(os.Stderr, "escrow: upload failed: %v %s\n", err, out)
		return
	}
	fmt.Printf("escrowed: %s (%d bytes)\n", obj, len(data))
}

// cleanupSnapshots removes the mind's snapshot prefix on delete (cascade).
func cleanupSnapshots(sc sessionCtx, brain string) {
	bucket := os.Getenv("AGENTPLANE_BUCKET")
	if bucket == "" {
		fmt.Fprintln(os.Stderr, "cleanup: AGENTPLANE_BUCKET unset — snapshots NOT removed")
		return
	}
	prefix := fmt.Sprintf("gs://%s/agentplane/%s", bucket, brain)
	if out, err := exec.Command("gcloud", "storage", "rm", "-r", prefix).CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "cleanup: %v %s\n", err, strings.TrimSpace(string(out)))
	}
}

// sessionEvents prints the persisted turn log, compactly.
func sessionEvents(sc sessionCtx, args []string) {
	fs := flag.NewFlagSet("session events", flag.ExitOnError)
	id := fs.String("id", "", "session id (sess-…)")
	since := fs.String("since", "", "cursor: only events after this id")
	_ = fs.Parse(args)
	if !naming.IsSessionID(*id) {
		sessionUsage()
	}
	brain := naming.BrainActor(*id)
	u := fmt.Sprintf("http://%s/v1/sessions/%s/events", sc.atenet, brain)
	if *since != "" {
		u += "?since=" + url.QueryEscape(*since)
	}
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Host = naming.ActorDNS(brain, sc.atespace)
	hc := &http.Client{Timeout: 45 * time.Second}
	resp, err := hc.Do(req)
	if err != nil {
		log.Fatalf("events: %v", err)
	}
	defer resp.Body.Close()
	var payload struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(&payload); err != nil {
		log.Fatalf("events: decode: %v", err)
	}
	for _, e := range payload.Events {
		eid, _ := e["id"].(string)
		typ, _ := e["type"].(string)
		var text string
		if cs, ok := e["content"].([]any); ok && len(cs) > 0 {
			if c0, ok := cs[0].(map[string]any); ok {
				text, _ = c0["text"].(string)
			}
		}
		if len(text) > 64 {
			text = text[:64] + "…"
		}
		fmt.Printf("%-12s %-24s %s\n", eid, typ, strings.ReplaceAll(text, "\n", " "))
	}
}
