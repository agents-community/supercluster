package main

// Session metadata store (#36).
//
// Every field `GET /v1/sessions/{id}` returns is derived live today, and two of
// them — `last_event_at` and `usage` — come from probing the brain. Probing
// WAKES a sleeping mind, so for any suspended session those fields are simply
// unavailable, and deleting a session destroys its usage record entirely. That
// is why /usage cannot total a user's spend.
//
// This store persists what the platform observes as it happens (ownership,
// activity, cost) so those questions can be answered without resuming anything.
//
// It deliberately does NOT store status/busy/queued. Substrate's registry is
// the authority on liveness; a mirrored copy drifts the moment a worker dies,
// and confidently-served stale status is worse than none. Firestore holds
// history; Substrate holds now.

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// sessionMeta is the durable half of a session: what Substrate cannot know.
type sessionMeta struct {
	ID           string          `firestore:"-"`
	Owner        string          `firestore:"owner"`
	Agent        string          `firestore:"agent"`
	Version      int             `firestore:"version,omitempty"` // agent version pin (#32)
	Title        string          `firestore:"title,omitempty"`
	CreatedAt    time.Time       `firestore:"createdAt"`
	LastActiveAt time.Time       `firestore:"lastActiveAt"`
	Usage        json.RawMessage `firestore:"-"`     // relayed verbatim; stored as a string
	UsageJSON    string          `firestore:"usage"` // harness totals, last observed
}

// sessionStore is the ownership + metadata registry. The ConfigMap
// implementation remains for local/dev clusters with no GCP project.
type sessionStore interface {
	claim(ctx context.Context, sid, user, agent string, version int) error
	release(ctx context.Context, sid string)
	mine(sid, user string) bool
	owner(sid string) (string, bool)
	// touch records activity and (when non-nil) the latest usage totals. Called
	// on the paths we already handle; never blocks the caller's response.
	touch(ctx context.Context, sid string, usage json.RawMessage)
	// get returns stored metadata, readable while the mind sleeps.
	get(ctx context.Context, sid string) (sessionMeta, bool)
	// ownedBy lists a user's sessions, including ones no longer in Substrate.
	ownedBy(ctx context.Context, user string) (map[string]sessionMeta, error)
}

// ---------- Firestore ----------

type fsStore struct {
	cl *firestore.Client

	// Ownership is consulted on EVERY session-scoped request, so it is cached.
	// A miss falls through to Firestore (another replica may have claimed it);
	// the cache only ever answers "known owner", never "no owner", so a cold
	// replica cannot wrongly deny access.
	mu    sync.RWMutex
	cache map[string]string // sid → owner
}

const (
	sessionsCollection = "sessions"
	// Lifetime spend per user, accumulated when a session is deleted so cost
	// history outlives the sessions that produced it.
	usageCollection = "usage"
)

func newFirestoreStore(ctx context.Context, project string) (*fsStore, error) {
	cl, err := firestore.NewClient(ctx, project)
	if err != nil {
		return nil, fmt.Errorf("firestore client: %w", err)
	}
	return &fsStore{cl: cl, cache: map[string]string{}}, nil
}

func (f *fsStore) doc(sid string) *firestore.DocumentRef {
	return f.cl.Collection(sessionsCollection).Doc(sid)
}

func (f *fsStore) claim(ctx context.Context, sid, user, agent string, version int) error {
	if f.cl == nil {
		return fmt.Errorf("session store unavailable")
	}
	now := time.Now().UTC()
	// Create-only: a claim must never overwrite an existing session's owner.
	// Two concurrent creates of the same id (which naming makes practically
	// impossible) fail the second rather than silently reassigning ownership —
	// the lost-update bug the ConfigMap store had by construction.
	_, err := f.doc(sid).Create(ctx, sessionMeta{
		Owner: user, Agent: agent, Version: version,
		CreatedAt: now, LastActiveAt: now,
	})
	if status.Code(err) == codes.AlreadyExists {
		return fmt.Errorf("session %s already claimed", sid)
	}
	if err != nil {
		return fmt.Errorf("claim %s: %w", sid, err)
	}
	f.mu.Lock()
	f.cache[sid] = user
	f.mu.Unlock()
	return nil
}

// release forgets the session, folding its usage into the owner's rollup first.
//
// The session document is deleted rather than tombstoned — a record that
// outlives its session would keep answering ownership questions for a dead id.
// But cost must not die with it: before this, deleting a session destroyed its
// usage, so a spend total could only ever cover sessions that happened to still
// exist. The rollup is what makes /usage answerable across a user's history.
func (f *fsStore) release(ctx context.Context, sid string) {
	if f.cl != nil {
		if m, found := f.get(ctx, sid); found {
			f.rollUp(ctx, m)
		}
		_, _ = f.doc(sid).Delete(ctx)
	}
	f.mu.Lock()
	delete(f.cache, sid)
	f.mu.Unlock()
}

// usageTotals is the subset of harness usage worth accumulating across
// sessions. Unknown fields are ignored rather than summed blindly: harnesses
// report different shapes, and adding up something like a context-window size
// would produce a confident, meaningless number.
// Both tag sets are required: the harness reports snake_case JSON, and Go's
// json decoder does NOT fold `cost_usd` onto `CostUSD` (its case-insensitive
// match ignores case, not underscores). Without the json tags every cost would
// silently accumulate as zero.
type usageTotals struct {
	Sessions            int     `firestore:"sessions" json:"sessions"`
	Turns               int     `firestore:"turns" json:"turns"`
	CostUSD             float64 `firestore:"cost_usd" json:"cost_usd"`
	InputTokens         int     `firestore:"input_tokens" json:"input_tokens"`
	OutputTokens        int     `firestore:"output_tokens" json:"output_tokens"`
	CacheReadTokens     int     `firestore:"cache_read_tokens" json:"cache_read_tokens"`
	CacheCreationTokens int     `firestore:"cache_creation_tokens" json:"cache_creation_tokens"`
}

// rollUp adds one session's final usage to its owner's lifetime totals, in a
// transaction so concurrent deletes cannot lose an increment.
func (f *fsStore) rollUp(ctx context.Context, m sessionMeta) {
	if len(m.Usage) == 0 || m.Owner == "" {
		return
	}
	var u usageTotals
	if json.Unmarshal(m.Usage, &u) != nil {
		return // unrecognized shape: better no number than a wrong one
	}
	ref := f.cl.Collection(usageCollection).Doc(m.Owner)
	_ = f.cl.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		var cur usageTotals
		if snap, err := tx.Get(ref); err == nil && snap.Exists() {
			_ = snap.DataTo(&cur)
		}
		cur.Sessions++
		cur.Turns += u.Turns
		cur.CostUSD += u.CostUSD
		cur.InputTokens += u.InputTokens
		cur.OutputTokens += u.OutputTokens
		cur.CacheReadTokens += u.CacheReadTokens
		cur.CacheCreationTokens += u.CacheCreationTokens
		return tx.Set(ref, cur)
	})
}

// lifetimeUsage returns a user's accumulated totals from deleted sessions.
// Live sessions are added by the caller, which already has them.
func (f *fsStore) lifetimeUsage(ctx context.Context, user string) (usageTotals, bool) {
	if f.cl == nil {
		return usageTotals{}, false
	}
	snap, err := f.cl.Collection(usageCollection).Doc(user).Get(ctx)
	if err != nil || !snap.Exists() {
		return usageTotals{}, false
	}
	var u usageTotals
	if snap.DataTo(&u) != nil {
		return usageTotals{}, false
	}
	return u, true
}

func (f *fsStore) owner(sid string) (string, bool) {
	f.mu.RLock()
	u, ok := f.cache[sid]
	f.mu.RUnlock()
	if ok {
		return u, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m, found := f.get(ctx, sid)
	if !found {
		return "", false
	}
	f.mu.Lock()
	f.cache[sid] = m.Owner
	f.mu.Unlock()
	return m.Owner, true
}

// mine reports whether user may act on sid. Unowned sessions stay
// operator-only, so a store outage or an unmigrated record can never widen
// access — it only ever denies.
func (f *fsStore) mine(sid, user string) bool {
	rec, ok := f.owner(sid)
	if !ok {
		return user == operatorUser
	}
	return rec == user
}

func (f *fsStore) get(ctx context.Context, sid string) (sessionMeta, bool) {
	if f.cl == nil { // no backing store: report "unknown", never panic
		return sessionMeta{}, false
	}
	snap, err := f.doc(sid).Get(ctx)
	if err != nil || !snap.Exists() {
		return sessionMeta{}, false
	}
	var m sessionMeta
	if err := snap.DataTo(&m); err != nil {
		return sessionMeta{}, false
	}
	m.ID = sid
	if m.UsageJSON != "" {
		m.Usage = json.RawMessage(m.UsageJSON)
	}
	return m, true
}

// touch stamps activity and stores the latest usage totals. Best-effort and
// non-fatal: losing a timestamp must never fail a user's turn.
func (f *fsStore) touch(ctx context.Context, sid string, usage json.RawMessage) {
	if f.cl == nil {
		return
	}
	updates := []firestore.Update{{Path: "lastActiveAt", Value: time.Now().UTC()}}
	if len(usage) > 0 {
		updates = append(updates, firestore.Update{Path: "usage", Value: string(usage)})
	}
	_, _ = f.doc(sid).Update(ctx, updates)
}

func (f *fsStore) ownedBy(ctx context.Context, user string) (map[string]sessionMeta, error) {
	out := map[string]sessionMeta{}
	if f.cl == nil {
		return out, fmt.Errorf("session store unavailable")
	}
	it := f.cl.Collection(sessionsCollection).Where("owner", "==", user).Documents(ctx)
	defer it.Stop()
	for {
		snap, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return out, fmt.Errorf("list sessions for %s: %w", user, err)
		}
		var m sessionMeta
		if snap.DataTo(&m) != nil {
			continue
		}
		m.ID = snap.Ref.ID
		if m.UsageJSON != "" {
			m.Usage = json.RawMessage(m.UsageJSON)
		}
		out[m.ID] = m
	}
	return out, nil
}

// ---------- ConfigMap (fallback) ----------

// cmStore adapts the original ConfigMap registry to the sessionStore
// interface. It carries the old limits — a 1 MiB ceiling and read-modify-write
// races — and stores no metadata, so it is a dev/no-GCP fallback only.
type cmStore struct{ *ownerStore }

func (c cmStore) claim(ctx context.Context, sid, user, _ string, _ int) error {
	return c.ownerStore.claim(ctx, sid, user)
}
func (c cmStore) release(ctx context.Context, sid string)        { c.ownerStore.release(ctx, sid) }
func (c cmStore) mine(sid, user string) bool                     { return c.ownerStore.mine(sid, user) }
func (c cmStore) owner(sid string) (string, bool)                { return c.ownerStore.owner(sid) }
func (c cmStore) touch(context.Context, string, json.RawMessage) {}
func (c cmStore) get(context.Context, string) (sessionMeta, bool) {
	return sessionMeta{}, false
}
func (c cmStore) ownedBy(_ context.Context, user string) (map[string]sessionMeta, error) {
	out := map[string]sessionMeta{}
	for sid, u := range c.ownerStore.snapshot() {
		if u == user {
			out[sid] = sessionMeta{ID: sid, Owner: u}
		}
	}
	return out, nil
}

// newSessionStore picks the durable store when a GCP project is configured and
// falls back to the ConfigMap otherwise, so a local cluster still runs.
func newSessionStore(ctx context.Context, project, ns string, log logger) sessionStore {
	if project != "" {
		if f, err := newFirestoreStore(ctx, project); err == nil {
			log.Info("session store: firestore", "project", project)
			return f
		} else {
			log.Warn("session store: firestore unavailable, falling back to ConfigMap "+
				"(no usage history, 1MiB cap, lost-update races — see #36)", "err", err)
		}
	}
	return cmStore{newOwnerStore(ns)}
}

// logger is the slice of *slog.Logger this file needs, kept small so tests can
// substitute a no-op.
type logger interface {
	Info(msg string, args ...any)
	Warn(msg string, args ...any)
}
