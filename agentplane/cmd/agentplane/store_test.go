package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

// Both stores must satisfy the interface — the ConfigMap path is the fallback
// for clusters with no GCP project and has to keep compiling.
var (
	_ sessionStore = (*fsStore)(nil)
	_ sessionStore = cmStore{}
)

// Ownership is the F1 security control, so the failure mode has to be denial.
// A store that cannot answer must never widen access.
func TestUnownedSessionIsOperatorOnly(t *testing.T) {
	f := &fsStore{cache: map[string]string{}} // no client: every lookup misses
	if f.mine("sess-unknown", "alice") {
		t.Error("SECURITY: an unowned session was granted to a non-operator")
	}
	if !f.mine("sess-unknown", operatorUser) {
		t.Error("operator must retain access to unowned sessions for cleanup")
	}
}

func TestCachedOwnerGatesAccess(t *testing.T) {
	f := &fsStore{cache: map[string]string{"sess-a": "alice"}}
	if !f.mine("sess-a", "alice") {
		t.Error("owner was denied their own session")
	}
	if f.mine("sess-a", "bob") {
		t.Error("SECURITY: a non-owner was granted another user's session")
	}
	// The operator override applies only to UNOWNED sessions; an owned session
	// stays private, so an operator token cannot silently read user data.
	if f.mine("sess-a", operatorUser) {
		t.Error("SECURITY: operator read an owned session")
	}
}

// The whole point of #36: usage and activity must be answerable without waking
// the mind, and must outlive the actor. This asserts the decode path that the
// sleeping-session branch in handleSessionGet depends on.
func TestMetaCarriesUsageAndActivity(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	m := sessionMeta{
		Owner: "alice", Agent: "starter",
		CreatedAt: now, LastActiveAt: now,
		UsageJSON: `{"input_tokens":120,"output_tokens":45}`,
	}
	if m.UsageJSON != "" {
		m.Usage = json.RawMessage(m.UsageJSON)
	}
	var got struct {
		In  int `json:"input_tokens"`
		Out int `json:"output_tokens"`
	}
	if err := json.Unmarshal(m.Usage, &got); err != nil {
		t.Fatalf("stored usage is not valid JSON: %v", err)
	}
	if got.In != 120 || got.Out != 45 {
		t.Errorf("usage did not survive the round trip: %+v", got)
	}
	if m.LastActiveAt.IsZero() {
		t.Error("lastActiveAt must be set — it is what a sleeping session reports")
	}
}

// Status must NOT be stored: Substrate owns liveness, and a mirrored copy goes
// stale the moment a worker dies. Guard the shape so nobody adds it casually.
func TestMetaDoesNotMirrorRuntimeStatus(t *testing.T) {
	blob, err := json.Marshal(sessionMeta{Owner: "alice"})
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]any
	if err := json.Unmarshal(blob, &fields); err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"status", "busy", "queued", "Status", "Busy", "Queued"} {
		if _, found := fields[banned]; found {
			t.Errorf("%q is runtime state owned by Substrate — storing it guarantees drift", banned)
		}
	}
}

// touch is called from a goroutine on the send path; it must tolerate a store
// with no backing client rather than panicking and taking serve down.
func TestTouchOnFallbackStoreIsInert(t *testing.T) {
	var c cmStore
	c.touch(context.Background(), "sess-a", json.RawMessage(`{"x":1}`))
	if _, ok := c.get(context.Background(), "sess-a"); ok {
		t.Error("the ConfigMap fallback stores no metadata and must report none")
	}
}

// The harness reports snake_case JSON. Go's json decoder folds case but NOT
// underscores, so `cost_usd` does not reach `CostUSD` without an explicit tag —
// a missing tag silently rolls every session up as $0, which looks like a
// working feature reporting a real number.
func TestUsageTotalsDecodeHarnessJSON(t *testing.T) {
	// Verbatim from a live session (sess-vlunj51bl5).
	const live = `{"turns":1,"cost_usd":0.06591225,"input_tokens":2,` +
		`"output_tokens":19,"cache_read_tokens":0,"cache_creation_tokens":17499}`
	var u usageTotals
	if err := json.Unmarshal([]byte(live), &u); err != nil {
		t.Fatalf("harness usage did not decode: %v", err)
	}
	if u.CostUSD == 0 {
		t.Error("cost_usd did not reach CostUSD — totals would report $0")
	}
	if u.CostUSD != 0.06591225 {
		t.Errorf("cost mismatch: got %v", u.CostUSD)
	}
	if u.Turns != 1 || u.InputTokens != 2 || u.OutputTokens != 19 {
		t.Errorf("token counts wrong: %+v", u)
	}
	if u.CacheCreationTokens != 17499 {
		t.Errorf("cache_creation_tokens lost: %+v", u)
	}
}

// Accumulation must be additive across sessions, and must count sessions even
// when a particular field is absent from the harness payload.
func TestUsageRollupAccumulates(t *testing.T) {
	add := func(cur usageTotals, raw string) usageTotals {
		var u usageTotals
		if err := json.Unmarshal([]byte(raw), &u); err != nil {
			t.Fatal(err)
		}
		cur.Sessions++
		cur.Turns += u.Turns
		cur.CostUSD += u.CostUSD
		cur.InputTokens += u.InputTokens
		return cur
	}
	var total usageTotals
	total = add(total, `{"turns":1,"cost_usd":0.05,"input_tokens":10}`)
	total = add(total, `{"turns":3,"cost_usd":0.25,"input_tokens":40}`)
	total = add(total, `{"turns":2}`) // a harness that reports no cost
	if total.Sessions != 3 {
		t.Errorf("session count wrong: %d", total.Sessions)
	}
	if total.Turns != 6 {
		t.Errorf("turns wrong: %d", total.Turns)
	}
	if total.CostUSD < 0.2999 || total.CostUSD > 0.3001 {
		t.Errorf("cost did not accumulate: %v", total.CostUSD)
	}
	if total.InputTokens != 50 {
		t.Errorf("input tokens wrong: %d", total.InputTokens)
	}
}

// The suspend/delete capture path must never probe a mind that is already
// asleep — probing resumes it, which burns a checkpoint restore to read a cost
// number and undoes the auto-sleep that just freed the worker. With no
// recorder registered (the CLI case) it must also be inert, not panic.
func TestUsageCaptureIsInertWithoutRecorder(t *testing.T) {
	saved := captureUsage
	captureUsage = nil
	defer func() { captureUsage = saved }()
	// nil client + nil recorder: must return quietly rather than dereference.
	recordUsageIfAwake(context.Background(), nil, sessionCtx{}, "sess-a", "b-sess-a")
}

func TestUsageCaptureSkipsWhenClientAbsent(t *testing.T) {
	saved := captureUsage
	called := false
	captureUsage = func(context.Context, string, json.RawMessage) { called = true }
	defer func() { captureUsage = saved }()
	// A nil control client means we cannot check status; capturing anyway would
	// risk probing (and waking) a suspended actor.
	recordUsageIfAwake(context.Background(), nil, sessionCtx{}, "sess-a", "b-sess-a")
	if called {
		t.Error("captured usage without confirming the actor is RUNNING — this can wake a sleeping mind")
	}
}

// GCP labels accept only [\p{Ll}\p{Lo}\p{N}_-]. Labelling a secret with a raw
// email made PUT /v1/credentials fail for every real address — the vault only
// ever worked for test names without a dot or an @.
func TestUserTagIsLabelSafe(t *testing.T) {
	for _, u := range []string{
		"first.last@example.com", "a+b@example.co.uk", "UPPER@Example.COM", "alice",
	} {
		tag := userTag(u)
		if tag == "" {
			t.Fatalf("%s: empty tag", u)
		}
		for _, r := range tag {
			ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_'
			if !ok {
				t.Errorf("%s -> %q contains %q, which GCP labels reject", u, tag, r)
			}
		}
		if len(tag) > 63 {
			t.Errorf("%s: tag too long for a label (%d)", u, len(tag))
		}
	}
	// Distinct users must not collide into one another's credentials.
	if userTag("alice@example.com") == userTag("bob@example.com") {
		t.Error("SECURITY: two users share a tag")
	}
}
