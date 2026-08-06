// Fleet view for operators (#70).
//
// Every other /v1 route is scoped to the caller — a session you do not own
// answers 404, and that is load-bearing (threat-model F1). So "what is running
// across the cluster, and whose is it?" had no answer short of kubectl plus a
// Firestore lookup by hand.
//
// Two things this surfaces that nothing else does:
//
//   - HANDS. listSessionActors filters to `b-` prefixed actors, so every
//     listing in the product ignores the hand half of a split agent. A wedged
//     or missing hand breaks tool execution while the brain looks perfectly
//     healthy, and until now it appeared nowhere.
//   - OWNERSHIP across users. The actor registry knows nothing about people;
//     Firestore holds sessions/{sid}.owner. Nothing joined them.
package main

import (
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"

	"github.com/agent-substrate/substrate/pkg/proto/ateapipb"
	"github.com/quantumnode/agentplane/internal/naming"
)

// adminAllowlist decides who may see the fleet. Deliberately the same shape as
// the email allowlist that gates access itself: a mounted ConfigMap and/or an
// env var, hot-reloaded, so granting an operator does not need a redeploy.
type adminAllowlist struct {
	mu   sync.RWMutex
	set  map[string]bool
	file string
}

func newAdminAllowlist() *adminAllowlist {
	a := &adminAllowlist{set: map[string]bool{}, file: os.Getenv("AGENTPLANE_ADMIN_EMAILS_FILE")}
	a.reload()
	return a
}

func (a *adminAllowlist) reload() {
	next := map[string]bool{}
	consume := func(s string) {
		for _, line := range strings.Split(s, "\n") {
			if i := strings.IndexByte(line, '#'); i >= 0 {
				line = line[:i]
			}
			for _, part := range strings.FieldsFunc(line, func(r rune) bool {
				return r == ',' || r == '\r' || r == ' ' || r == '\t'
			}) {
				e := strings.ToLower(strings.TrimSpace(part))
				if emailRe.MatchString(e) {
					next[e] = true
				}
			}
		}
	}
	if a.file != "" {
		if data, err := os.ReadFile(a.file); err == nil {
			consume(string(data))
		}
	}
	consume(os.Getenv("AGENTPLANE_ADMIN_EMAILS"))
	a.mu.Lock()
	a.set = next
	a.mu.Unlock()
}

// is reports whether this user may see the fleet. An empty allowlist grants
// nobody: a missing config must not silently open a cross-user view.
func (a *adminAllowlist) is(user string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.set[strings.ToLower(strings.TrimSpace(user))]
}

func (a *adminAllowlist) count() int {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return len(a.set)
}

// adminActor is one running actor, joined to the person who owns it.
type adminActor struct {
	Actor   string `json:"actor"`
	Role    string `json:"role"`    // brain | hand
	Session string `json:"session"` // sess-…
	Agent   string `json:"agent"`   // logical agent, version suffix stripped
	Version int    `json:"version"` // 0 when the template is unversioned
	Status  string `json:"status"`
	Owner   string `json:"owner,omitempty"`
	// Paired reports whether this actor's COUNTERPART exists — the hand for a
	// brain, the brain for a hand. Both directions are failures worth seeing:
	// a brain without its hand stays responsive and fails every tool call, and
	// a hand without its brain is a leaked actor holding a DurableDir and
	// snapshots for a session nobody can reach (#71).
	//
	// Defined for both roles on purpose. It was originally hardcoded true for
	// hands, which made `paired == false` useless as an orphan query: the two
	// leaked hands on this cluster reported themselves as fine.
	Paired bool `json:"paired"`
}

// agentFromTemplate splits a template name back into agent + version, using the
// same rule templateFor() applies when composing it.
func agentFromTemplate(template string) (string, int) {
	m := versionSuffixRe.FindString(template)
	if m == "" {
		return template, 0
	}
	v := 0
	for _, r := range m[2:] { // skip "-v"
		v = v*10 + int(r-'0')
	}
	return strings.TrimSuffix(template, m), v
}

// counterpartOf names the other half of a split agent.
func counterpartOf(actor, sid string) string {
	if strings.HasPrefix(actor, "h-") {
		return naming.BrainActor(sid)
	}
	return naming.HandActor(sid)
}

// handleAdminActors lists every brain and hand in the atespace.
func (s *server) handleAdminActors(w http.ResponseWriter, r *http.Request) {
	user := userOf(r)
	if s.admins == nil || !s.admins.is(user) {
		// 404, not 403 — the same reasoning that hides sessions you do not own.
		// A non-admin should not learn that a fleet view exists.
		s.log.Warn("admin route denied", "user", user, "path", r.URL.Path) // audit
		writeErr(w, http.StatusNotFound, "not found")
		return
	}

	ctrl, closeFn, err := s.sc.dial()
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "cannot reach the control plane", err)
		return
	}
	defer closeFn()

	// Page through every actor. Unlike listSessionActors this keeps BOTH roles:
	// the hand half is the whole point.
	var all []*ateapipb.Actor
	token := ""
	for {
		resp, err := ctrl.ListActors(r.Context(), &ateapipb.ListActorsRequest{PageToken: token})
		if err != nil {
			s.fail(w, r, http.StatusBadGateway, "cannot list actors", err)
			return
		}
		for _, a := range resp.GetActors() {
			if a.GetMetadata().GetAtespace() == s.sc.atespace {
				all = append(all, a)
			}
		}
		if token = resp.GetNextPageToken(); token == "" {
			break
		}
	}

	// Which actors exist, so either half can be reported as unpaired.
	present := map[string]bool{}
	for _, a := range all {
		present[a.GetMetadata().GetName()] = true
	}

	probe := r.URL.Query().Get("probe") == "true"
	out := make([]adminActor, 0, len(all))
	for _, a := range all {
		name := a.GetMetadata().GetName()
		sid, ok := naming.SessionFromActor(name)
		if !ok {
			continue // not a session actor (golden bake helpers and the like)
		}
		role := "brain"
		if strings.HasPrefix(name, "h-") {
			role = "hand"
		}
		agent, version := agentFromTemplate(a.GetActorTemplateName())

		// Raw status by default. derivedStatus health-probes a RUNNING actor,
		// and at fleet scale that is one HTTP call per actor on every page load
		// — turning "look at the dashboard" into a burst of traffic. Suspended
		// actors are never probed either way, so looking cannot wake a mind.
		status := strings.ToLower(strings.TrimPrefix(a.GetStatus().String(), "STATUS_"))
		if status == "suspended" {
			status = "sleeping"
		} else if probe && role == "brain" {
			status = derivedStatus(s.sc, name, a.GetStatus().String())
		}

		item := adminActor{
			Actor: name, Role: role, Session: sid,
			Agent: agent, Version: version, Status: status,
			Paired: present[counterpartOf(name, sid)],
		}
		if s.owners != nil {
			if o, ok := s.owners.owner(sid); ok {
				item.Owner = o
			}
		}
		out = append(out, item)
	}

	// Group a session's brain and hand together, so the pair reads as one unit
	// rather than being scattered by whatever order the registry returned.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Session != out[j].Session {
			return out[i].Session < out[j].Session
		}
		return out[i].Role < out[j].Role
	})

	s.log.Info("admin fleet view", "user", user, "actors", len(out), "probed", probe)
	writeJSON(w, http.StatusOK, map[string]any{"actors": out, "count": len(out)})
}
