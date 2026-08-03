package main

// The `agentplane agent` command group — the Agent surface (D3: Agent =
// ActorTemplate). An AgentSpec file (internal/agentspec) compiles to the
// ActorTemplate realizing it; sessions are minted from an agent by name.
//
//	agentplane agent create -f spec.yaml [-print]   compile + apply + wait Ready (golden bake)
//	agentplane agent list                           managed agents: harness, phase, live sessions
//	agentplane agent delete -name X [-cascade]      delete an agent (+ its sessions, escrow-first)
//
// Cascade rules (finding #4, verified live): deleting a template while its
// sessions live makes them UNRESUMABLE — so sessions are always destroyed
// (escrow-first) BEFORE the template, and delete refuses without -cascade when
// sessions exist. Template recreation is safe: old snapshots restore fine.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"

	"github.com/quantumnode/agentplane/internal/agentspec"
	"github.com/quantumnode/agentplane/internal/naming"
)

func runAgent(args []string) {
	if len(args) < 1 {
		agentUsage()
	}
	sc := newSessionCtx()
	switch args[0] {
	case "create":
		agentCreate(sc, args[1:])
	case "list":
		agentList(sc)
	case "delete":
		agentDelete(sc, args[1:])
	default:
		agentUsage()
	}
}

func agentUsage() {
	fmt.Fprintln(os.Stderr, `usage:
  agentplane agent create -f spec.yaml [-print]
  agentplane agent list
  agentplane agent delete -name <agent> [-cascade]`)
	os.Exit(2)
}

func agentCreate(sc sessionCtx, args []string) {
	fs := flag.NewFlagSet("agent create", flag.ExitOnError)
	file := fs.String("f", "", "AgentSpec file (yaml)")
	printOnly := fs.Bool("print", false, "print the compiled ActorTemplate instead of applying")
	_ = fs.Parse(args)
	if *file == "" {
		agentUsage()
	}
	spec, err := agentspec.Load(*file)
	if err != nil {
		log.Fatalf("agent create: %v", err)
	}
	tmpl, err := spec.CompileTemplate(sc.templateNS, os.Getenv("AGENTPLANE_BUCKET"))
	if err != nil {
		log.Fatalf("agent create: %v", err)
	}
	if *printOnly {
		fmt.Println(string(tmpl))
		return
	}
	if err := applyAgent(context.Background(), sc, spec.Name, tmpl); err != nil {
		log.Fatalf("agent create: %v", err)
	}
	// Golden bake: the controller boots one actor from the image and checkpoints
	// it; every session then starts from that snapshot (~1s wakes).
	fmt.Printf("agent %s: baking golden snapshot", spec.Name)
	for i := 0; i < 60; i++ {
		time.Sleep(5 * time.Second)
		if templatePhase(context.Background(), sc, spec.Name) == "Ready" {
			fmt.Printf("\nagent %s ready (harness %s)\n\nmint a session:\n  agentplane session new -agent %s\n", spec.Name, spec.Harness, spec.Name)
			return
		}
		fmt.Print(".")
	}
	fmt.Println()
	log.Fatalf("agent %s not Ready after 5m — inspect: kubectl describe actortemplate %s -n %s", spec.Name, spec.Name, sc.templateNS)
}

// kubectlTimeout bounds every kubectl call so a hung API server can't wedge a
// CLI command or an HTTP handler goroutine forever.
const kubectlTimeout = 30 * time.Second

// runKubectl runs a context-bounded kubectl command. SECURITY: any
// user-derived value (an agent name) MUST be passed after a "--" separator by
// the caller so kubectl can never interpret it as a flag (e.g. "--all").
func runKubectl(ctx context.Context, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, kubectlTimeout)
	defer cancel()
	return exec.CommandContext(cctx, "kubectl", args...).CombinedOutput()
}

// runKubectlStdin is runKubectl with a body piped to stdin (`apply -f -`).
func runKubectlStdin(ctx context.Context, stdin string, args ...string) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, kubectlTimeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, "kubectl", args...)
	cmd.Stdin = strings.NewReader(stdin)
	return cmd.CombinedOutput()
}

// applyAgent applies a compiled template, enforcing create-not-replace:
// ActorTemplate specs are IMMUTABLE — creating over an existing agent would
// fail confusingly on apply, and replace-in-place is only safe with zero
// sessions (finding #4). Shared by `agent create` and POST /v1/agents.
func applyAgent(ctx context.Context, sc sessionCtx, name string, tmpl []byte) error {
	if !naming.IsAgentName(name) {
		return fmt.Errorf("invalid agent name %q", name)
	}
	if templateExists(ctx, sc, name) {
		return fmt.Errorf("agent %q already exists (templates are immutable) — delete it first", name)
	}
	cctx, cancel := context.WithTimeout(ctx, kubectlTimeout)
	defer cancel()
	apply := exec.CommandContext(cctx, "kubectl", "apply", "-f", "-")
	apply.Stdin = strings.NewReader(string(tmpl))
	if out, err := apply.CombinedOutput(); err != nil {
		return fmt.Errorf("apply failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// errHasSessions signals a refused non-cascade delete; Sessions lists the strays.
type errHasSessions struct{ Sessions []string }

func (e errHasSessions) Error() string {
	return fmt.Sprintf("agent has %d live session(s): %s — cascade required", len(e.Sessions), strings.Join(e.Sessions, ", "))
}

// deleteAgent removes an agent, cascading its sessions escrow-first when asked
// (ORDER MATTERS: sessions before template — finding #4). Returns the deleted
// session ids. Shared by `agent delete` and DELETE /v1/agents/{name}.
func deleteAgent(ctx context.Context, sc sessionCtx, name string, cascade bool) ([]string, error) {
	if !naming.IsAgentName(name) {
		return nil, fmt.Errorf("invalid agent name %q", name)
	}
	if !templateExists(ctx, sc, name) {
		return nil, fmt.Errorf("no agent %q in %s", name, sc.templateNS)
	}
	actors, err := listSessionActors(ctx, sc, name)
	if err != nil {
		return nil, err
	}
	var sids []string
	for _, a := range actors {
		sid, _ := naming.SessionFromActor(a.GetMetadata().GetName())
		sids = append(sids, sid)
	}
	if len(sids) > 0 && !cascade {
		return nil, errHasSessions{Sessions: sids}
	}
	for _, a := range actors {
		brain := a.GetMetadata().GetName()
		sid, _ := naming.SessionFromActor(brain)
		if err := deleteSession(ctx, sc, sid, brain); err != nil {
			return nil, err
		}
	}
	// The controller GCs the golden actor with its template. "--" guards the
	// name (belt-and-suspenders with IsAgentName above).
	if out, err := runKubectl(ctx, "delete", "actortemplate", "-n", sc.templateNS, "--", name); err != nil {
		return sids, fmt.Errorf("template delete: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return sids, nil
}

func templateExists(ctx context.Context, sc sessionCtx, name string) bool {
	_, err := runKubectl(ctx, "get", "actortemplate", "-n", sc.templateNS, "--", name)
	return err == nil
}

// templateHarnesses maps agent name → harness label in one kubectl call.
// Used to enrich session objects (a session's harness = its agent's harness;
// derived, per the stateless-control-plane rule — never stored).
// templateAgents maps each ActorTemplate to the LOGICAL agent it is a version
// of. Sessions run on a version template (`starter-v2`), but every client —
// and every user — thinks in agent names, so the API must not leak the
// template. Templates predating versioning carry no label and map to
// themselves.
func templateAgents(ctx context.Context, sc sessionCtx) (map[string]string, error) {
	out, err := runKubectl(ctx, "get", "actortemplates", "-n", sc.templateNS,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{.metadata.labels.agentplane\.io/agent}{"\n"}{end}`)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w: %s", err, strings.TrimSpace(string(out)))
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, agent, _ := strings.Cut(line, " ")
		if name == "" {
			continue
		}
		if agent == "" {
			agent = name
		}
		m[name] = agent
	}
	return m, nil
}

func templateHarnesses(ctx context.Context, sc sessionCtx) (map[string]string, error) {
	out, err := runKubectl(ctx, "get", "actortemplates", "-n", sc.templateNS,
		"-o", `jsonpath={range .items[*]}{.metadata.name}{" "}{.metadata.labels.agentplane\.io/harness}{"\n"}{end}`)
	if err != nil {
		return nil, fmt.Errorf("list templates: %w: %s", err, strings.TrimSpace(string(out)))
	}
	m := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		name, harness, _ := strings.Cut(line, " ")
		if name != "" {
			if harness == "" {
				harness = "-"
			}
			m[name] = harness
		}
	}
	return m, nil
}

func templatePhase(ctx context.Context, sc sessionCtx, name string) string {
	out, err := runKubectl(ctx, "get", "actortemplate", "-n", sc.templateNS,
		"-o", "jsonpath={.status.phase}", "--", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// agentInfo is the Agent object shape shared by `agent list` and `serve`.
type agentInfo struct {
	Name     string `json:"name"`
	Harness  string `json:"harness"`
	Phase    string `json:"phase"`
	Sessions int    `json:"sessions"`
	// Version is the newest version of this agent; Sessions counts minds across
	// ALL of its versions, since an older version still serving sessions is
	// exactly why its template must not be deleted (#32).
	Version int `json:"version,omitempty"`
}

func listAgents(ctx context.Context, sc sessionCtx) ([]agentInfo, error) {
	out, err := runKubectl(ctx, "get", "actortemplates", "-n", sc.templateNS, "-o", "json")
	if err != nil {
		return nil, fmt.Errorf("agent list: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var payload struct {
		Items []struct {
			Metadata struct {
				Name   string            `json:"name"`
				Labels map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("agent list: parse: %w", err)
	}
	counts := map[string]int{}
	actors, err := listSessionActors(ctx, sc, "")
	if err != nil {
		return nil, err
	}
	for _, a := range actors {
		counts[a.GetActorTemplateName()]++
	}
	// Versions of one agent are separate templates (starter, starter-v2, …).
	// Collapse them back into a single entry keyed by the logical agent, or a
	// listing would show every version as if it were a different agent.
	byAgent := map[string]*agentInfo{}
	order := []string{}
	for _, it := range payload.Items {
		// The hand is infrastructure (execution sandbox), not a user-selectable
		// agent — hide it so callers only see minds they can talk to.
		if it.Metadata.Labels["agentplane.io/role"] == "hand" {
			continue
		}
		harness := it.Metadata.Labels["agentplane.io/harness"]
		if harness == "" {
			harness = "-" // pre-CLI template (e.g. the hand-deployed brain)
		}
		// Templates predating versioning carry no agent label; their own name
		// is the agent name.
		name := it.Metadata.Labels["agentplane.io/agent"]
		if name == "" {
			name = it.Metadata.Name
		}
		version := 0
		if v := it.Metadata.Labels["agentplane.io/version"]; v != "" {
			version, _ = strconv.Atoi(v)
		}
		cur, seen := byAgent[name]
		if !seen {
			cur = &agentInfo{Name: name, Harness: harness}
			byAgent[name] = cur
			order = append(order, name)
		}
		// Sessions accumulate across versions; phase and harness track the
		// NEWEST version, which is what a new session would be minted from.
		cur.Sessions += counts[it.Metadata.Name]
		if version >= cur.Version {
			cur.Version, cur.Phase, cur.Harness = version, it.Status.Phase, harness
		}
	}
	agents := []agentInfo{}
	for _, name := range order {
		agents = append(agents, *byAgent[name])
	}
	return agents, nil
}

func agentList(sc sessionCtx) {
	agents, err := listAgents(context.Background(), sc)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("%-16s %-14s %-10s %s\n", "AGENT", "HARNESS", "PHASE", "SESSIONS")
	for _, a := range agents {
		fmt.Printf("%-16s %-14s %-10s %d\n", a.Name, a.Harness, a.Phase, a.Sessions)
	}
}

// listSessionActors returns live session actors (b-sess-…) in our atespace,
// optionally filtered to one template.
func listSessionActors(ctx context.Context, sc sessionCtx, template string) ([]*ateapipb.Actor, error) {
	ctrl, closeFn, err := sc.dial()
	if err != nil {
		return nil, err
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var actors []*ateapipb.Actor
	token := ""
	for {
		resp, err := ctrl.ListActors(ctx, &ateapipb.ListActorsRequest{PageToken: token})
		if err != nil {
			return nil, fmt.Errorf("list actors: %w", err)
		}
		for _, a := range resp.GetActors() {
			name := a.GetMetadata().GetName()
			if _, ok := naming.SessionFromActor(name); !ok || !strings.HasPrefix(name, "b-") {
				continue
			}
			if a.GetMetadata().GetAtespace() != sc.atespace {
				continue
			}
			if template != "" &&
				(a.GetActorTemplateName() != template || a.GetActorTemplateNamespace() != sc.templateNS) {
				continue
			}
			actors = append(actors, a)
		}
		token = resp.GetNextPageToken()
		if token == "" {
			break
		}
	}
	return actors, nil
}

func agentDelete(sc sessionCtx, args []string) {
	fs := flag.NewFlagSet("agent delete", flag.ExitOnError)
	name := fs.String("name", "", "agent (ActorTemplate) name")
	cascade := fs.Bool("cascade", false, "also delete the agent's live sessions (escrow-first)")
	_ = fs.Parse(args)
	if *name == "" {
		agentUsage()
	}
	sids, err := deleteAgent(context.Background(), sc, *name, *cascade)
	var hasSess errHasSessions
	if errors.As(err, &hasSess) {
		fmt.Fprintf(os.Stderr, "agent %q has %d live session(s) — deleting the template would strand them (finding #4: unresumable).\n", *name, len(hasSess.Sessions))
		for _, sid := range hasSess.Sessions {
			fmt.Fprintf(os.Stderr, "  %s\n", sid)
		}
		fmt.Fprintln(os.Stderr, "re-run with -cascade to escrow + delete them, or delete them individually first.")
		os.Exit(1)
	}
	if err != nil {
		log.Fatalf("agent delete: %v", err)
	}
	fmt.Printf("agent %s deleted (%d session(s) escrowed + removed)\n", *name, len(sids))
}

// deleteSession runs the standard session cascade: escrow → suspend → delete
// actor → remove snapshots. Same path as `session delete`.
func deleteSession(ctx context.Context, sc sessionCtx, sid, brain string) error {
	ctrl, closeFn, err := sc.dial()
	if err != nil {
		return err
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	// Capture cost before the mind goes away — after this the actor is gone and
	// its usage is unrecoverable (#42). Same pass as escrow: the actor is live
	// and we are already talking to it.
	recordUsageIfAwake(ctx, ctrl, sc, sid, brain)
	escrowTranscript(sc, sid, brain)
	ref := &ateapipb.ObjectRef{Atespace: sc.atespace, Name: brain}
	_, _ = ctrl.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: ref}) // fine if already suspended
	if _, err := ctrl.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref}); err != nil {
		return fmt.Errorf("delete session %s: %w", sid, err)
	}
	cleanupSnapshots(sc, brain)
	// Cascade the paired hand actor (best-effort — absent for non-hand agents).
	hand := naming.HandActor(sid)
	handRef := &ateapipb.ObjectRef{Atespace: sc.atespace, Name: hand}
	_, _ = ctrl.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: handRef})
	_, _ = ctrl.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: handRef})
	cleanupSnapshots(sc, hand)
	return nil
}

// createSession mints a session id and creates its brain actor from the given
// agent (template). Shared by `session new` and `serve`.
func createSession(ctx context.Context, sc sessionCtx, agent string) (string, error) {
	sid, err := naming.NewSessionID()
	if err != nil {
		return "", err
	}
	ctrl, closeFn, err := sc.dial()
	if err != nil {
		return "", err
	}
	defer closeFn()
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = ctrl.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
		Metadata:               &ateapipb.ResourceMetadata{Atespace: sc.atespace, Name: naming.BrainActor(sid)},
		ActorTemplateNamespace: sc.templateNS,
		ActorTemplateName:      agent,
	}})
	if err != nil {
		return "", fmt.Errorf("create brain actor: %w", err)
	}
	// Paired hand (h-<id>): if the agent is hand-enabled, create its hand actor
	// from the hand template so the brain has something to execute against. The
	// brain wires mcp.hand → h-<id> and denies local tools (finding: split).
	if agentWantsHand(ctx, sc, agent) {
		if _, err = ctrl.CreateActor(ctx, &ateapipb.CreateActorRequest{Actor: &ateapipb.Actor{
			Metadata:               &ateapipb.ResourceMetadata{Atespace: sc.atespace, Name: naming.HandActor(sid)},
			ActorTemplateNamespace: sc.templateNS,
			ActorTemplateName:      env("BRAIN_HAND_TEMPLATE", "hand"),
		}}); err != nil {
			// Roll back the brain so we don't leave a half-created session.
			_, _ = ctrl.DeleteActor(ctx, &ateapipb.DeleteActorRequest{
				Actor: &ateapipb.ObjectRef{Atespace: sc.atespace, Name: naming.BrainActor(sid)}})
			return "", fmt.Errorf("create hand actor: %w", err)
		}
		// Wait for the hand to actually serve before returning the session.
		//
		// A newly created hand takes a few seconds to restore from its golden.
		// The brain connects to the hand's MCP endpoint once, when its harness
		// initializes, and Claude Code caches the tool list at that moment — so
		// a brain that starts first is tool-less for the WHOLE turn. The harness
		// self-heals by restarting after the turn, but that means the user's
		// FIRST message silently runs with no tools, which is exactly when they
		// are deciding whether the product works.
		//
		// Bounded and non-fatal: on timeout we continue and let the self-heal
		// cover it, because a slow hand should delay a session, never fail one.
		waitHandReady(ctx, sc, sid, handReadyTimeout)

		// Tell the hand who it is: the actor has no ambient identity (no env,
		// no hostname, and the routed hop drops the Host header), so span
		// attribution depends on this push. Best-effort like the rest.
		if err := pushHandIdentity(ctx, sc, sid); err != nil {
			log.Printf("warn: push hand identity for %s: %v", sid, err)
		}
		// Hand-as-gateway: federate the agent's OWN MCP servers through the hand
		// (with any credentials) so the brain — which connects only to the hand —
		// sees those tools too. Best-effort: on failure the hand still serves its
		// own bash/fs tools. Must happen here, before the first message, because
		// the brain lists tools when it connects.
		if err := injectHandUpstreams(ctx, sc, sid, agent); err != nil {
			log.Printf("warn: federate hand upstreams for %s: %v", sid, err)
		}
	}
	return sid, nil
}

// mcpSrv mirrors the mcp entries encoded in AGENTPLANE_SPEC (json tags), so we
// can read them back off the template to federate them through the hand.
type mcpSrv struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers,omitempty"`
}

// templateMCP reads the agent template's embedded spec and returns its mcp map.
func templateMCP(ctx context.Context, sc sessionCtx, agent string) (map[string]mcpSrv, error) {
	out, err := runKubectl(ctx, "get", "actortemplate", "-n", sc.templateNS,
		"-o", `jsonpath={.spec.containers[0].env[?(@.name=="AGENTPLANE_SPEC")].value}`, "--", agent)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}
	var rt struct {
		MCP map[string]mcpSrv `json:"mcp"`
	}
	if err := json.Unmarshal([]byte(raw), &rt); err != nil {
		return nil, err
	}
	return rt.MCP, nil
}

// templateCredentials reads the agent template's embedded spec and returns the
// list of vault credential names its sessions declare.
func templateCredentials(ctx context.Context, sc sessionCtx, agent string) ([]string, error) {
	out, err := runKubectl(ctx, "get", "actortemplate", "-n", sc.templateNS,
		"-o", `jsonpath={.spec.containers[0].env[?(@.name=="AGENTPLANE_SPEC")].value}`, "--", agent)
	if err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil, nil
	}
	var rt struct {
		Credentials []string `json:"credentials"`
	}
	if err := json.Unmarshal([]byte(raw), &rt); err != nil {
		return nil, err
	}
	return rt.Credentials, nil
}

// injectHandUpstreams pushes the agent's mcp servers to the paired hand's
// /admin/upstreams over atenet. The hand connects outward to each (attaching the
// credential) and re-publishes their tools as its own. Retries the wake race —
// the hand may be cold, and the POST triggers atenet's auto-resume.
func injectHandUpstreams(ctx context.Context, sc sessionCtx, sid, agent string) error {
	mcp, err := templateMCP(ctx, sc, agent)
	if err != nil || len(mcp) == 0 {
		return err // nothing to federate — the hand still serves its own tools
	}
	type upstream struct {
		Name    string            `json:"name"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers,omitempty"`
	}
	ups := make([]upstream, 0, len(mcp))
	for name, cfg := range mcp {
		ups = append(ups, upstream{Name: name, URL: cfg.URL, Headers: cfg.Headers})
	}
	body, _ := json.Marshal(map[string]any{"upstreams": ups})
	hand := naming.HandActor(sid)
	url := fmt.Sprintf("http://%s/admin/upstreams", sc.atenet)
	adminTok := env("HAND_ADMIN_TOKEN", "")

	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		req.Host = naming.ActorDNS(hand, sc.atespace)
		req.Header.Set("Content-Type", "application/json")
		if adminTok != "" {
			req.Header.Set("Authorization", "Bearer "+adminTok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			if resp.StatusCode >= 400 {
				return fmt.Errorf("hand rejected upstreams: HTTP %d", resp.StatusCode)
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
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return lastErr
}

// pushHandIdentity tells the freshly created hand which session it belongs to
// via its admin plane — the actor itself has no ambient identity, and span
// attribution (hand.tool → agentplane.session) depends on it. Same wake-race
// retry shape as the other admin pushes.
func pushHandIdentity(ctx context.Context, sc sessionCtx, sid string) error {
	hand := naming.HandActor(sid)
	body, _ := json.Marshal(map[string]string{"session": sid, "actor": hand})
	url := fmt.Sprintf("http://%s/admin/identity", sc.atenet)
	adminTok := env("HAND_ADMIN_TOKEN", "")

	var lastErr error
	for attempt := 1; attempt <= 4; attempt++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		req.Host = naming.ActorDNS(hand, sc.atespace)
		req.Header.Set("Content-Type", "application/json")
		if adminTok != "" {
			req.Header.Set("Authorization", "Bearer "+adminTok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode < 500 {
			resp.Body.Close()
			if resp.StatusCode >= 400 {
				return fmt.Errorf("hand rejected identity: HTTP %d", resp.StatusCode)
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
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	return lastErr
}

// agentWantsHand reports whether the agent template is labeled for a paired hand.
// handReadyTimeout bounds the wait for a new hand to come up. Restores observed
// at ~5s; this leaves headroom for a cold worker without stalling a session
// creation indefinitely.
const handReadyTimeout = 25 * time.Second

// waitHandReady polls the hand's health endpoint until it answers. Returns
// whether it became ready; callers treat false as "continue anyway".
func waitHandReady(ctx context.Context, sc sessionCtx, sid string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	hand := naming.HandActor(sid)
	url := fmt.Sprintf("http://%s/healthz", sc.atenet)
	host := naming.ActorDNS(hand, sc.atespace)
	client := &http.Client{Timeout: 3 * time.Second}
	for attempt := 0; time.Now().Before(deadline); attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false
		}
		req.Host = host
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 500 {
				return true
			}
		}
		if ctx.Err() != nil {
			return false
		}
		time.Sleep(500 * time.Millisecond)
	}
	log.Printf("warn: hand for %s not ready within %s — first turn may run tool-less "+
		"until the harness reconnects", sid, budget)
	return false
}

func agentWantsHand(ctx context.Context, sc sessionCtx, agent string) bool {
	out, err := runKubectl(ctx, "get", "actortemplate", "-n", sc.templateNS,
		"-o", `jsonpath={.metadata.labels.agentplane\.io/hand}`, "--", agent)
	return err == nil && strings.TrimSpace(string(out)) == "true"
}
