package indexroom

import "testing"

// This file is the regression guard for resubmitting the COMPLETE instance
// list of a service whose state is already in use: a session is bound, the
// shared rotation cursor has moved, and the instances carry accepted health
// observations (healthy, unhealthy with a reason, and still-unknown).
//
// Resubmitting the same normalized content — even with the instances in a
// different order and whitespace padding around the service name, instance ids
// and addresses — must be an idempotent no-op, not a recreation:
//
//   - the result succeeds with Changed == false and the revision unchanged;
//   - every health record (state, sequence, reason) is retained, so an instance
//     still unknown or unhealthy is not made eligible and no fresh observation
//     is required to keep selecting;
//   - a session key trimmed to an existing key keeps returning that session's
//     bound instance with its current address and latest accepted sequence, and
//     reuse does not advance the rotation;
//   - plain selections continue just after the last REAL rotation selection,
//     honouring ascending id order and end wrap, regardless of the order the
//     duplicate list was submitted in;
//   - identical content does not bypass the revision gate: a valid list with a
//     mismatched expectedRevision is a conflict naming both revisions and clears
//     neither the binding nor the cursor, and a later correct-revision
//     duplicate is still an unchanged success.
//
// Everything here is observed through the public Validate/Apply/Select and
// Snapshot APIs; no session-management entry point is introduced.

// TestRegistryResubmittedIdenticalListKeepsRevisionHealthSessionAndRotation
// walks one service through registration, mixed health observations, a bound
// session and a partially advanced rotation, then resubmits the same list
// (reordered and padded), probes a revision conflict, and verifies that only a
// genuinely new observation changes anything afterwards.
func TestRegistryResubmittedIdenticalListKeepsRevisionHealthSessionAndRotation(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "i1", Address: "h1:1"},
		{ID: "i2", Address: "h2:2"},
		{ID: "i3", Address: "h3:3"},
		{ID: "i4", Address: "h4:4"},
	})
	markHealth(t, r, "svc", "i1", 1, 11, true, "")
	markHealth(t, r, "svc", "i2", 1, 22, true, "")
	markHealth(t, r, "svc", "i3", 1, 33, false, "磁盘故障")
	// i4 deliberately stays unknown at sequence 0 with no reason.

	// The session's first success rotates to the smallest healthy id (i1; i3 is
	// unhealthy and i4 unknown, so neither participates) and binds s -> i1; the
	// shared cursor rests on i1.
	assertPick(t, selectSession(t, r, "svc", 1, "  s  "), "i1", "h1:1", 11)

	// A plain selection advances the rotation to i2.
	assertPick(t, selected(t, r, "svc", 1), "i2", "h2:2", 22)

	// Reusing the session returns the bound instance with its current address
	// and latest accepted sequence without moving the cursor off i2.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "i1", "h1:1", 11)

	// Resubmit the complete list in a different order, with whitespace padding
	// around the service name, every id and every address. After trimming, the
	// normalized ids and addresses are identical to the current list.
	reg, err := r.ValidateRegistration("  svc  ", 1, []Instance{
		{ID: " i4 ", Address: " h4:4 "},
		{ID: " i2 ", Address: " h2:2 "},
		{ID: "  i3  ", Address: "  h3:3  "},
		{ID: "i1", Address: "\th1:1\t"},
	})
	if err != nil {
		t.Fatalf("validate resubmitted list: %v", err)
	}
	out := r.Apply(reg)
	if !out.OK || out.Changed || out.Revision != 1 {
		t.Fatalf("identical resubmission must succeed without a change or revision bump: %+v", out)
	}

	// The padded key trims to the same session: its binding survived the
	// resubmission and still answers i1's current address/sequence. No fresh
	// health report was needed to keep selecting it.
	assertPick(t, selectSession(t, r, "svc", 1, "   s   "), "i1", "h1:1", 11)

	// The plain rotation continues just after the last real rotation (i2) and
	// wraps past the end: unhealthy i3 and unknown i4 are not candidates, so the
	// wrap returns i1 rather than anything the resubmission order would pick.
	assertPick(t, selected(t, r, "svc", 1), "i1", "h1:1", 11)

	// Identical content does not bypass the revision gate: a fully valid list
	// with expectedRevision 2 is a conflict stating the reason and both
	// revisions, not a success and not a recreation of the service.
	reg, err = r.ValidateRegistration("svc", 2, []Instance{
		{ID: "i2", Address: "h2:2"},
		{ID: "i1", Address: "h1:1"},
		{ID: "i4", Address: "h4:4"},
		{ID: "i3", Address: "h3:3"},
	})
	if err != nil {
		t.Fatalf("validate conflicting resubmission: %v", err)
	}
	out = r.Apply(reg)
	if out.OK || out.Kind != OutcomeConflict || out.Expected != 2 || out.Actual != 1 ||
		out.Revision != 1 || out.Reason == "" {
		t.Fatalf("identical content with a wrong revision must conflict with the current revision: %+v", out)
	}

	// The conflict cleared neither the session binding nor the cursor: the key
	// still reuses i1, and the next plain selection continues after i1 to i2 —
	// it does not restart at the smallest id.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "i1", "h1:1", 11)
	assertPick(t, selected(t, r, "svc", 1), "i2", "h2:2", 22)

	// A later duplicate at the correct revision is still an unchanged success.
	reg, err = r.ValidateRegistration("svc", 1, []Instance{
		{ID: "i1", Address: "h1:1"},
		{ID: "i2", Address: "h2:2"},
		{ID: "i3", Address: "h3:3"},
		{ID: "i4", Address: "h4:4"},
	})
	if err != nil {
		t.Fatalf("validate later duplicate: %v", err)
	}
	if out = r.Apply(reg); !out.OK || out.Changed || out.Revision != 1 {
		t.Fatalf("correct-revision duplicate after the conflict must stay unchanged: %+v", out)
	}

	// The final snapshot reflects only the originally accepted records:
	// revision 1, trimmed original addresses, ids in ascending order, i1/i2
	// healthy at their sequences, i3 unhealthy with its reason, and i4 still
	// unknown/0 with no reason — the resubmissions and the conflict changed none
	// of it.
	views := r.Snapshot()
	if len(views) != 1 || views[0].Service != "svc" || views[0].Revision != 1 {
		t.Fatalf("service view: %+v", views)
	}
	want := []InstanceView{
		{ID: "i1", Address: "h1:1", Health: HealthHealthy, Sequence: 11},
		{ID: "i2", Address: "h2:2", Health: HealthHealthy, Sequence: 22},
		{ID: "i3", Address: "h3:3", Health: HealthUnhealthy, Sequence: 33, Reason: "磁盘故障"},
		{ID: "i4", Address: "h4:4", Health: HealthUnknown, Sequence: 0},
	}
	if len(views[0].Instances) != len(want) {
		t.Fatalf("instances: %+v", views[0].Instances)
	}
	for i := range want {
		if views[0].Instances[i] != want[i] {
			t.Fatalf("instance %d: got %+v want %+v", i, views[0].Instances[i], want[i])
		}
	}
}
