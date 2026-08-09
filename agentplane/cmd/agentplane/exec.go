package main

// Internal exec endpoint: runs a command in a RUNNING actor via the control
// plane's ExecActor (`runsc exec`), on behalf of the broker. This is how the
// broker runs commands with the executor OUTSIDE the sandbox — the actor holds
// no command-runner of ours.
//
// Internal-only. Actors can reach serve:7433, so this MUST NOT be an open
// exec-into-any-actor door: the caller is authenticated by its ServiceAccount —
// the broker presents a projected SA token, verified here with a k8s TokenReview
// (F15). No shared secret.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// brokerTokenCache memoizes successful TokenReviews so the hot path does not
// fork kubectl on every tool call. Keyed by a hash of the token; the broker's
// projected SA token rotates ~hourly, so a short TTL is safe.
var brokerTokenCache sync.Map // string(sha256) -> time.Time (expiry)

// verifyBrokerToken authenticates the caller of /v1/internal/exec by its
// ServiceAccount, not a shared secret (F15): the broker presents its projected
// SA token; serve validates it with a k8s TokenReview (via kubectl — serve does
// not vendor client-go) and checks the SA identity and audience. Result cached.
func (s *server) verifyBrokerToken(ctx context.Context, token string) bool {
	if token == "" {
		return false
	}
	sum := sha256.Sum256([]byte(token))
	key := string(sum[:])
	if v, ok := brokerTokenCache.Load(key); ok {
		if exp, ok := v.(time.Time); ok && time.Now().Before(exp) {
			return true
		}
		brokerTokenCache.Delete(key)
	}
	aud := env("AGENTPLANE_BROKER_AUDIENCE", "agentplane-serve")
	wantSA := env("AGENTPLANE_BROKER_SA", "system:serviceaccount:agentplane:agentplane-broker")
	tr := fmt.Sprintf(`{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","spec":{"token":%q,"audiences":[%q]}}`, token, aud)
	// --validate=false: client-side validation lists CRDs to classify the kind,
	// which serve's SA cannot do (and TokenReview is built-in, so it is moot).
	out, err := runKubectlStdin(ctx, tr, "create", "-f", "-", "-o", "json", "--validate=false")
	if err != nil {
		s.log.Warn("broker tokenreview failed", "err", err, "out", string(out))
		return false
	}
	// runKubectlStdin merges stderr; a leading warning line would break JSON
	// parsing, so start at the first object brace.
	if i := bytes.IndexByte(out, '{'); i > 0 {
		out = out[i:]
	}
	var rev struct {
		Status struct {
			Authenticated bool     `json:"authenticated"`
			Audiences     []string `json:"audiences"`
			User          struct {
				Username string `json:"username"`
			} `json:"user"`
		} `json:"status"`
	}
	if err := json.Unmarshal(out, &rev); err != nil || !rev.Status.Authenticated {
		return false
	}
	if rev.Status.User.Username != wantSA {
		s.log.Warn("internal exec: unexpected caller identity", "user", rev.Status.User.Username)
		return false
	}
	audOK := false
	for _, a := range rev.Status.Audiences {
		if a == aud {
			audOK = true
		}
	}
	if !audOK {
		return false
	}
	brokerTokenCache.Store(key, time.Now().Add(5*time.Minute))
	return true
}

// execInHand runs argv in a hand actor via Control.ExecActor, with resume-on-exec:
// the hand auto-suspends when idle and ExecActor does not resume, so on
// FailedPrecondition ("not running") we wake it once and retry. This is the one
// path everything runs commands through — the internal endpoint (broker tools)
// and git setup — so the executor stays outside the sandbox.
func execInHand(ctx context.Context, ctrl ateapipb.ControlClient, atespace, actor, container string, argv []string, cwd string, env []string, timeoutMs int64) (*ateapipb.ExecActorResponse, error) {
	ref := &ateapipb.ObjectRef{Atespace: atespace, Name: actor}
	req := &ateapipb.ExecActorRequest{
		Actor: ref, ContainerName: container, Argv: argv, Cwd: cwd, Env: env, TimeoutMs: timeoutMs,
	}
	resp, err := ctrl.ExecActor(ctx, req)
	if err != nil && status.Code(err) == codes.FailedPrecondition {
		if _, rerr := ctrl.ResumeActor(ctx, &ateapipb.ResumeActorRequest{Actor: ref}); rerr != nil {
			return nil, fmt.Errorf("resume hand: %w", rerr)
		}
		resp, err = ctrl.ExecActor(ctx, req)
	}
	return resp, err
}

func (s *server) handleInternalExec(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !s.verifyBrokerToken(r.Context(), token) {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"error": "internal auth required (broker SA token)"})
		return
	}

	var in struct {
		Actor     string            `json:"actor"`     // e.g. h-sess-xxx
		Container string            `json:"container"` // e.g. exec
		Argv      []string          `json:"argv"`
		Cwd       string            `json:"cwd"`
		Env       map[string]string `json:"env"`
		TimeoutMs int64             `json:"timeoutMs"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "invalid json"})
		return
	}
	if in.Actor == "" || len(in.Argv) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "actor and argv required"})
		return
	}

	ctrl, closeFn, err := s.sc.dial()
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "dial control plane: " + err.Error()})
		return
	}
	defer closeFn()

	var envv []string
	for k, v := range in.Env {
		envv = append(envv, k+"="+v)
	}
	timeout := time.Duration(in.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout+15*time.Second)
	defer cancel()

	resp, err := execInHand(ctx, ctrl, s.sc.atespace, in.Actor, in.Container, in.Argv, in.Cwd, envv, in.TimeoutMs)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"stdout":   string(resp.GetStdout()),
		"stderr":   string(resp.GetStderr()),
		"exitCode": resp.GetExitCode(),
	})
}
