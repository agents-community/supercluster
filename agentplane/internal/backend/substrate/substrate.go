// Package substrate is agentplane's PRIMARY sandbox backend: it implements the
// pkg/sandbox interfaces against Agent Substrate.
//
// Each logical sandbox maps to a Substrate *actor* (derived from an
// ActorTemplate). The actor is resumed (runsc restore) on first use and
// suspended (runsc checkpoint → object storage) on Suspend, so Substrate
// multiplexes many sandboxes onto a small WorkerPool. Commands execute by
// POSTing to the actor's /process endpoint through atenet, routed by Host.
//
// Hardening carried over from the substrate-agents-api POCs:
//   - Exec runs commands as background jobs + polls a sentinel file, so long
//     commands survive atenet's ~10s route timeout AND suspend/resume.
//   - HTTP responses are status-checked; transient 5xx/transport errors (e.g. a
//     briefly-unprogrammed route after resume) retry with backoff instead of
//     surfacing as JSON-decode noise.
package substrate

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	"github.com/quantumnode/agentplane/pkg/sandbox"
)

// Mirrors internal/resources.ActorDNSSuffix (that package is internal to the
// substrate module and cannot be imported here).
const actorDNSSuffix = "actors.resources.substrate.ate.dev"

// Config points the provider at a Substrate cluster and the ActorTemplate new
// actors are derived from.
type Config struct {
	AteapiAddr        string // gRPC control API, e.g. "localhost:8080"
	AtenetAddr        string // HTTP router, e.g. "localhost:8000"
	Atespace          string // actors live here, e.g. "agents"
	TemplateNamespace string // e.g. "ate-demo-sandbox"
	TemplateName      string // e.g. "sandbox-template"
}

type provider struct {
	cfg  Config
	conn *grpc.ClientConn
	ctrl ateapipb.ControlClient
	hc   *http.Client
}

// NewProvider dials ateapi (gRPC over TLS; the dev api uses a self-signed cert).
func NewProvider(cfg Config) (sandbox.Provider, error) {
	conn, err := grpc.NewClient(cfg.AteapiAddr,
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{InsecureSkipVerify: true})))
	if err != nil {
		return nil, fmt.Errorf("dial ateapi: %w", err)
	}
	return &provider{cfg: cfg, conn: conn, ctrl: ateapipb.NewControlClient(conn), hc: &http.Client{Timeout: 120 * time.Second}}, nil
}

func (p *provider) Close() error { return p.conn.Close() }

func (p *provider) Sandbox(name string) sandbox.Sandbox {
	return &actorSandbox{p: p, name: name, dns: name + "." + p.cfg.Atespace + "." + actorDNSSuffix}
}

type actorSandbox struct {
	p    *provider
	name string
	dns  string

	mu      sync.Mutex
	resumed bool
}

// ensure creates (idempotently) and resumes the actor. It latches only on
// SUCCESS: a failed attempt (e.g. a cancelled/expired ctx, a transient resume
// error) is NOT cached, so a later call with a healthy context retries rather
// than returning a stale error forever.
func (s *actorSandbox) ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.resumed {
		return nil
	}
	_, err := s.p.ctrl.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata:               &ateapipb.ResourceMetadata{Atespace: s.p.cfg.Atespace, Name: s.name},
		ActorTemplateNamespace: s.p.cfg.TemplateNamespace,
		ActorTemplateName:      s.p.cfg.TemplateName,
	}})
	if err != nil && status.Code(err) != codes.AlreadyExists {
		return fmt.Errorf("create actor %s: %w", s.name, err)
	}
	if _, err := s.p.ctrl.ResumeActor(ctx, &ateapipb.ResumeActorRequest{
		Actor: &ateapipb.ObjectRef{Atespace: s.p.cfg.Atespace, Name: s.name},
	}); err != nil {
		return fmt.Errorf("resume actor %s: %w", s.name, err)
	}
	s.resumed = true
	return nil
}

type processRequest struct {
	Command []string `json:"command"`
}
type processResponse struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
	Error    string `json:"error,omitempty"`
}

// processRouteRetry bounds how long run() retries atenet's transient upstream
// failures. A freshly-resumed actor's route can be briefly unprogrammed (and a
// stale worker IP surfaces here as a 5xx/connection error too), so we ride that
// out rather than failing the tool call on the first blip.
const processRouteRetry = 30 * time.Second

func (s *actorSandbox) run(ctx context.Context, argv []string) (sandbox.ExecResult, error) {
	if err := s.ensure(ctx); err != nil {
		return sandbox.ExecResult{}, err
	}
	body, _ := json.Marshal(processRequest{Command: argv})

	deadline := time.Now().Add(processRouteRetry)
	for attempt := 0; ; attempt++ {
		res, retryable, err := s.doProcess(ctx, body)
		if err == nil || !retryable || time.Now().After(deadline) {
			return res, err
		}
		wait := time.Duration(200*(attempt+1)) * time.Millisecond
		if wait > 2*time.Second {
			wait = 2 * time.Second
		}
		select {
		case <-ctx.Done():
			return sandbox.ExecResult{}, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// doProcess makes one POST /process call. retryable is true when atenet answered
// with a transient upstream failure — a 5xx (Envoy returns a plaintext body like
// "upstream connect error ... connection timeout", NOT the actor's JSON) or a
// transport error — i.e. the actor isn't routable yet.
func (s *actorSandbox) doProcess(ctx context.Context, body []byte) (sandbox.ExecResult, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+s.p.cfg.AtenetAddr+"/process", bytes.NewReader(body))
	if err != nil {
		return sandbox.ExecResult{}, false, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Host = s.dns // atenet routes by Host to this actor

	resp, err := s.p.hc.Do(req)
	if err != nil {
		return sandbox.ExecResult{}, true, fmt.Errorf("post /process: %w", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode != http.StatusOK {
		return sandbox.ExecResult{}, resp.StatusCode >= 500, fmt.Errorf("/process HTTP %d: %s", resp.StatusCode, snippet(raw))
	}
	var pr processResponse
	if err := json.Unmarshal(raw, &pr); err != nil {
		return sandbox.ExecResult{}, false, fmt.Errorf("decode /process (body %q): %w", snippet(raw), err)
	}
	res := sandbox.ExecResult{Stdout: pr.Stdout, Stderr: pr.Stderr, ExitCode: pr.ExitCode}
	if pr.Error != "" {
		return res, false, fmt.Errorf("sandbox /process error: %s", pr.Error)
	}
	return res, false, nil
}

// snippet trims a response body for inclusion in an error message.
func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		return s[:300] + "…"
	}
	return s
}

var (
	jobCounter int64
	// procNonce makes job dirs unique per process. The actor fs persists across
	// suspend/resume and across separate agentplane invocations, so a bare
	// per-process counter (j1, j2, …) would collide with a previous run's job
	// dir and read its STALE exit code/output. The nonce + a clean of the dir
	// (see launch) prevent that.
	procNonce = strconv.FormatInt(time.Now().UnixNano(), 36)
)

// Exec runs a command asynchronously inside the actor: it launches the command
// as a background job (one fast /process call) and then polls for completion
// (more fast calls), so a long-running command never blocks a single /process
// request past atenet's ~10s Envoy route timeout. Because the job is a real
// background process in the actor, it also survives Substrate suspend/resume.
func (s *actorSandbox) Exec(ctx context.Context, command string) (sandbox.ExecResult, error) {
	if err := s.ensure(ctx); err != nil {
		return sandbox.ExecResult{}, err
	}
	id := fmt.Sprintf("j%s-%d", procNonce, atomic.AddInt64(&jobCounter, 1))
	dir := "/tmp/jobs/" + id
	b64 := base64.StdEncoding.EncodeToString([]byte(command))

	// "send": launch the job in the background; returns immediately.
	// The dir is cleaned first (rm -rf) so a leftover $d/code from any prior job
	// of the same id can never be mistaken for this job's result.
	// The background job MUST redirect its own stdio to /dev/null: otherwise it
	// inherits the /process handler's stdout fd, and cmd.Run() blocks until the
	// job exits (defeating the async launch and hitting atenet's 10s timeout).
	launch := fmt.Sprintf(`d=%s; rm -rf "$d"; mkdir -p "$d"; printf '%%s' %q | base64 -d > "$d/cmd";`+
		` ( sh "$d/cmd" > "$d/out" 2> "$d/err"; echo $? > "$d/code" ) </dev/null >/dev/null 2>&1 & echo launched`, dir, b64)
	if _, err := s.run(ctx, []string{"/bin/sh", "-c", launch}); err != nil {
		return sandbox.ExecResult{}, err
	}

	// "stream": poll for the exit-code sentinel; each poll is a fast /process call.
	poll := fmt.Sprintf(`d=%s; if [ -f "$d/code" ]; then cat "$d/code"; else echo RUNNING; fi`, dir)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		r, err := s.run(ctx, []string{"/bin/sh", "-c", poll})
		if err != nil {
			return sandbox.ExecResult{}, err
		}
		if out := strings.TrimSpace(r.Stdout); out != "RUNNING" && out != "" {
			code, _ := strconv.Atoi(out)
			stdout, _ := s.catFile(ctx, dir+"/out")
			stderr, _ := s.catFile(ctx, dir+"/err")
			return sandbox.ExecResult{Stdout: stdout, Stderr: stderr, ExitCode: code}, nil
		}
		select {
		case <-ctx.Done():
			return sandbox.ExecResult{}, ctx.Err()
		case <-ticker.C:
		}
	}
}

// catFile reads a file's contents via /process (best-effort, no exit-code check).
func (s *actorSandbox) catFile(ctx context.Context, path string) (string, error) {
	r, err := s.run(ctx, []string{"/bin/cat", path})
	if err != nil {
		return "", err
	}
	return r.Stdout, nil
}

func (s *actorSandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	r, err := s.run(ctx, []string{"/bin/cat", path})
	if err != nil {
		return nil, err
	}
	if r.ExitCode != 0 {
		return nil, fmt.Errorf("read %s: %s", path, r.Stderr)
	}
	return []byte(r.Stdout), nil
}

func (s *actorSandbox) WriteFile(ctx context.Context, path string, data []byte) error {
	b64 := base64.StdEncoding.EncodeToString(data)
	script := fmt.Sprintf(`mkdir -p "$(dirname %q)" && printf '%%s' %q | base64 -d > %q`, path, b64, path)
	r, err := s.run(ctx, []string{"/bin/sh", "-c", script})
	if err != nil {
		return err
	}
	if r.ExitCode != 0 {
		return fmt.Errorf("write %s: %s", path, r.Stderr)
	}
	return nil
}

func (s *actorSandbox) Suspend(ctx context.Context) error {
	if !s.resumed {
		return nil
	}
	_, err := s.p.ctrl.SuspendActor(ctx, &ateapipb.SuspendActorRequest{
		Actor: &ateapipb.ObjectRef{Atespace: s.p.cfg.Atespace, Name: s.name},
	})
	return err
}
