// Readiness for atenet, the component /readyz could not see (#60).
//
// atenet has now gone stale twice: newly created actors stopped being routable
// while every existing session kept working. Both times /readyz was green, and
// both times it was green *correctly* — it dialed ateapi and read the registry
// from valkey, and that path was genuinely fine. atenet sits on a different
// path (Envoy + xDS, fed by atenet's own watches of ateapi and the Kubernetes
// API), and nothing in the readiness check touched it. The outage was found by
// hand, from a user reporting that a new session never answered.
//
// atenet publishes what is needed on its status port: the health of each
// dependency it watches, and the set of ActorTemplates it currently believes
// exist. That second list is the useful one — it is atenet's *view*, and when
// the view drifts from the cluster, routes for anything missing from it cannot
// be programmed. That is the stale state, observable directly rather than
// inferred from a user complaint.
//
// What this does not prove: that a specific actor is reachable end to end.
// Proving that means routing a real request to a real actor, and probing an
// actor resumes it — the same trap that made usage accounting wake sleeping
// sessions. These checks are leading indicators of the failure, not a
// substitute for the smoke test.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
)

// atenetStatus is the subset of /statusz this cares about. atenet reports far
// more (build tag, flags, recent queries, parking); decoding only these two
// keeps the check working when the rest of the document changes shape.
type atenetStatus struct {
	Health map[string]struct {
		Healthy bool   `json:"healthy"`
		Message string `json:"message"`
	} `json:"health"`
	Templates []struct {
		Name      string `json:"name"`
		Namespace string `json:"namespace"`
	} `json:"templates"`
}

// atenetStatusAddr derives the status endpoint from the routing address.
//
// SUBSTRATE_ATENET is the HTTP data port (…:80); /statusz is on the status port
// (…:4040) of the same service. Deriving rather than requiring a second env var
// means an existing deployment gains this check without a config change — and
// an operator whose ports differ can still override it outright.
func atenetStatusAddr(atenet string) string {
	if v := env("SUBSTRATE_ATENET_STATUS", ""); v != "" {
		return v
	}
	host := atenet
	if h, _, err := net.SplitHostPort(atenet); err == nil {
		host = h
	}
	return net.JoinHostPort(host, "4040")
}

// fetchAtenetStatus reads /statusz. A failure here is itself a finding: serve
// reaches atenet over the same in-cluster network it uses to route every turn.
func fetchAtenetStatus(ctx context.Context, atenet string) (*atenetStatus, error) {
	url := fmt.Sprintf("http://%s/statusz?format=json", atenetStatusAddr(atenet))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("statusz returned %d", resp.StatusCode)
	}
	var st atenetStatus
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&st); err != nil {
		return nil, fmt.Errorf("decode statusz: %w", err)
	}
	return &st, nil
}

// unhealthySubsystems names atenet's own dependencies that are failing, sorted
// so the message is stable across probes. atenet watches ateapi and the k8s API
// to build its routing view and health-checks the Envoy it manages; any of the
// three being down is how the view goes stale in the first place.
func unhealthySubsystems(st *atenetStatus) []string {
	var bad []string
	for name, h := range st.Health {
		if !h.Healthy {
			bad = append(bad, fmt.Sprintf("%s (%s)", name, h.Message))
		}
	}
	sort.Strings(bad)
	return bad
}

// missingTemplates returns templates that exist in the cluster but that atenet
// does not know about — sessions pinned to any of them are unroutable.
//
// Deliberately one-directional. atenet legitimately knows templates this does
// not ask about (its own sandbox template, and anything in another namespace),
// so an entry atenet has and the cluster does not is not evidence of a problem.
// Only the reverse is.
func missingTemplates(st *atenetStatus, ns string, cluster []string) []string {
	known := make(map[string]bool, len(st.Templates))
	for _, t := range st.Templates {
		// Namespace-scope the comparison: two namespaces may hold a template of
		// the same name, and matching on name alone would mask a real gap.
		if t.Namespace == ns {
			known[t.Name] = true
		}
	}
	var missing []string
	for _, name := range cluster {
		if !known[name] {
			missing = append(missing, name)
		}
	}
	sort.Strings(missing)
	return missing
}

// clusterTemplates lists ActorTemplate names in the agent namespace.
//
// Uses the raw list rather than listAgents(), which collapses versions into one
// entry per logical agent and hides hands. Both are exactly what must be
// checked here: each version is its own template, and an unroutable hand breaks
// tool execution just as thoroughly as an unroutable brain.
func clusterTemplates(ctx context.Context, sc sessionCtx) ([]string, error) {
	out, err := runKubectl(ctx, "get", "actortemplates", "-n", sc.templateNS,
		"-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.Fields(string(out)), nil
}

// checkAtenet reports a component name and message when atenet is not fit to
// route, or "" when it is.
func checkAtenet(ctx context.Context, sc sessionCtx) (component, message string, err error) {
	st, err := fetchAtenetStatus(ctx, sc.atenet)
	if err != nil {
		return "atenet", "cannot reach atenet status endpoint", err
	}
	if bad := unhealthySubsystems(st); len(bad) > 0 {
		return "atenet-deps", "atenet dependencies are unhealthy",
			fmt.Errorf("unhealthy: %s", strings.Join(bad, ", "))
	}

	cluster, err := clusterTemplates(ctx, sc)
	if err != nil {
		// Not fatal to readiness: this is the freshness check's input, not a
		// property of the serving path. Reporting it as an atenet failure would
		// blame the wrong component for a kubectl or RBAC problem.
		return "", "", nil
	}
	if missing := missingTemplates(st, sc.templateNS, cluster); len(missing) > 0 {
		return "atenet-stale", "atenet's routing view is missing templates that exist in the cluster",
			fmt.Errorf("unknown to atenet: %s", strings.Join(missing, ", "))
	}
	return "", "", nil
}
