package indexroom

import (
	"strings"
	"testing"
)

// This file is the regression guard for releasing a session whose binding was
// REBOUND by an exclude-driven selection: x first binds a, a later select with
// an exclude list moves it onto the same instance another session y is already
// bound to. The
// release that follows must remove exactly x's current binding — it must not
// touch y (which points at the same instance), must not move the shared
// rotation (whose last real position is also that shared instance), must
// report the current registration revision with no target fields, and the
// released key's next selection must rotate from just after that shared
// position. The observations here are all through the public
// Validate/Apply/Select/ReleaseSession and Snapshot results: there is no query
// for a session binding or the rotation position, only later selections and
// release results reveal them.
//
// The concrete lifecycle locked here, with a, b, c healthy:
//
//   - x -> a, then y -> b; the cursor rests on b.
//   - x selects excluding a and c: a is its bound instance (cannot be reused
//     while excluded), so the filtered rotation from just after b wraps to the
//     smallest non-excluded candidate b; x rebinds to b and the cursor stays on
//     b. x and y then both answer b.
//   - releasing x reports changed at the current revision with no instance id,
//     address or health sequence; y still answers b and the cursor is still b.
//   - the released x, with no exclude list, rotates just after b to c and
//     establishes a fresh binding; a later x reuses c without rotating, after
//     which a plain select wraps to a — the rebind fallback and the release
//     added no extra rotation step, and the earlier exclude list carried to no
//     later request.

// buildReboundByExclusion establishes the shared starting state for this
// file's tests: a, b, c healthy; x bound to a, then y bound to b; x's
// exclude-a,c request rebounds it to b. The cursor rests on b and both
// sessions answer b.
func buildReboundByExclusion(t *testing.T, r *Registry) {
	t.Helper()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	markHealth(t, r, "svc", "c", 1, 3, true, "")

	// x first succeeds by rotating to the smallest id and binds x -> a; the
	// shared cursor rests on a.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)

	// y is a different session: its first success continues the same rotation
	// just after a to b and binds y -> b; the cursor rests on b.
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// x now selects with a and c excluded for this one request: its bound
	// instance a cannot be reused while excluded, so it falls back to the
	// filtered candidates {b} starting just after the cursor b, wrapping to b.
	// Only x's binding moves (to b); the cursor stays on b.
	keyX := "x"
	assertPick(t, selectExcluding(t, r, "svc", 1, &keyX, "a", "c"), "b", "h2:2", 2)

	// Both sessions now point at b; the cursor is still b.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)
}

// assertABCHealthy locks the tail invariant shared by these tests: the whole
// sequence changed neither the instance list nor an accepted health record —
// a, b, c stay healthy at sequences 1, 2, 3 with no reasons, revision stays 1.
func assertABCHealthy(t *testing.T, r *Registry) {
	t.Helper()
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("registration revision changed: got %d want 1", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst != (InstanceView{ID: "a", Address: "h1:1", Health: HealthHealthy, Sequence: 1}) {
		t.Fatalf("a record changed: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst != (InstanceView{ID: "b", Address: "h2:2", Health: HealthHealthy, Sequence: 2}) {
		t.Fatalf("b record changed: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "c"); inst != (InstanceView{ID: "c", Address: "h3:3", Health: HealthHealthy, Sequence: 3}) {
		t.Fatalf("c record changed: %+v", inst)
	}
}

// assertReleaseCarriesNoTarget locks that a release result reports only
// service/revision (plus changed when a binding existed): it never carries an
// instance id, address, health sequence, expected/actual revision or a reason
// on success — a release chooses no target.
func assertReleaseCarriesNoTarget(t *testing.T, out ReleaseOutcome, wantChanged bool) {
	t.Helper()
	if !out.OK || out.Changed != wantChanged || out.Service != "svc" || out.Revision != 1 {
		t.Fatalf("release (changed=%v): %+v", wantChanged, out)
	}
	if out.Kind != "" || out.Reason != "" || out.Expected != 0 || out.Actual != 0 {
		t.Fatalf("successful release must carry no failure fields: %+v", out)
	}
}

// TestRegistryReleaseAfterExcludeRebindRemovesOnlyCurrentBinding is the
// headline regression: releasing x after an exclude-driven rebind removes
// exactly x's current binding (to the shared instance b), leaves y's binding
// and the shared rotation position untouched, and lets the released key
// rejoin the rotation just after b.
func TestRegistryReleaseAfterExcludeRebindRemovesOnlyCurrentBinding(t *testing.T) {
	r := NewRegistry()
	buildReboundByExclusion(t, r)

	// Releasing x succeeds with changed:true (the rebound binding existed),
	// keeps reporting the current revision 1 and carries no target fields.
	assertReleaseCarriesNoTarget(t, releaseSession(t, r, "svc", 1, "x"), true)

	// y is unaffected even though it is bound to the very same instance b the
	// released binding pointed at — release removes one binding, not an
	// instance, and does not disturb another session pointing there.
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// The released x, carrying no exclude list this time, joins the rotation as
	// a first binding would: starting just after the last real position b it
	// chooses c — not a (the list would have been excluded had the earlier
	// exclude list persisted) and not b (which would mean the cursor moved or
	// the old binding leaked).
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "c", "h3:3", 3)

	// x's fresh success rebound it to c: a later x reuses c without rotating.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "c", "h3:3", 3)

	// A plain select continues just after the last real rotation — which was
	// x's rebind to b, since release and session reuse move nothing — and wraps
	// to a: neither the exclude fallback, the release nor the reuses added an
	// extra rotation step.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 1)

	// y still answers b throughout; x stays on c.
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "c", "h3:3", 3)

	assertABCHealthy(t, r)
}

// TestRegistryReleaseAfterExcludeRebindRepeatBeforeRebind locks that a second
// release of x after it has been released but before it has been bound again
// is still a success without changed and still carries no target fields; it
// also locks that the key-trimmed release is what removed the binding. A
// subsequent first use of x then rotates from the shared position b to c
// exactly as in the headline case.
func TestRegistryReleaseAfterExcludeRebindRepeatBeforeRebind(t *testing.T) {
	r := NewRegistry()
	buildReboundByExclusion(t, r)

	// First release removes the rebound binding and reports changed.
	assertReleaseCarriesNoTarget(t, releaseSession(t, r, "svc", 1, "  x  "), true)

	// Releasing again before x is rebound succeeds with no changed field and
	// no target fields; y and the cursor are still parked on b.
	assertReleaseCarriesNoTarget(t, releaseSession(t, r, "svc", 1, "x"), false)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// y first, then x's first post-release success: y reuse moves nothing, and
	// x rotates just after b to c and establishes a fresh binding.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "c", "h3:3", 3)
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "c", "h3:3", 3)
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 1)

	assertABCHealthy(t, r)
}

// TestRegistryReleaseAfterExcludeRebindConflictPreservesRebinding locks the
// post-rebind failure shape: a release with a legal but mismatched
// expectedRevision returns conflict carrying the current and submitted
// revisions and a reason, removes nothing, and every later request behaves
// exactly as without the failed item — x still reuses the rebound b, y and
// the rotation position are unchanged, and a later release at the correct
// revision still takes effect.
func TestRegistryReleaseAfterExcludeRebindConflictPreservesRebinding(t *testing.T) {
	r := NewRegistry()
	buildReboundByExclusion(t, r)

	// Wrong-but-in-range expectedRevision: conflict stating both revisions and
	// the current one, with a non-empty reason and no target fields.
	rel := releaseSession(t, r, "svc", 9, "x")
	if rel.OK || rel.Kind != OutcomeConflict ||
		rel.Expected != 9 || rel.Actual != 1 || rel.Revision != 1 ||
		rel.Changed || rel.Reason == "" {
		t.Fatalf("mismatched release after rebind should conflict: %+v", rel)
	}
	if !strings.Contains(rel.Reason, "revision 1, not 9") {
		t.Fatalf("conflict reason must state both revisions, got %q", rel.Reason)
	}

	// x still reuses the rebound b; y is untouched; the cursor is still b so a
	// plain select continues just after it to c.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)
	assertPick(t, selected(t, r, "svc", 1), "c", "h3:3", 3)

	// The plain rotation moved the cursor to c, but both bindings are intact:
	// the failed release changed nothing about them.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// Releasing later at the correct revision still takes effect (changed),
	// and x's next selection rejoins the rotation — now just after c — and
	// wraps to a rather than reusing the removed binding to b.
	assertReleaseCarriesNoTarget(t, releaseSession(t, r, "svc", 1, "x"), true)
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)

	assertABCHealthy(t, r)
}
