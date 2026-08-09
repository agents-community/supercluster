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
	"net/url"
	"os"
	"os/exec"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"

	"github.com/quantumnode/agentplane/internal/agentspec"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

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
	// Same rule as the serve path: the brain image is resolved from the harness
	// when the spec omits it, so an author never pins a per-deployment digest.
	if spec.Image == "" {
		img := brainImageFor(spec.Harness)
		if img == "" {
			log.Fatalf("agent create: no brain image for harness %q — set image: in the spec, "+
				"or export AGENTPLANE_BRAIN_IMAGE_* (e.g. `source infra/config.env`)", spec.Harness)
		}
		spec.Image = img
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
	// NotFound is success: the cascade must be idempotent, because a retry
	// after a partial failure has to get past an already-deleted brain to
	// reach the hand that is still there.
	if _, err := ctrl.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: ref}); err != nil &&
		status.Code(err) != codes.NotFound {
		return fmt.Errorf("delete session %s: %w", sid, err)
	}
	cleanupSnapshots(sc, brain)

	// Cascade the paired hand. NotFound is the ONLY expected failure — that is
	// a single-actor agent with no hand to remove.
	//
	// Every other error used to be discarded (`_, _ =`), and the caller treats
	// a nil return as success: it answers 200 and releases the Firestore
	// ownership record. So one transient ateapi failure leaked the hand, its
	// DurableDir and its GCS snapshots permanently, AND erased the only record
	// of who owned it — which is exactly how the orphans found by the fleet
	// view ended up with no owner (#71).
	hand := naming.HandActor(sid)
	handRef := &ateapipb.ObjectRef{Atespace: sc.atespace, Name: hand}
	_, _ = ctrl.SuspendActor(ctx, &ateapipb.SuspendActorRequest{Actor: handRef})
	if _, err := ctrl.DeleteActor(ctx, &ateapipb.DeleteActorRequest{Actor: handRef}); err != nil {
		if status.Code(err) == codes.NotFound {
			return nil // no hand: nothing to cascade
		}
		// Deliberately NOT removing its snapshots: the actor still exists, and
		// deleting the snapshots of a live actor leaves one that cannot be
		// restored. Returning the error keeps the ownership record too, so the
		// leak stays attributable and the delete can be retried.
		return fmt.Errorf("delete session %s: brain removed but its hand %s remains "+
			"(retry the delete): %w", sid, hand, err)
	}
	cleanupSnapshots(sc, hand)
	return nil
}

// createSession mints a session id and creates its brain actor from the given
// agent (template). Shared by `session new` and `serve`.
// createSession mints a session. user and v carry the caller's identity and
// credential vault so per-user MCP auth can be resolved at setup (#46); the CLI
// passes ("", nil) and gets unauthenticated upstreams.
func createSession(ctx context.Context, sc sessionCtx, agent, user string, v *vault) (string, error) {
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
		//
		// With the broker, the brain connects to the broker (not the hand), and
		// the broker resumes the hand on the first exec — so there is no gateway
		// to wait on and this wait is skipped.
		if sc.brokerMCPBase == "" {
			waitHandReady(ctx, sc, sid, handReadyTimeout)
		}

		// Federated MCP (a user's own MCP servers) is not re-homed in the broker
		// yet — it used to be federated through the in-actor gateway, which
		// hand-v15 no longer runs. `federated` stays empty until the broker
		// federates upstreams; non-federated agents are unaffected. See the broker
		// README / threat-model for the deferred item.
		federated := map[string][]string{}
		// Now that the upstreams' real tool names are known, translate the
		// spec's tool policy into them and push it to the brain (#58). Must
		// precede the first message: the harness reads its options when it
		// starts, and the brain lists tools when it connects.
		// Stash rather than push: the brain is not reachable yet at create — it
		// wakes on the first message — so pushing here reliably 504s. The send
		// path applies it, where the brain is already being woken.
		allow := templateAllow(ctx, sc, agent)
		// An allow entry that matches nothing is a misconfiguration that fails
		// OPEN, so it stops session creation rather than being logged (#58).
		// Misspell the upstream — `gihub/list_issues` — and the real `github`
		// is never scoped, so every one of its tools stays permitted and the
		// spec reads as if it restricted them. This is the same class of
		// silent no-op as `allow: [mcp__github__*]`, which is what prompted the
		// whole resolution path.
		if bad := unmatchedScopes(allow, federated); len(bad) > 0 {
			// Roll back: the actors exist by now, and leaving them behind would
			// strand a brain and a hand for a session no caller ever learns of.
			if derr := deleteSession(ctx, sc, sid, naming.BrainActor(sid)); derr != nil {
				log.Printf("warn: rollback of %s after invalid tool policy: %v", sid, derr)
			}
			return "", fmt.Errorf("agent %q allows tools that no connected MCP server exposes: %s "+
				"(connected: %s)", agent, strings.Join(bad, ", "), strings.Join(upstreamNames(federated), ", "))
		}
		if deny := resolveFederatedDeny(allow, federated); len(deny) > 0 {
			rememberFederatedDeny(sid, deny)
			log.Printf("session %s: %d federated tool(s) will be denied on first turn", sid, len(deny))
		}
	}
	return sid, nil
}

// templateAllow reads the agent's `allow` list off the compiled template.
func templateAllow(ctx context.Context, sc sessionCtx, agent string) []string {
	out, err := runKubectl(ctx, "get", "actortemplate", "-n", sc.templateNS,
		"-o", `jsonpath={.spec.containers[0].env[?(@.name=="AGENTPLANE_SPEC")].value}`, "--", agent)
	if err != nil {
		return nil
	}
	raw := strings.TrimSpace(string(out))
	if raw == "" {
		return nil
	}
	var rt struct {
		Allow []string `json:"allow"`
	}
	if json.Unmarshal([]byte(raw), &rt) != nil {
		return nil
	}
	return rt.Allow
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

// templateCredentialNames is templateCredentials with errors swallowed — the
// caller wants a grant scoped to whatever the agent declares, and an agent that
// declares none still needs a grant for public-repo traffic to carry a session
// identity the proxy can log.
func templateCredentialNames(ctx context.Context, sc sessionCtx, agent string) []string {
	names, err := templateCredentials(ctx, sc, agent)
	if err != nil {
		return nil
	}
	return names
}

// templateRepositories reads the agent's declared repositories off the template.
func templateRepositories(ctx context.Context, sc sessionCtx, agent string) ([]agentspec.Repository, error) {
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
		Repositories []agentspec.Repository `json:"repositories"`
	}
	if err := json.Unmarshal([]byte(raw), &rt); err != nil {
		return nil, fmt.Errorf("parse repositories: %w", err)
	}
	return rt.Repositories, nil
}

// injectHandRepositories points the hand's git at the proxy and clones the
// agent's declared repositories, BEFORE the first message so the workspace is
// ready when the model starts.
//
// The grant travels to the hand, not the credential: the proxy exchanges it for
// the real token outside the sandbox, so the token never enters actor memory
// and cannot be captured by a checkpoint (#49).
func injectHandRepositories(ctx context.Context, sc sessionCtx, sid, agent, grant string) error {
	repos, err := templateRepositories(ctx, sc, agent)
	if err != nil || len(repos) == 0 {
		return err
	}
	proxyBase := env("AGENTPLANE_GIT_PROXY", "http://git-proxy.agentplane.svc")
	// One credential name for the session's git traffic. Repositories may name
	// different credentials, but git's extraHeader is global to the process, so
	// the first declared one wins; a second distinct name is a config we cannot
	// honor and should not silently ignore.
	cred := ""
	for _, r := range repos {
		if r.Credential == "" {
			continue
		}
		if cred != "" && cred != r.Credential {
			log.Printf("warn: session %s declares repositories with different credentials (%q, %q); "+
				"git sends one header per process, so %q is used for all", sid, cred, r.Credential, cred)
			continue
		}
		cred = r.Credential
	}
	// Configure git in the zone and clone, run via ExecActor (the executor is
	// outside the sandbox now — there is no in-actor gateway to POST to). The
	// grant travels in git's extraHeader; git-proxy exchanges it for the real
	// token OUTSIDE the actor, so the credential never enters actor memory (#49).
	ctrl, closeFn, err := sc.dial()
	if err != nil {
		return fmt.Errorf("dial control plane: %w", err)
	}
	defer closeFn()
	hand := naming.HandActor(sid)
	base := strings.TrimSuffix(proxyBase, "/")

	// git config --global … (each a separate exec; --global writes ~/.gitconfig
	// in the zone, which persists in the actor's fs delta across suspend/resume).
	gitCfg := [][]string{
		{"git", "config", "--global", "credential." + base + ".helper", ""},
		{"git", "config", "--global", "url." + base + "/gh/.insteadOf", "https://github.com/"},
		{"git", "config", "--global", "--replace-all", "http.extraHeader", "X-Agentplane-Grant: " + grant},
	}
	if cred != "" {
		gitCfg = append(gitCfg, []string{"git", "config", "--global", "--add", "http.extraHeader", "X-Agentplane-Credential: " + cred})
	}
	for _, argv := range gitCfg {
		if _, err := execInHand(ctx, ctrl, sc.atespace, hand, "exec", argv, "/workspace", nil, 30000); err != nil {
			return fmt.Errorf("git config in hand: %w", err)
		}
	}
	// Clone each declared repo, idempotently (durable /workspace may already hold
	// a checkout from a prior run — never clobber uncommitted work).
	for _, r := range repos {
		if r.URL == "" {
			continue
		}
		dir := r.MountPath
		if dir == "" {
			dir = repoDirFromURL(r.URL)
		}
		script := fmt.Sprintf(`[ -e %q ] && echo "exists: %s" || git clone %q %q`, dir, dir, r.URL, dir)
		res, err := execInHand(ctx, ctrl, sc.atespace, hand, "exec", []string{"/bin/sh", "-c", script}, "/workspace", nil, 120000)
		if err != nil {
			return fmt.Errorf("clone %s: %w", r.URL, err)
		}
		if res.GetExitCode() != 0 {
			se := string(res.GetStderr())
			if len(se) > 200 {
				se = se[:200]
			}
			log.Printf("warn: session %s clone %s exit %d: %s", sid, r.URL, res.GetExitCode(), se)
		}
	}
	log.Printf("session %s: git configured + %d repositories cloned via ExecActor", sid, len(repos))
	return nil
}

// repoDirFromURL derives a checkout directory from a repo URL (the last path
// segment, minus a trailing .git) when the spec does not name one.
func repoDirFromURL(u string) string {
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	if i := strings.LastIndex(u, "/"); i >= 0 {
		u = u[i+1:]
	}
	if u == "" {
		return "repo"
	}
	return u
}

// pendingDeny holds resolved tool policy between session create (where the
// upstream tool names become known) and the first send (where the brain is
// awake enough to receive it).
//
// In memory on purpose: it is a cache of something recomputable, and a serve
// restart between the two loses only an unsent restriction — which the send
// path logs. Persisting it would mean a second store for data with a lifetime
// of seconds.
var pendingDeny sync.Map // sid -> []string

func rememberFederatedDeny(sid string, deny []string) { pendingDeny.Store(sid, deny) }

// takeFederatedDeny returns and clears the pending policy for a session.
func takeFederatedDeny(sid string) []string {
	v, ok := pendingDeny.LoadAndDelete(sid)
	if !ok {
		return nil
	}
	d, _ := v.([]string)
	return d
}

// resolveFederatedDeny turns spec-level tool policy into the runtime tool names
// the model will actually see (#58).
//
// The spec is compiled onto the template before any upstream is dialled, so it
// cannot name a federated tool: at that point nobody knows a `github` server
// exposes `create_issue`. The hand reports that at session setup, and the model
// sees it as `mcp__hand__github__create_issue`.
//
// The rule: naming ANY tool of an upstream turns that upstream into an
// allow-list. `allow: [github/create_issue]` permits that one and denies the
// rest of github's tools. An upstream nobody names is untouched, so adding this
// changes nothing for specs that do not use it.
//
// Returns only DENIALS. That is deliberate — the result is unioned with the
// spec's own deny list downstream, so this can only ever remove capability. A
// bug here cannot grant a tool the operator disabled.
// upstreamNames lists the connected upstreams, sorted, for error messages —
// "you wrote gihub, these are the servers that actually connected" is the whole
// diagnosis.
func upstreamNames(federated map[string][]string) []string {
	names := make([]string, 0, len(federated))
	for up := range federated {
		names = append(names, up)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return []string{"none"}
	}
	return names
}

// unmatchedScopes returns spec allow entries of the form `upstream/tool` that
// name something the hand did not report.
//
// Two shapes, both misconfigurations, only one of which is dangerous:
//
//   - Unknown UPSTREAM. Fails open — the upstream the author meant is left
//     unscoped, so every tool on it stays permitted while the spec looks like
//     it restricted them.
//   - Unknown TOOL on a known upstream. Fails closed, because naming any tool
//     turns that upstream into an allow-list and the misspelling matches
//     nothing. Safe, but the agent silently cannot do the one thing it was
//     allowed to, which is worth failing on rather than debugging later.
//
// Returns nothing when no upstream connected at all: the tools do not exist, so
// nothing is granted, and failing a session because a third-party MCP server is
// down would be a worse outcome than running without its tools.
func unmatchedScopes(allow []string, federated map[string][]string) []string {
	if len(federated) == 0 {
		return nil
	}
	var bad []string
	for _, a := range allow {
		up, tool, ok := strings.Cut(a, "/")
		if !ok || up == "" || tool == "" {
			continue // a plain tool name (Bash, …) — not our concern
		}
		tools, known := federated[up]
		if !known {
			bad = append(bad, a+" (no such server)")
			continue
		}
		if tool == "*" {
			continue
		}
		if !slices.Contains(tools, tool) {
			bad = append(bad, a+" (server has no such tool)")
		}
	}
	sort.Strings(bad)
	return bad
}

func resolveFederatedDeny(allow []string, federated map[string][]string) []string {
	if len(federated) == 0 {
		return nil
	}
	// upstream -> the tools explicitly permitted on it
	permitted := map[string]map[string]bool{}
	for _, a := range allow {
		up, tool, ok := strings.Cut(a, "/")
		if !ok || up == "" || tool == "" {
			continue // a plain tool name (Bash, …) — not our concern
		}
		if permitted[up] == nil {
			permitted[up] = map[string]bool{}
		}
		permitted[up][tool] = true
	}
	if len(permitted) == 0 {
		return nil
	}
	var deny []string
	for up, tools := range federated {
		want := permitted[up]
		if want == nil {
			continue // this upstream was not scoped; leave it alone
		}
		if want["*"] {
			continue // explicit "all of this upstream"
		}
		for _, t := range tools {
			if !want[t] {
				deny = append(deny, fmt.Sprintf("mcp__hand__%s__%s", up, t))
			}
		}
	}
	sort.Strings(deny) // stable, so a redeploy does not churn the pushed policy
	return deny
}

// brokerHandURL is the URL the brain should dial for its hand when the broker is
// enabled: the broker's MCP endpoint, carrying the real hand's actor MCP URL as
// the ?hand= target so the broker knows which hand to forward to.
func brokerHandURL(sc sessionCtx, sid string) string {
	if sc.brokerMCPBase == "" {
		return ""
	}
	hand := naming.HandActor(sid)
	handMCP := fmt.Sprintf("http://%s/mcp", naming.ActorDNS(hand, sc.atespace))
	sep := "?"
	if strings.Contains(sc.brokerMCPBase, "?") {
		sep = "&"
	}
	return sc.brokerMCPBase + sep + "hand=" + url.QueryEscape(handMCP)
}

// pushBrainOptions hands the resolved restrictions and (when the broker is
// enabled) the hand MCP URL to the brain. Best-effort by necessity — but the
// failure direction matters: without it the brain keeps the spec's policy and
// dials the hand directly, so a missed push is a missing restriction / a
// bypassed broker, never an opening.
func pushBrainOptions(ctx context.Context, sc sessionCtx, sid string, deny []string, handMcpURL string) error {
	if len(deny) == 0 && handMcpURL == "" {
		return nil
	}
	payload := map[string]any{}
	if len(deny) > 0 {
		payload["disallowedTools"] = deny
	}
	if handMcpURL != "" {
		payload["handMcpUrl"] = handMcpURL
	}
	body, _ := json.Marshal(payload)
	brain := naming.BrainActor(sid)
	url := fmt.Sprintf("http://%s/v1/sessions/%s/options", sc.atenet, brain)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Host = naming.ActorDNS(brain, sc.atespace)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("brain rejected options: HTTP %d", resp.StatusCode)
	}
	log.Printf("session %s: pushed options (%d federated deny, broker=%t)", sid, len(deny), handMcpURL != "")
	return nil
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
