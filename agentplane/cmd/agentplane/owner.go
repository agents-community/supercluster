package main

// Session ownership (threat-model F1). Substrate Actors carry no label map, so
// the owner of a session lives in a small control-plane registry: a ConfigMap
// holding {sid: user}. Written when a session is created, dropped when it is
// deleted, and consulted on EVERY session-scoped request.
//
// Deny-by-default: when auth is on and a session has no recorded owner, only
// the reconciling path (delete) may touch it — a request from a user who is
// not the owner gets 404, never 403, so session ids can't be enumerated by
// probing for the difference.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

const ownerConfigMap = "agentplane-session-owners"

type ownerStore struct {
	mu     sync.RWMutex
	ns     string
	byID   map[string]string // sid → user
	loaded bool
}

func newOwnerStore(ns string) *ownerStore {
	os := &ownerStore{ns: ns, byID: map[string]string{}}
	os.reload(context.Background())
	return os
}

// reload pulls the registry from the ConfigMap. Best-effort: a missing map is
// an empty registry (first run), not an error.
func (o *ownerStore) reload(ctx context.Context) {
	out, err := runKubectl(ctx, "get", "configmap", ownerConfigMap, "-n", o.ns,
		"-o", `jsonpath={.data.owners\.json}`)
	next := map[string]string{}
	if err == nil && len(out) > 0 {
		_ = json.Unmarshal(out, &next)
	}
	o.mu.Lock()
	o.byID = next
	o.loaded = true
	o.mu.Unlock()
}

func (o *ownerStore) snapshot() map[string]string {
	o.mu.RLock()
	defer o.mu.RUnlock()
	cp := make(map[string]string, len(o.byID))
	for k, v := range o.byID {
		cp[k] = v
	}
	return cp
}

// persist writes the whole registry back (create-or-replace, same approach as
// the token Secret).
func (o *ownerStore) persist(ctx context.Context, m map[string]string) error {
	blob, _ := json.Marshal(m)
	manifest, err := runKubectl(ctx, "create", "configmap", ownerConfigMap, "-n", o.ns,
		"--from-literal=owners.json="+string(blob), "--dry-run=client", "-o", "yaml")
	if err != nil {
		return fmt.Errorf("render owners: %v: %s", err, strings.TrimSpace(string(manifest)))
	}
	if out, err := runKubectlStdin(ctx, string(manifest), "apply", "-f", "-"); err != nil {
		return fmt.Errorf("apply owners: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// claim records user as the owner of sid.
func (o *ownerStore) claim(ctx context.Context, sid, user string) error {
	o.reload(ctx) // read-modify-write against the current state
	m := o.snapshot()
	m[sid] = user
	if err := o.persist(ctx, m); err != nil {
		return err
	}
	o.mu.Lock()
	o.byID = m
	o.mu.Unlock()
	return nil
}

// release forgets sid (session deleted).
func (o *ownerStore) release(ctx context.Context, sid string) {
	o.reload(ctx)
	m := o.snapshot()
	if _, ok := m[sid]; !ok {
		return
	}
	delete(m, sid)
	if err := o.persist(ctx, m); err != nil {
		return // best-effort: a stale entry denies access, it never grants it
	}
	o.mu.Lock()
	o.byID = m
	o.mu.Unlock()
}

// owner returns the recorded owner of sid, if any.
func (o *ownerStore) owner(sid string) (string, bool) {
	o.mu.RLock()
	u, ok := o.byID[sid]
	o.mu.RUnlock()
	return u, ok
}

// mine reports whether user may act on sid. Unowned sessions (created before
// this registry existed, or whose claim failed) are visible only to the
// operator token so a migration can't lock anyone out of cleanup.
func (o *ownerStore) mine(sid, user string) bool {
	rec, ok := o.owner(sid)
	if !ok {
		o.reload(context.Background()) // second chance: another replica may have claimed it
		rec, ok = o.owner(sid)
	}
	if !ok {
		return user == operatorUser
	}
	return rec == user
}

// operatorUser is the label of the token used for platform administration
// (`agentplane token issue -user operator`). It may act on unowned sessions.
const operatorUser = "operator"
