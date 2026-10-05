package indexroom

import "testing"

// This file is the regression guard for a pinned session whose service's
// instance list is cleared (instances: []) and then populated again. Clearing
// removes the instances together with their health records, but the service
// itself, the per-service rotation cursor and the session bindings all
// survive: nothing actively tears a session down, and whether its target is
// still reusable is judged purely from the instance state at the next
// selection.
//
// The lifecycle locked here, observed entirely through public results:
//
//   - the key's first success binds s -> a and a later plain select parks the
//     shared rotation cursor on b;
//   - an empty-list replacement bumps the revision but keeps the service, so
//     the session's next request is no_healthy at the current revision with a
//     readable reason — never not_found — and fabricates neither the pre-clear
//     instance id, address nor health sequence; the failure rewrites the
//     binding and the cursor neither;
//   - re-adding the same ids starts every instance at unknown/0/no reason,
//     including instances that kept their old address; until a fresh
//     observation is accepted at the new revision the session still fails;
//   - after the bound instance and one other instance accept new positive
//     sequences (which may be far below the pre-clear sequences), the session
//     reuses its binding and returns the bound instance's CURRENT address and
//     sequence, while a plain rotation from the preserved cursor lands on the
//     other instance rather than being dragged back to the bound one.

// TestRegistrySessionBindingAndCursorSurviveInstanceClear walks one pinned
// session through a full clear/re-add cycle and locks every guarantee above.
func TestRegistrySessionBindingAndCursorSurviveInstanceClear(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 11, true, "")
	markHealth(t, r, "svc", "b", 1, 12, true, "")
	markHealth(t, r, "svc", "c", 1, 13, true, "")

	// The key's first success rotates to the smallest id and binds s -> a; the
	// shared cursor rests on a.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 11)

	// A plain selection without a session key advances the shared rotation to
	// another instance; the cursor now rests on b, the last real rotation.
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 12)

	// Clear the instance list at the current revision: content changes, so the
	// revision bumps to 2 and every instance with its health record is removed.
	registerService(t, r, "svc", 1, nil)
	if rev := r.RevisionOf("svc"); rev != 2 {
		t.Fatalf("revision after clear: %d", rev)
	}
	views := r.Snapshot()
	if len(views) != 1 || views[0].Service != "svc" || views[0].Revision != 2 || len(views[0].Instances) != 0 {
		t.Fatalf("clear must keep the service with an empty instance list: %+v", views)
	}

	// The session is not actively cleared. Its next request finds the service
	// alive at revision 2 with no instance at all, so the answer is no_healthy
	// (never not_found), carrying the current revision and a non-empty reason.
	// It must not leak the pre-clear binding's id, address or sequence.
	failOnClear := selectSession(t, r, "svc", 2, "s")
	if failOnClear.OK {
		t.Fatalf("selection on the cleared list should fail: %+v", failOnClear)
	}
	if failOnClear.Kind != OutcomeNoHealthy {
		t.Fatalf("cleared-but-existing service should be no_healthy, got %q (%+v)", failOnClear.Kind, failOnClear)
	}
	if failOnClear.Kind == OutcomeNotFound {
		t.Fatalf("the service survived the clear; not_found would mean it was deleted: %+v", failOnClear)
	}
	if failOnClear.Revision != 2 || failOnClear.Reason == "" {
		t.Fatalf("no_healthy must state the current revision and a reason: %+v", failOnClear)
	}
	if failOnClear.InstanceID != "" || failOnClear.Address != "" || failOnClear.Sequence != 0 {
		t.Fatalf("failure must carry no pre-clear instance id, address or sequence: %+v", failOnClear)
	}

	// Re-add the original ids at the post-clear revision: the bound instance a
	// uses a new address; b and c keep their old addresses. The revision bumps
	// to 3 and EVERY instance starts at unknown/0/no reason — the identical
	// ids and addresses restore none of the pre-clear health records.
	registerService(t, r, "svc", 2, []Instance{
		{ID: "a", Address: "h9:9"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	if rev := r.RevisionOf("svc"); rev != 3 {
		t.Fatalf("revision after re-add: %d", rev)
	}
	for _, want := range []InstanceView{
		{ID: "a", Address: "h9:9", Health: HealthUnknown, Sequence: 0},
		{ID: "b", Address: "h2:2", Health: HealthUnknown, Sequence: 0},
		{ID: "c", Address: "h3:3", Health: HealthUnknown, Sequence: 0},
	} {
		if got := instanceHealth(t, r, "svc", want.ID); got != want {
			t.Fatalf("re-added instance %s must start at unknown/0/no reason even at its old address: got %+v want %+v", want.ID, got, want)
		}
	}

	// Before any fresh observation is accepted, the session still fails — again
	// no_healthy at the now-current revision 3, with no fabricated target and
	// no rewrite of the binding or cursor.
	failAfterReadd := selectSession(t, r, "svc", 3, "s")
	if failAfterReadd.OK || failAfterReadd.Kind != OutcomeNoHealthy ||
		failAfterReadd.Revision != 3 || failAfterReadd.Reason == "" ||
		failAfterReadd.InstanceID != "" || failAfterReadd.Address != "" || failAfterReadd.Sequence != 0 {
		t.Fatalf("unknown re-added instances must keep the session failing: %+v", failAfterReadd)
	}

	// Accept fresh observations at the new revision. The new positive sequences
	// restart at 1, well below the pre-clear 11-13; that is legal because the
	// clear dropped the old records and the per-instance sequence starts at 0.
	markHealth(t, r, "svc", "a", 3, 1, true, "")
	markHealth(t, r, "svc", "c", 3, 1, true, "")

	// Both a (the bound instance) and c (the next id past the parked cursor)
	// are healthy, while b stays unknown. A fresh rotation from the preserved
	// cursor on b would skip b and land on c; the session instead reuses its
	// surviving binding and returns a with its CURRENT address h9:9 and the
	// newly accepted sequence 1 — never h1:1/11 from before the clear.
	assertPick(t, selectSession(t, r, "svc", 3, "s"), "a", "h9:9", 1)

	// The reuse moved the rotation neither forward nor back: the next plain
	// selection continues just after the pre-clear cursor b and lands on c,
	// rather than being brought back to the bound a.
	assertPick(t, selected(t, r, "svc", 3), "c", "h3:3", 1)

	// The successful plain rotation rewrote no session binding.
	assertPick(t, selectSession(t, r, "svc", 3, "s"), "a", "h9:9", 1)

	// The plain rotation then naturally wraps past c to a, and what it returns
	// there is the re-joined record (new address, new sequence).
	assertPick(t, selected(t, r, "svc", 3), "a", "h9:9", 1)

	// Selections and the clear-cycle changed nothing beyond the registrations
	// and explicitly accepted observations: the revision is still 3, a and c
	// hold only their new sequence-1 records at their current addresses, and b
	// — which merely kept its old address — is still unknown at sequence 0.
	if rev := r.RevisionOf("svc"); rev != 3 {
		t.Fatalf("selections changed the registration revision: %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst != (InstanceView{ID: "a", Address: "h9:9", Health: HealthHealthy, Sequence: 1}) {
		t.Fatalf("a final record: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "c"); inst != (InstanceView{ID: "c", Address: "h3:3", Health: HealthHealthy, Sequence: 1}) {
		t.Fatalf("c final record: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst != (InstanceView{ID: "b", Address: "h2:2", Health: HealthUnknown, Sequence: 0}) {
		t.Fatalf("b must stay unknown/0 at its old address without a fresh observation: %+v", inst)
	}
}
