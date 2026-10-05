package indexroom

import "testing"

// This file is the regression guard for a session bound in a service whose
// instance list is cleared (instances: []) and then populated again with the
// same instance ids. Clearing removes the instances and their health records
// but leaves the service, its revision history, the rotation cursor and the
// session bindings in place. Whether a previously bound session can reuse its
// target is therefore decided entirely by the instance state seen when the
// NEXT selection is handled, never by memory of the old list:
//
//   - while the list is empty (or the rebound id is back but still unknown),
//     the keyed selection is no_healthy: current revision, non-empty reason,
//     no fabricated instance id/address/sequence, and the failure rewrites no
//     binding and moves no cursor — it must not degrade to not_found, because
//     the service survives an empty list;
//   - re-adding an id starts it from unknown/sequence 0/no reason even when
//     both the id and the address are byte-identical to a removed instance, so
//     a session bound to that id cannot follow an old address or sequence onto
//     the fresh record — a healthy observation at the CURRENT revision is
//     required first, and its new positive sequence may sit below the sequence
//     the cleared record had;
//   - once the bound id is healthy again without any successful rebind in
//     between, the SAME session reuses it directly, reporting the rejoined
//     record's current address and sequence while leaving the rotation where
//     the last real rotation parked it;
//   - a plain selection afterwards continues just after the id the last
//     pre-clear rotation actually returned: the no_healthy failures and the
//     keyed reuses neither rewind it nor drag it back to the bound instance.

// TestRegistrySessionClearListThenRejoinKeepsBindingWithoutOldHealth walks one
// bound session through a full clear/rejoin cycle and observes the binding only
// through what later selections return.
func TestRegistrySessionClearListThenRejoinKeepsBindingWithoutOldHealth(t *testing.T) {
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

	// A plain selection without a session key advances the rotation to b; that
	// position is the last real rotation before the clear.
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 12)

	// Clear the instance list at the current revision. The service survives and
	// the revision advances; instances and health records disappear together.
	registerService(t, r, "svc", 1, nil)
	if rev := r.RevisionOf("svc"); rev != 2 {
		t.Fatalf("revision after clear: %d", rev)
	}

	// The bound session requests a target on the empty list. The service still
	// exists, so this is no_healthy at the current revision with a stated
	// reason — never not_found — and it carries no id, address or sequence from
	// before the clear.
	failed := selectSession(t, r, "svc", 2, "s")
	if failed.OK || failed.Kind != OutcomeNoHealthy {
		t.Fatalf("select on the cleared list should be no_healthy, got %+v", failed)
	}
	if failed.Revision != 2 || failed.Reason == "" {
		t.Fatalf("no_healthy must state the current revision and a reason, got %+v", failed)
	}
	if failed.InstanceID != "" || failed.Address != "" || failed.Sequence != 0 {
		t.Fatalf("no_healthy must fabricate no target from the cleared list, got %+v", failed)
	}

	// Re-add the same ids: a moves to a new address while b and c keep their
	// old addresses. The content change bumps the revision once.
	registerService(t, r, "svc", 2, []Instance{
		{ID: "a", Address: "h9:9"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	if rev := r.RevisionOf("svc"); rev != 3 {
		t.Fatalf("revision after rejoin: %d", rev)
	}

	// Every rejoined instance starts from unknown/0/no reason — including b and
	// c, whose ids AND addresses match the removed records exactly. Old health
	// must not be recovered by either identity.
	for _, want := range []InstanceView{
		{ID: "a", Address: "h9:9", Health: HealthUnknown, Sequence: 0},
		{ID: "b", Address: "h2:2", Health: HealthUnknown, Sequence: 0},
		{ID: "c", Address: "h3:3", Health: HealthUnknown, Sequence: 0},
	} {
		if got := instanceHealth(t, r, "svc", want.ID); got != want {
			t.Fatalf("rejoined %s must start fresh: got %+v want %+v", want.ID, got, want)
		}
	}

	// Before any fresh observation is accepted, the same session still fails —
	// an unknown bound instance behaves like a removed one for reuse — and the
	// failure again leaks no target and changes no revision.
	failed = selectSession(t, r, "svc", 3, "s")
	if failed.OK || failed.Kind != OutcomeNoHealthy || failed.Revision != 3 || failed.Reason == "" {
		t.Fatalf("select with only unknown instances should be no_healthy, got %+v", failed)
	}
	if failed.InstanceID != "" || failed.Address != "" || failed.Sequence != 0 {
		t.Fatalf("no_healthy must fabricate no target, got %+v", failed)
	}

	// An observation addressed to a pre-clear revision conflicts even with a
	// sequence far above the cleared 11 and heals nothing on the new record.
	upd, err := r.ValidateHealth("svc", "a", 1, 99, true, "")
	if err != nil {
		t.Fatalf("validate stale-revision report: %v", err)
	}
	if out := r.ApplyHealth(upd); out.OK || out.Kind != OutcomeConflict ||
		out.Expected != 1 || out.Actual != 3 || out.Revision != 3 {
		t.Fatalf("old-revision observation should conflict: %+v", out)
	}

	// Accept a healthy observation at the CURRENT revision. The fresh positive
	// sequence 1 is free to stay below the 11 accepted for the cleared address.
	markHealth(t, r, "svc", "a", 3, 1, true, "")

	// No successful rebind happened during the failure window, so the session
	// reuses a directly — keyed by its id but carrying the rejoined record's
	// CURRENT address h9:9 and sequence 1, never h1:1/11. The reuse does not
	// rotate.
	assertPick(t, selectSession(t, r, "svc", 3, "s"), "a", "h9:9", 1)

	// Another instance (c), whose id follows the parked cursor b, becomes
	// healthy on its reset record, also at a sequence below its pre-clear one.
	markHealth(t, r, "svc", "c", 3, 1, true, "")

	// A plain selection resumes from the preserved rotation position (just
	// after the last pre-clear pick b) and lands on c, not on the session's
	// bound a: the failures and keyed reuses never moved the cursor.
	assertPick(t, selected(t, r, "svc", 3), "c", "h3:3", 1)

	// The next plain rotation wraps past c to a — a result of the rotation, not
	// of the session binding — and reports a's current rejoined record.
	assertPick(t, selected(t, r, "svc", 3), "a", "h9:9", 1)

	// The plain rotations rewrote no session binding: the keyed request still
	// answers a with the current record.
	assertPick(t, selectSession(t, r, "svc", 3, "s"), "a", "h9:9", 1)

	// Selections changed neither the revision nor any accepted health record; b
	// was never re-observed and stays unknown/0 despite reusing its old address.
	if rev := r.RevisionOf("svc"); rev != 3 {
		t.Fatalf("selections changed the registration revision: %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthHealthy || inst.Sequence != 1 || inst.Reason != "" {
		t.Fatalf("a final record: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst.Health != HealthUnknown || inst.Sequence != 0 || inst.Reason != "" {
		t.Fatalf("b must stay a fresh unknown record: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "c"); inst.Health != HealthHealthy || inst.Sequence != 1 || inst.Reason != "" {
		t.Fatalf("c final record: %+v", inst)
	}
}
