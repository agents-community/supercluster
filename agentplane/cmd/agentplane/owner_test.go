package main

import "testing"

// The F1 fix hinges entirely on `mine`: it is the single predicate standing
// between one tester and everyone else's sessions.
func TestOwnerMine(t *testing.T) {
	o := &ownerStore{ns: "test", byID: map[string]string{
		"sess-aaa": "alice@example.com",
		"sess-bbb": "bob@example.com",
	}, loaded: true}

	if !o.mine("sess-aaa", "alice@example.com") {
		t.Error("owner denied access to their own session")
	}
	if o.mine("sess-aaa", "bob@example.com") {
		t.Error("SECURITY: another user was granted access to a session they do not own")
	}
	if o.mine("sess-bbb", "alice@example.com") {
		t.Error("SECURITY: cross-session access granted")
	}
	// Empty/unknown user must never match, even against an empty owner record.
	if o.mine("sess-aaa", "") {
		t.Error("SECURITY: empty user matched an owned session")
	}
}

func TestOwnerUnownedSessionsAreOperatorOnly(t *testing.T) {
	o := &ownerStore{ns: "test", byID: map[string]string{}, loaded: true}

	// Pre-registry sessions have no owner: only the operator may reconcile them,
	// so a migration can't strand them — but no ordinary user inherits them.
	if !o.mine("sess-legacy", operatorUser) {
		t.Error("operator denied access to an unowned session")
	}
	if o.mine("sess-legacy", "alice@example.com") {
		t.Error("SECURITY: an ordinary user claimed an unowned session")
	}
}

func TestOwnerClaimReleaseInMemory(t *testing.T) {
	o := &ownerStore{ns: "test", byID: map[string]string{}, loaded: true}
	o.byID["sess-ccc"] = "carol@example.com" // as claim() would, minus the API call

	if got, ok := o.owner("sess-ccc"); !ok || got != "carol@example.com" {
		t.Fatalf("owner() = %q,%v want carol,true", got, ok)
	}
	delete(o.byID, "sess-ccc") // as release() would
	if _, ok := o.owner("sess-ccc"); ok {
		t.Error("owner survived release")
	}
}

// A stale entry must deny, never grant: release() is best-effort, so the
// failure mode has to be safe.
func TestOwnerStaleEntryDenies(t *testing.T) {
	o := &ownerStore{ns: "test", byID: map[string]string{"sess-gone": "dave@example.com"}, loaded: true}
	if o.mine("sess-gone", "eve@example.com") {
		t.Error("SECURITY: stale ownership record granted access to another user")
	}
}
