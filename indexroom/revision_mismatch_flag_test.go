package indexroom

import "testing"

// RevisionMismatch must be set only on the registration-revision conflict, so
// the command layer can emit expectedRevision/actualRevision even when a side
// is 0 while keeping both fields off every other outcome — including health's
// same-sequence/different-content conflict, which shares the "conflict" kind
// but is not a registration mismatch.

func TestRevisionMismatchFlagOnRegisterOutcome(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "i1", Address: "h1:8080"}}) // revision 1

	// Existing service at revision 1, request submits 0: mismatch (0, 1).
	reg, err := r.ValidateRegistration("svc", 0, nil)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := r.Apply(reg)
	if out.OK || out.Kind != OutcomeConflict || !out.RevisionMismatch ||
		out.Expected != 0 || out.Actual != 1 || out.Revision != 1 {
		t.Fatalf("submit 0 against revision 1: %+v", out)
	}

	// Unknown service at revision 0, request submits 2: mismatch (2, 0).
	reg, err = r.ValidateRegistration("ghost", 2, nil)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	out = r.Apply(reg)
	if out.OK || out.Kind != OutcomeConflict || !out.RevisionMismatch ||
		out.Expected != 2 || out.Actual != 0 || out.Revision != 0 {
		t.Fatalf("submit 2 against an unknown service: %+v", out)
	}

	// A successful registration carries no flag.
	reg, err = r.ValidateRegistration("ghost", 0, nil)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	out = r.Apply(reg)
	if !out.OK || out.RevisionMismatch {
		t.Fatalf("successful creation must not flag a mismatch: %+v", out)
	}
}

func TestRevisionMismatchFlagOnHealthOutcome(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "i1", Address: "h1:8080"}}) // revision 1
	upd, err := r.ValidateHealth("svc", "i1", 1, 1, true, "")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if hout := r.ApplyHealth(upd); !hout.OK {
		t.Fatalf("accept first observation: %+v", hout)
	}

	// Registration-revision mismatch: flagged, with both revision values even
	// though one is 0; no current sequence is reported.
	upd, _ = r.ValidateHealth("svc", "i1", 0, 2, true, "")
	hout := r.ApplyHealth(upd)
	if hout.OK || hout.Kind != OutcomeConflict || !hout.RevisionMismatch ||
		hout.Expected != 0 || hout.Actual != 1 || hout.Revision != 1 || hout.Sequence != 0 {
		t.Fatalf("health revision mismatch: %+v", hout)
	}
	upd, _ = r.ValidateHealth("ghost", "i1", 2, 1, true, "")
	hout = r.ApplyHealth(upd)
	if hout.OK || hout.Kind != OutcomeConflict || !hout.RevisionMismatch ||
		hout.Expected != 2 || hout.Actual != 0 || hout.Revision != 0 {
		t.Fatalf("health against unknown service: %+v", hout)
	}

	// Same accepted sequence reused with different health content: still a
	// conflict and it reports the current sequence, but it is NOT a revision
	// mismatch, so the comparison fields stay off.
	upd, _ = r.ValidateHealth("svc", "i1", 1, 1, false, "连接失败")
	hout = r.ApplyHealth(upd)
	if hout.OK || hout.Kind != OutcomeConflict || hout.RevisionMismatch ||
		hout.Sequence != 1 || hout.Reason == "" {
		t.Fatalf("health content conflict must not flag a revision mismatch: %+v", hout)
	}

	// Stale sequence and missing instance are not revision mismatches either.
	// The identical resubmission first succeeds without changing anything...
	upd, _ = r.ValidateHealth("svc", "i1", 1, 1, true, "")
	if hout = r.ApplyHealth(upd); !hout.OK || hout.RevisionMismatch {
		t.Fatalf("identical resubmission must succeed without the flag: %+v", hout)
	}
	// ...advance to sequence 5, then an older 4 is stale.
	upd2, _ := r.ValidateHealth("svc", "i1", 1, 5, true, "")
	if hout = r.ApplyHealth(upd2); !hout.OK {
		t.Fatalf("advance to sequence 5: %+v", hout)
	}
	upd3, _ := r.ValidateHealth("svc", "i1", 1, 4, true, "")
	if hout = r.ApplyHealth(upd3); hout.OK || hout.Kind != OutcomeStale || hout.RevisionMismatch {
		t.Fatalf("stale observation must not flag a revision mismatch: %+v", hout)
	}
	upd4, _ := r.ValidateHealth("svc", "missing", 1, 1, true, "")
	if hout = r.ApplyHealth(upd4); hout.OK || hout.Kind != OutcomeNotFound || hout.RevisionMismatch {
		t.Fatalf("missing instance must not flag a revision mismatch: %+v", hout)
	}
}

func TestRevisionMismatchFlagOnSelectAndRelease(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, nil) // revision 1, empty list -> no_healthy

	// Existing service, submit 0: revision mismatch flagged (0, 1).
	sel, err := r.ValidateSelection("svc", 0)
	if err != nil {
		t.Fatalf("validate select: %v", err)
	}
	out := r.Select(sel)
	if out.OK || out.Kind != OutcomeConflict || !out.RevisionMismatch ||
		out.Expected != 0 || out.Actual != 1 || out.Revision != 1 {
		t.Fatalf("select revision mismatch: %+v", out)
	}

	// Unknown service, submit 2: revision mismatch flagged (2, 0).
	sel, _ = r.ValidateSelection("ghost", 2)
	out = r.Select(sel)
	if out.OK || out.Kind != OutcomeConflict || !out.RevisionMismatch ||
		out.Expected != 2 || out.Actual != 0 || out.Revision != 0 {
		t.Fatalf("select against unknown service: %+v", out)
	}

	// Unknown service at matching revision 0 is not_found: no comparison fields.
	sel, _ = r.ValidateSelection("ghost", 0)
	out = r.Select(sel)
	if out.OK || out.Kind != OutcomeNotFound || out.RevisionMismatch || out.Revision != 0 {
		t.Fatalf("select not_found must not flag a mismatch: %+v", out)
	}

	// Matching revision on an empty registered service reaches no_healthy.
	sel, _ = r.ValidateSelection("svc", 1)
	out = r.Select(sel)
	if out.OK || out.Kind != OutcomeNoHealthy || out.RevisionMismatch || out.Revision != 1 {
		t.Fatalf("select no_healthy must not flag a mismatch: %+v", out)
	}

	key := "k"
	// Release: revision mismatch flagged for the same gate, both zero sides.
	rel, _ := r.ValidateSessionRelease("svc", 0, &key)
	relOut := r.ReleaseSession(rel)
	if relOut.OK || relOut.Kind != OutcomeConflict || !relOut.RevisionMismatch ||
		relOut.Expected != 0 || relOut.Actual != 1 {
		t.Fatalf("release revision mismatch: %+v", relOut)
	}
	rel, _ = r.ValidateSessionRelease("ghost", 2, &key)
	relOut = r.ReleaseSession(rel)
	if relOut.OK || relOut.Kind != OutcomeConflict || !relOut.RevisionMismatch ||
		relOut.Expected != 2 || relOut.Actual != 0 {
		t.Fatalf("release against unknown service: %+v", relOut)
	}
	// Unknown service at matching revision 0 is not_found: no comparison fields.
	rel, _ = r.ValidateSessionRelease("ghost", 0, &key)
	relOut = r.ReleaseSession(rel)
	if relOut.OK || relOut.Kind != OutcomeNotFound || relOut.RevisionMismatch {
		t.Fatalf("release not_found must not flag a mismatch: %+v", relOut)
	}
}
