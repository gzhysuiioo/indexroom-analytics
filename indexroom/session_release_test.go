package indexroom

import (
	"strings"
	"testing"
)

// This file is the regression guard for release_session, the one operation
// that actively tears a session binding down. Everything is observed through
// the public release and selection results:
//
//   - releasing a bound key succeeds with changed and removes only that key's
//     binding in that service; releasing an unbound key also succeeds but
//     reports no change;
//   - a release never selects a target, so its result carries no instance id,
//     address or health sequence, and it never moves the rotation cursor, the
//     instance list, the registration revision or any health record;
//   - the released key's next selection follows the first-use rules again,
//     rotating from just after the last ACTUAL rotation — not from wherever
//     the released binding pointed;
//   - the bound instance's state is irrelevant: deleted, never observed
//     healthy, unhealthy, or an empty instance list all still allow the
//     release;
//   - other sessions bound to the same instance keep their bindings, and the
//     same key under another service is untouched;
//   - a revision mismatch is a conflict and an unknown service at
//     expectedRevision 0 is not_found; failures remove nothing and later
//     requests still run.

// releaseSession validates and runs one release for the given key, returning
// the outcome (success or failure) so failure cases can be asserted directly.
func releaseSession(t *testing.T, r *Registry, service string, revision int64, key string) ReleaseOutcome {
	t.Helper()
	rel, err := r.ValidateReleaseSession(service, revision, key)
	if err != nil {
		t.Fatalf("validate release %s key %q: %v", service, key, err)
	}
	return r.ReleaseSession(rel)
}

// assertRelease locks one successful release: ok, the expected changed flag,
// the current revision, and never an instance id, address or sequence.
func assertRelease(t *testing.T, out ReleaseOutcome, changed bool, revision int) {
	t.Helper()
	if !out.OK {
		t.Fatalf("release should succeed, got %+v", out)
	}
	if out.Changed != changed {
		t.Fatalf("release changed = %v, want %v (%+v)", out.Changed, changed, out)
	}
	if out.Revision != revision {
		t.Fatalf("release revision = %d, want %d (%+v)", out.Revision, revision, out)
	}
}

// TestRegistryReleaseSessionRejoinsRotation is the lifecycle pinned by the
// feature request: with healthy i1 < i2 < i3, a session binds i1, a plain
// select rotates to i2, and after the release the session's next select is a
// fresh rotation continuing just after i2 — landing on i3, not back on i1.
func TestRegistryReleaseSessionRejoinsRotation(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "i1", Address: "h1:1"}, {ID: "i2", Address: "h2:2"}, {ID: "i3", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "i1", 1, 1, true, "")
	markHealth(t, r, "svc", "i2", 1, 2, true, "")
	markHealth(t, r, "svc", "i3", 1, 3, true, "")

	// The key's first success binds s -> i1; the shared cursor rests on i1.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "i1", "h1:1", 1)
	// A plain select advances the last actual rotation to i2.
	assertPick(t, selected(t, r, "svc", 1), "i2", "h2:2", 2)
	// The binding still answers the key's requests without moving the cursor.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "i1", "h1:1", 1)

	// Releasing the bound key succeeds with changed and chooses no target.
	assertRelease(t, releaseSession(t, r, "svc", 1, "s"), true, 1)

	// The released key's next select is a first-use rotation again: it
	// continues just after the last actual rotation (i2) and lands on i3,
	// establishing a fresh binding — it is NOT dragged back to the released i1.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "i3", "h3:3", 3)
	// The new binding sticks.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "i3", "h3:3", 3)

	// Releasing the same key a second time finds no binding only after the
	// first release; here the re-established binding is released instead.
	assertRelease(t, releaseSession(t, r, "svc", 1, "s"), true, 1)
	// A key that was never bound releases successfully without a change.
	assertRelease(t, releaseSession(t, r, "svc", 1, "never-bound"), false, 1)

	// The release moved nothing: the rotation continues just after i3 (the
	// last actual rotation, made by the re-established binding's first select).
	assertPick(t, selected(t, r, "svc", 1), "i1", "h1:1", 1)

	// Neither release changed the revision or any health record.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("release changed the registration revision: %d", rev)
	}
	for id, seq := range map[string]int64{"i1": 1, "i2": 2, "i3": 3} {
		if inst := instanceHealth(t, r, "svc", id); inst.Health != HealthHealthy || inst.Sequence != seq {
			t.Fatalf("%s record changed by release: %+v", id, inst)
		}
	}
}

// TestRegistryReleaseSessionIgnoresBoundInstanceState locks that the release
// looks at the binding alone: the target being deleted, never observed
// healthy, unhealthy, or the whole instance list being empty never blocks it.
func TestRegistryReleaseSessionIgnoresBoundInstanceState(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")

	// Bind four keys: u and v to a, w to b; x is bound later to a deleted id.
	assertPick(t, selectSession(t, r, "svc", 1, "u"), "a", "h1:1", 1)
	assertPick(t, selectSession(t, r, "svc", 1, "v"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "w"), "a", "h1:1", 1)

	// The bound instance turns unhealthy: releasing its keys still works.
	markHealth(t, r, "svc", "a", 1, 3, false, "down")
	assertRelease(t, releaseSession(t, r, "svc", 1, "u"), true, 1)
	// The other session bound to the same (unhealthy) instance keeps its
	// binding: it is released separately, not by u's release.
	assertRelease(t, releaseSession(t, r, "svc", 1, "w"), true, 1)

	// The bound instance is deleted by a replacement: the binding's target is
	// gone, yet the release still succeeds with changed.
	registerService(t, r, "svc", 1, []Instance{{ID: "b", Address: "h2:2"}})
	assertRelease(t, releaseSession(t, r, "svc", 2, "v"), true, 2)

	// An empty instance list does not block a release either. First rebind x
	// while b is healthy, then clear the list.
	assertPick(t, selectSession(t, r, "svc", 2, "x"), "b", "h2:2", 2)
	registerService(t, r, "svc", 2, nil)
	assertRelease(t, releaseSession(t, r, "svc", 3, "x"), true, 3)

	// A binding whose target was never observed healthy (unknown) also
	// releases cleanly. Rebuild with one instance, bind y through its first
	// rotation while healthy, then replace the list so the surviving id starts
	// unknown at the new address.
	registerService(t, r, "svc", 3, []Instance{{ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "b", 4, 1, true, "")
	assertPick(t, selectSession(t, r, "svc", 4, "y"), "b", "h2:2", 1)
	registerService(t, r, "svc", 4, []Instance{{ID: "b", Address: "h9:9"}})
	assertRelease(t, releaseSession(t, r, "svc", 5, "y"), true, 5)
}

// TestRegistryReleaseSessionIsolatesKeysAndServices locks the scoping: a
// release touches only the named key in the named service — another session
// bound to the same instance keeps its binding, and the same key under a
// different service is unaffected.
func TestRegistryReleaseSessionIsolatesKeysAndServices(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}})
	registerService(t, r, "other", 0, []Instance{{ID: "a", Address: "h8:8"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "other", "a", 1, 7, true, "")

	// Two sessions in svc converge on the sole healthy instance a; the same
	// key s also binds in the other service.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 1)
	assertPick(t, selectSession(t, r, "svc", 1, "t"), "a", "h1:1", 1)
	assertPick(t, selectSession(t, r, "other", 1, "s"), "a", "h8:8", 7)

	// Releasing s in svc leaves t's binding to the same instance and s's
	// binding in the other service intact.
	assertRelease(t, releaseSession(t, r, "svc", 1, "s"), true, 1)
	assertPick(t, selectSession(t, r, "svc", 1, "t"), "a", "h1:1", 1)
	assertPick(t, selectSession(t, r, "other", 1, "s"), "a", "h8:8", 7)

	// The released key rotates again on its next select in svc (single healthy
	// instance, so it rebinds to a), proving the binding — not the instance —
	// was what the release removed.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 1)
}

// TestRegistryReleaseSessionFailuresPreserveState locks the failure paths:
// content validity precedes the revision comparison, a mismatch is a conflict
// carrying both revisions, an unknown service at expectedRevision 0 is
// not_found, and every failure keeps all bindings and the rotation position.
func TestRegistryReleaseSessionFailuresPreserveState(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")

	// Bind s -> a; the cursor rests on a.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 1)

	// Blank keys are invalid and the reason names the sessionKey problem; the
	// invalid field wins over a simultaneously wrong revision.
	for _, key := range []string{"", "   "} {
		if _, err := r.ValidateReleaseSession("svc", 99, key); err == nil ||
			!strings.Contains(err.Error(), "sessionKey") {
			t.Fatalf("blank key %q should be invalid naming sessionKey, got %v", key, err)
		}
	}
	// The service name and expectedRevision keep their usual rules.
	if _, err := r.ValidateReleaseSession("  ", 1, "s"); err == nil {
		t.Fatalf("blank service name should be invalid")
	}
	if _, err := r.ValidateReleaseSession("svc", -1, "s"); err == nil ||
		!strings.Contains(err.Error(), "expectedRevision") {
		t.Fatalf("negative revision should be invalid naming expectedRevision, got %v", err)
	}

	// A revision mismatch is a conflict reporting both revisions.
	conflict := releaseSession(t, r, "svc", 99, "s")
	if conflict.OK || conflict.Kind != OutcomeConflict ||
		conflict.Expected != 99 || conflict.Actual != 1 || conflict.Revision != 1 {
		t.Fatalf("revision mismatch should be conflict with both revisions: %+v", conflict)
	}

	// An unknown service at expectedRevision 0 is not_found; at any other
	// revision it is a conflict against actual revision 0.
	missing := releaseSession(t, r, "ghost", 0, "s")
	if missing.OK || missing.Kind != OutcomeNotFound || missing.Revision != 0 {
		t.Fatalf("unknown service at revision 0 should be not_found: %+v", missing)
	}
	ghostConflict := releaseSession(t, r, "ghost", 3, "s")
	if ghostConflict.OK || ghostConflict.Kind != OutcomeConflict ||
		ghostConflict.Expected != 3 || ghostConflict.Actual != 0 {
		t.Fatalf("unknown service at revision 3 should be conflict against 0: %+v", ghostConflict)
	}

	// The failures removed nothing: s still reuses its binding to a, and the
	// rotation still continues just after a.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 1)
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 2)
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("failed releases changed the registration revision: %d", rev)
	}
}
