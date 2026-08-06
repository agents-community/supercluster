// Approval relay (#67): serve owns identity, the brain owns the decision.
//
// serve deliberately does not keep its own copy of what is pending. The brain's
// event log is the record — approvals outlive a serve restart, and a second
// store would be a second answer to the same question. serve's job here is the
// part only it can do: prove the caller owns this session before letting them
// approve anything.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/quantumnode/agentplane/internal/naming"
)

// brainRequest issues an HTTP call to a session's brain actor through atenet.
// Shared by both approval routes so the Host-header routing is written once.
func brainRequest(ctx context.Context, sc sessionCtx, sid, method, path string, body []byte) (int, []byte, error) {
	brain := naming.BrainActor(sid)
	url := fmt.Sprintf("http://%s/v1/sessions/%s%s", sc.atenet, brain, path)
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return 0, nil, err
	}
	req.Host = naming.ActorDNS(brain, sc.atespace)
	req.Header.Set("Content-Type", "application/json")
	// Generous: the brain may be asleep and this request is what wakes it.
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, out, err
}

// handleApprovalList answers what this session is still waiting on.
func (s *server) handleApprovalList(w http.ResponseWriter, r *http.Request) {
	sid, _, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	code, body, err := brainRequest(r.Context(), s.sc, sid, http.MethodGet, "/approvals", nil)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "cannot reach the session", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(body)
}

// handleApprovalDecide answers one request.
//
// Note this WAKES the session: the brain records the decision and queues an
// input so the agent retries the call it was blocked on. Approving is therefore
// an action, not a note for later — which is the behaviour you want, since an
// approval nobody acts on is indistinguishable from being ignored.
func (s *server) handleApprovalDecide(w http.ResponseWriter, r *http.Request) {
	sid, _, ok := s.ownedSession(w, r)
	if !ok {
		return
	}
	req := r.PathValue("req")
	if req == "" {
		writeErr(w, http.StatusBadRequest, "missing approval id")
		return
	}
	var in struct {
		Decision string `json:"decision"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	// Checked here as well as in the brain so a typo cannot reach the actor at
	// all — and so the error names the session's owner in serve's audit log.
	if in.Decision != "approve" && in.Decision != "deny" {
		writeErr(w, http.StatusBadRequest, `decision must be "approve" or "deny"`)
		return
	}
	body, _ := json.Marshal(map[string]any{"decision": in.Decision, "note": in.Note})
	code, out, err := brainRequest(r.Context(), s.sc, sid, http.MethodPost, "/approvals/"+req, body)
	if err != nil {
		s.fail(w, r, http.StatusBadGateway, "cannot reach the session", err)
		return
	}
	// Who approved what, on the record. This is the audit trail for a human
	// decision that let a gated tool run.
	s.log.Info("approval decided", "session", sid, "request", req,
		"decision", in.Decision, "user", userOf(r), "brain_status", code)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(out)
}
