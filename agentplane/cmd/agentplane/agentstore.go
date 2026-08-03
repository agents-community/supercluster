package main

// Agent versioning (#32).
//
// Substrate's ActorTemplate.spec is immutable, so before this the only way to
// change an agent was DELETE ?cascade=true + recreate — which destroys every
// session that agent owns. Editing a system prompt killed every durable mind
// using it, there was no rollback, and the spec existed only inside the
// template's env, so deleting the template lost the definition too.
//
// An agent is now a versioned record; an ActorTemplate is the compiled artifact
// of ONE version. Re-creating an existing agent mints version N+1 against a new
// template and leaves prior templates untouched, so running sessions keep
// answering on the exact template they were minted from.

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"cloud.google.com/go/firestore"
	"google.golang.org/api/iterator"
)

const agentsCollection = "agents"

// versionSuffixRe matches the template-name suffix this file appends. An agent
// may not be named to look like a versioned template, or `foo-v2` as an agent
// would collide with version 2 of `foo` — same template name, different specs,
// second one silently unable to apply.
var versionSuffixRe = regexp.MustCompile(`-v[0-9]+$`)

// agentRecord is the agent itself: a name, an owner, and a version counter.
// The spec lives on each version, never here — this document is mutable
// (the counter moves) and specs must not be.
type agentRecord struct {
	Name          string    `firestore:"-"`
	Owner         string    `firestore:"owner"`
	LatestVersion int       `firestore:"latestVersion"`
	CreatedAt     time.Time `firestore:"createdAt"`
	UpdatedAt     time.Time `firestore:"updatedAt"`
}

// agentVersion is written once and never updated. Rollback is not a mutation:
// it is a new version whose spec is copied from an older one, so the history
// stays append-only and auditable.
type agentVersion struct {
	Version   int       `firestore:"version"`
	Spec      string    `firestore:"spec"` // the submitted YAML, verbatim
	Template  string    `firestore:"template"`
	Harness   string    `firestore:"harness"`
	CreatedBy string    `firestore:"createdBy"`
	CreatedAt time.Time `firestore:"createdAt"`
}

// templateFor is the ActorTemplate name backing a version.
//
// Version 1 keeps the bare agent name so agents that predate versioning — and
// the sessions already running on them — stay valid without a migration. Only
// version 2 onward gets a suffix.
func templateFor(agent string, version int) string {
	if version <= 1 {
		return agent
	}
	return fmt.Sprintf("%s-v%d", agent, version)
}

// nextVersion reserves the next version number for an agent, creating the
// agent record on first use. The increment happens inside a transaction, so
// two concurrent updates cannot both claim the same number and silently
// overwrite one another's template.
func (f *fsStore) nextVersion(ctx context.Context, agent, user string) (int, error) {
	if f.cl == nil {
		return 0, fmt.Errorf("agent store unavailable")
	}
	ref := f.cl.Collection(agentsCollection).Doc(agent)
	var assigned int
	err := f.cl.RunTransaction(ctx, func(ctx context.Context, tx *firestore.Transaction) error {
		now := time.Now().UTC()
		snap, err := tx.Get(ref)
		if err != nil || !snap.Exists() {
			// First version. An agent that already exists as a bare template
			// (created before versioning) is adopted here as version 1 rather
			// than being renumbered — its running sessions still point at it.
			assigned = 1
			return tx.Set(ref, agentRecord{
				Owner: user, LatestVersion: 1, CreatedAt: now, UpdatedAt: now,
			})
		}
		var rec agentRecord
		if err := snap.DataTo(&rec); err != nil {
			return err
		}
		assigned = rec.LatestVersion + 1
		return tx.Set(ref, agentRecord{
			Owner: rec.Owner, LatestVersion: assigned,
			CreatedAt: rec.CreatedAt, UpdatedAt: now,
		}, firestore.Merge(
			firestore.FieldPath{"latestVersion"}, firestore.FieldPath{"updatedAt"}))
	})
	if err != nil {
		return 0, fmt.Errorf("reserve version for %s: %w", agent, err)
	}
	return assigned, nil
}

// recordVersion writes the immutable version document. Create-only: a version
// that already exists is never rewritten, so history cannot be edited.
func (f *fsStore) recordVersion(ctx context.Context, agent string, v agentVersion) error {
	if f.cl == nil {
		return fmt.Errorf("agent store unavailable")
	}
	ref := f.cl.Collection(agentsCollection).Doc(agent).
		Collection("versions").Doc(fmt.Sprint(v.Version))
	if _, err := ref.Create(ctx, v); err != nil {
		return fmt.Errorf("record %s v%d: %w", agent, v.Version, err)
	}
	return nil
}

// latestTemplate resolves an agent to the template new sessions should use.
// Unknown agents fall back to the bare name: a cluster whose agents predate
// this store keeps working, and session creation never depends on Firestore
// being reachable.
func (f *fsStore) latestTemplate(ctx context.Context, agent string) (string, int) {
	if f.cl == nil {
		return agent, 0
	}
	snap, err := f.cl.Collection(agentsCollection).Doc(agent).Get(ctx)
	if err != nil || !snap.Exists() {
		return agent, 0
	}
	var rec agentRecord
	if snap.DataTo(&rec) != nil || rec.LatestVersion == 0 {
		return agent, 0
	}
	return templateFor(agent, rec.LatestVersion), rec.LatestVersion
}

// templateForVersion resolves a specific pinned version, for reproducing a run
// against the exact agent definition it was minted from.
func (f *fsStore) templateForVersion(ctx context.Context, agent string, version int) (string, error) {
	if f.cl == nil {
		return "", fmt.Errorf("agent store unavailable")
	}
	snap, err := f.cl.Collection(agentsCollection).Doc(agent).
		Collection("versions").Doc(fmt.Sprint(version)).Get(ctx)
	if err != nil || !snap.Exists() {
		return "", fmt.Errorf("agent %s has no version %d", agent, version)
	}
	var v agentVersion
	if err := snap.DataTo(&v); err != nil {
		return "", err
	}
	return v.Template, nil
}

// versions lists an agent's history, newest first.
func (f *fsStore) versions(ctx context.Context, agent string) ([]agentVersion, error) {
	if f.cl == nil {
		return nil, fmt.Errorf("agent store unavailable")
	}
	out := []agentVersion{}
	it := f.cl.Collection(agentsCollection).Doc(agent).Collection("versions").
		OrderBy("version", firestore.Desc).Documents(ctx)
	defer it.Stop()
	for {
		snap, err := it.Next()
		if err == iterator.Done {
			break
		}
		if err != nil {
			return out, fmt.Errorf("list versions of %s: %w", agent, err)
		}
		var v agentVersion
		if snap.DataTo(&v) == nil {
			out = append(out, v)
		}
	}
	return out, nil
}

// forgetAgent drops the agent record and its history. Called only when the
// agent itself is deleted — never on a version bump.
func (f *fsStore) forgetAgent(ctx context.Context, agent string) {
	if f.cl == nil {
		return
	}
	doc := f.cl.Collection(agentsCollection).Doc(agent)
	it := doc.Collection("versions").Documents(ctx)
	defer it.Stop()
	for {
		snap, err := it.Next()
		if err != nil {
			break
		}
		_, _ = snap.Ref.Delete(ctx)
	}
	_, _ = doc.Delete(ctx)
}
