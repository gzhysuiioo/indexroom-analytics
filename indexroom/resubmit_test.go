package indexroom

import "testing"

// This file is the regression guard for resubmitting a service's COMPLETE
// instance list after sessions already exist. A client may register, report
// health and run session-keyed selections in one batch and then post the very
// same list again — reordered, or with surrounding whitespace added around the
// service name, instance ids and addresses. Once the fields are trimmed those
// submissions are content-identical, so each must:
//
//   - succeed without a "changed" outcome and leave the revision exactly where
//     it was: the resubmission is NOT a recreation of the service, so it must
//     not look like revision 0 -> 1 again;
//   - preserve every accepted health observation (state, per-instance sequence
//     and trimmed reason); unknown and unhealthy instances stay exactly as
//     accepted and never have to be re-reported to be eligible again;
//   - preserve the per-service session bindings: a key that differs only in
//     surrounding whitespace names the same session and keeps returning its
//     bound healthy instance with its current address and latest accepted
//     health sequence;
//   - preserve the shared rotation cursor: session reuses advance nothing, and
//     keyless selections continue just after the instance last chosen by a real
//     rotation, wrapping by ascending id, regardless of the order the
//     resubmitted list happened to be written in;
//   - still honor the revision gate: a content-identical list carrying a legal
//     but non-current expectedRevision is a conflict stating the reason and the
//     current revision, and the rejected item clears no session and moves no
//     cursor; a later duplicate carrying the correct revision proceeds normally
//     and sees only the state accepted earlier.
//
// No session management entry point is added or assumed: every guarantee below
// is observed through the same registration, health and selection results the
// product already exposes.

// duplicateResubmission validates and applies one register request expected to
// be content-identical to the service's current list, asserting the outcome is
// an unchanged success at exactly the given revision (never a recreation).
func duplicateResubmission(t *testing.T, r *Registry, service string, rev int64, instances []Instance) {
	t.Helper()
	reg, err := r.ValidateRegistration(service, rev, instances)
	if err != nil {
		t.Fatalf("validate duplicate resubmission for %s: %v", service, err)
	}
	out := r.Apply(reg)
	if !out.OK || out.Changed || out.Revision != int(rev) {
		t.Fatalf("identical resubmission must succeed unchanged at revision %d, got %+v", rev, out)
	}
}

// TestRegistryIdenticalListResubmissionKeepsRevisionHealthSessionsAndCursor is
// the main guard: after a session is bound and the shared rotation has moved,
// two content-identical resubmissions (reordered, with whitespace padding on
// the service name, ids and addresses) succeed without "changed", keep the
// revision at 1, reset no accepted health record, keep the unhealthy and
// unknown instances out of the candidate set, preserve the session across
// padded-key variants, and leave the rotation exactly where the last real
// rotation parked it.
func TestRegistryIdenticalListResubmissionKeepsRevisionHealthSessionsAndCursor(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"},
		{ID: "b", Address: "h2:2"},
		{ID: "c", Address: "h3:3"},
		{ID: "d", Address: "h4:4"},
	})
	markHealth(t, r, "svc", "a", 1, 10, true, "")
	markHealth(t, r, "svc", "b", 1, 20, false, "磁盘故障")
	markHealth(t, r, "svc", "c", 1, 30, true, "")
	// d deliberately stays unknown: it must never enter the candidate set just
	// because the list was resubmitted.

	// The padded key's first success rotates to the smallest healthy id and
	// binds k -> a; b (unhealthy) and d (unknown) are skipped. The cursor rests
	// on a.
	assertPick(t, selectSession(t, r, "svc", 1, "  k  "), "a", "h1:1", 10)

	// A keyless selection continues just after a: c is the next healthy id, so
	// the last real rotation before the resubmission parks the cursor on c.
	assertPick(t, selected(t, r, "svc", 1), "c", "h3:3", 30)

	// Resubmit the complete list with the instances REVERSED and every identity
	// field padded; the service name is padded too. Trimming restores the exact
	// current set, so this must succeed without a change and keep revision 1 —
	// it is not a recreation at revision 0 and must not bump to 2.
	duplicateResubmission(t, r, "  svc  ", 1, []Instance{
		{ID: "  d  ", Address: "  h4:4  "},
		{ID: " c ", Address: "\t h3:3 \n"},
		{ID: " b ", Address: " h2:2 "},
		{ID: "a", Address: "h1:1"},
	})

	// The unpadded key names the same session: the binding survived the
	// resubmission and the reuse reports the bound instance's own address and
	// accepted sequence without rotating.
	assertPick(t, selectSession(t, r, "svc", 1, "k"), "a", "h1:1", 10)

	// A keyless selection continues from the last REAL rotation (c), not from
	// the first id in the resubmitted list (d) and not from the bound a: the
	// healthy rotation wraps past c back to a. This proves neither the reversed
	// submission order nor the session reuse redistributed the target.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 10)
	// The next real rotation lands on c again; b and d are still not candidates.
	assertPick(t, selected(t, r, "svc", 1), "c", "h3:3", 30)

	// The plain rotation rewrote no binding.
	assertPick(t, selectSession(t, r, "svc", 1, "k"), "a", "h1:1", 10)

	// A second identical resubmission in yet another order is unchanged again.
	duplicateResubmission(t, r, "svc", 1, []Instance{
		{ID: "c", Address: "h3:3"},
		{ID: "a", Address: " h1:1 "},
		{ID: "d", Address: "h4:4"},
		{ID: "b", Address: "h2:2"},
	})

	// The padded key variant still reuses the original session, and a keyless
	// selection still resumes from c (the last real rotation above) and wraps
	// to a: the second resubmission reset nothing despite its ordering.
	assertPick(t, selectSession(t, r, "  svc  ", 1, "\t k \n"), "a", "h1:1", 10)
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 10)

	// A newer accepted observation after the resubmissions is what the reuse
	// must then report — the session carries the instance's LATEST accepted
	// health sequence, not one frozen at bind time.
	markHealth(t, r, "svc", "a", 1, 11, true, "")
	assertPick(t, selectSession(t, r, "svc", 1, "k"), "a", "h1:1", 11)

	// Nothing the resubmissions did consumed a revision: still revision 1. The
	// snapshot keeps every original record — a healthy at its latest sequence,
	// b unhealthy with its trimmed reason, c healthy, d unknown at sequence 0 —
	// sorted by id with the trimmed addresses.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("identical resubmissions changed the revision: %d", rev)
	}
	for _, want := range []InstanceView{
		{ID: "a", Address: "h1:1", Health: HealthHealthy, Sequence: 11},
		{ID: "b", Address: "h2:2", Health: HealthUnhealthy, Sequence: 20, Reason: "磁盘故障"},
		{ID: "c", Address: "h3:3", Health: HealthHealthy, Sequence: 30},
		{ID: "d", Address: "h4:4", Health: HealthUnknown, Sequence: 0},
	} {
		if got := instanceHealth(t, r, "svc", want.ID); got != want {
			t.Fatalf("instance %s record after resubmission: got %+v want %+v", want.ID, got, want)
		}
	}
}

// TestRegistryIdenticalResubmissionConflictKeepsSessionAndCursor locks the
// revision gate on an otherwise identical resubmission: a legal
// expectedRevision that merely differs from the current revision is a conflict
// carrying the reason and both revisions, changes nothing, clears no session
// and moves no cursor. The subsequent duplicate submitted with the correct
// revision then succeeds unchanged, duplicate health reports stay unchanged,
// and later selections see only the state accepted before the conflict.
func TestRegistryIdenticalResubmissionConflictKeepsSessionAndCursor(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	markHealth(t, r, "svc", "c", 1, 3, true, "")

	// Bind k -> a (cursor on a), then a keyless rotation moves to b, the last
	// real rotation before the rejected resubmission.
	assertPick(t, selectSession(t, r, "svc", 1, "k"), "a", "h1:1", 1)
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 2)

	// Content-identical list (reordered and padded) but expectedRevision 5 while
	// the service is at 1: conflict, never a false success or a recreation, and
	// the reason names both revisions.
	reg, err := r.ValidateRegistration("  svc  ", 5, []Instance{
		{ID: " c ", Address: " h3:3 "},
		{ID: " b ", Address: " h2:2 "},
		{ID: "a", Address: "h1:1"},
	})
	if err != nil {
		t.Fatalf("the resubmitted content itself is valid: %v", err)
	}
	out := r.Apply(reg)
	if out.OK || out.Kind != OutcomeConflict || out.Changed ||
		out.Expected != 5 || out.Actual != 1 || out.Revision != 1 || out.Reason == "" {
		t.Fatalf("wrong-revision duplicate must conflict at revision 1: %+v", out)
	}

	// The conflict cleared no session: the padded key still reuses a.
	assertPick(t, selectSession(t, r, "svc", 1, "  k  "), "a", "h1:1", 1)

	// The conflict moved no cursor: the keyless rotation continues just after
	// the pre-conflict position b and lands on c. Had the rejected item reset
	// the rotation, this request would restart at the smallest id a instead.
	assertPick(t, selected(t, r, "svc", 1), "c", "h3:3", 3)

	// The same duplicate list with the correct current revision succeeds
	// unchanged at revision 1.
	duplicateResubmission(t, r, "svc", 1, []Instance{
		{ID: "b", Address: "h2:2"}, {ID: "a", Address: " h1:1 "}, {ID: "c", Address: "h3:3"},
	})

	// A duplicate health observation (same sequence, same normalized content)
	// also succeeds without a change and without consuming the revision.
	hupd, err := r.ValidateHealth(" svc ", " a ", 1, 1, true, "")
	if err != nil {
		t.Fatalf("duplicate health validation: %v", err)
	}
	if hout := r.ApplyHealth(hupd); !hout.OK || hout.Changed || hout.Revision != 1 || hout.Sequence != 1 {
		t.Fatalf("duplicate health report must be unchanged: %+v", hout)
	}

	// The session and rotation relationships established before the rejected
	// item are intact: k still reuses a without rotating, and the keyless
	// rotation continues after c and wraps to a.
	assertPick(t, selectSession(t, r, "svc", 1, "k"), "a", "h1:1", 1)
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 1)

	// Final state reflects only originally accepted records at revision 1.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("revision after conflict and duplicates: %d", rev)
	}
	for _, want := range []InstanceView{
		{ID: "a", Address: "h1:1", Health: HealthHealthy, Sequence: 1},
		{ID: "b", Address: "h2:2", Health: HealthHealthy, Sequence: 2},
		{ID: "c", Address: "h3:3", Health: HealthHealthy, Sequence: 3},
	} {
		if got := instanceHealth(t, r, "svc", want.ID); got != want {
			t.Fatalf("instance %s final record: got %+v want %+v", want.ID, got, want)
		}
	}
}
