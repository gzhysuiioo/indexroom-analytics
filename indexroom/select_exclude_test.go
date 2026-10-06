package indexroom

import (
	"strings"
	"testing"
)

// This file is the regression guard for the select request's optional
// excludeInstanceIds list. The list scopes one request only: excluded
// instances stay registered with their health records and remain eligible for
// later requests that carry no list. These tests lock the contract purely
// through the public validation, selection and snapshot results:
//
//   - the rotation still continues just after the last actually rotated id,
//     skipping excluded ids without resetting the position, and wraps to the
//     smallest candidate only when no candidate follows;
//   - a session whose bound instance is excluded falls back to the filtered
//     rotation and rebinds only its own key on success, while other sessions
//     keep their bindings;
//   - excluding every healthy instance is no_healthy with a reason naming the
//     exclusion, and failures move neither the cursor nor any binding;
//   - the list is trimmed and deduplicated, unknown ids are ignored, a blank
//     id is invalid naming excludeInstanceIds, and field validation precedes
//     the revision check.

// selectExcluding validates and runs one select carrying an excludeInstanceIds
// list (and optionally a session key), returning the outcome so failure cases
// can be asserted directly instead of failing the helper.
func selectExcluding(t *testing.T, r *Registry, service string, revision int64, key *string, excludes ...string) SelectOutcome {
	t.Helper()
	sel, err := r.ValidateSelectionWithExclusions(service, revision, key, excludes)
	if err != nil {
		t.Fatalf("validate select excludes %v: %v", excludes, err)
	}
	return r.Select(sel)
}

// TestRegistrySelectExcludeRotatesPastExcludedIDs locks the spec's rotation
// example: with the cursor resting on a and a, b, c all healthy, excluding b
// chooses c, the next plain selection wraps to a, and the one after chooses
// the previously excluded b — the exclusion neither resets the position nor
// removes the instance from later rotations.
func TestRegistrySelectExcludeRotatesPastExcludedIDs(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	markHealth(t, r, "svc", "c", 1, 3, true, "")

	// Park the cursor on a with an ordinary first selection.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 1)

	// Excluding b skips it for this one request: the rotation continues just
	// after a and lands on c.
	assertPick(t, selectExcluding(t, r, "svc", 1, nil, "b"), "c", "h3:3", 3)

	// Plain selections are unaffected by the earlier exclusion: from c the
	// rotation wraps to a, then continues to b — the excluded instance was
	// never removed and rejoins the rotation in id order.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 1)
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 2)
}

// TestRegistrySelectExcludeLastChosenKeepsCursor locks that excluding the
// instance the cursor rests on does not reset the rotation: the next
// non-excluded healthy id after the cursor is chosen, wrapping only past the
// end. It also locks that the exclusion changes no registration state: the
// excluded instance keeps its health record and the revision stays put.
func TestRegistrySelectExcludeLastChosenKeepsCursor(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"},
	})
	markHealth(t, r, "svc", "a", 1, 5, true, "")
	markHealth(t, r, "svc", "b", 1, 6, true, "")

	// Cursor rests on a after the first rotation.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 5)

	// Excluding a — the last actually rotated id — still continues just after
	// it, choosing b; the position is not restarted at the smallest id.
	assertPick(t, selectExcluding(t, r, "svc", 1, nil, "a"), "b", "h2:2", 6)

	// Excluding b (now the cursor) wraps past the end to a, the only other
	// candidate.
	assertPick(t, selectExcluding(t, r, "svc", 1, nil, "b"), "a", "h1:1", 5)

	// The exclusions changed nothing durable: both instances keep their
	// accepted health records and the revision never moved.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("exclusion changed the registration revision: %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthHealthy || inst.Sequence != 5 {
		t.Fatalf("a record changed by exclusion: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst.Health != HealthHealthy || inst.Sequence != 6 {
		t.Fatalf("b record changed by exclusion: %+v", inst)
	}
}

// TestRegistrySelectExcludeSessionFallbackRebindsOnlyThatKey locks the session
// interaction: a bound instance that is excluded is treated like an
// unavailable one — the request falls back to the filtered rotation, rebinds
// only its own key on success and advances the cursor, while another session
// bound elsewhere keeps its binding. A later request without the list reuses
// the new binding, and excluding the new binding falls back again.
func TestRegistrySelectExcludeSessionFallbackRebindsOnlyThatKey(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	markHealth(t, r, "svc", "c", 1, 3, true, "")

	// x -> a, then y -> b; the shared cursor rests on b.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// x excludes its own bound instance a: the request cannot reuse a and
	// falls back to the rotation just after b, choosing c among the
	// non-excluded candidates {b, c}. Only x's binding moves, to c, and the
	// cursor advances with this real rotation.
	keyX := "x"
	assertPick(t, selectExcluding(t, r, "svc", 1, &keyX, "a"), "c", "h3:3", 3)

	// y is unaffected: its own binding to b is still returned, and the reuse
	// does not move the cursor.
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// x without a list reuses the new binding c; the exclusion was scoped to
	// the one request that carried it.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "c", "h3:3", 3)

	// A plain select continues just after the last real rotation (c) and
	// wraps to a — the excluded instance is fully eligible again.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 1)

	// Excluding x's new binding c falls back again: from the cursor on a the
	// filtered candidates {a, b} yield b, and only x rebinds.
	assertPick(t, selectExcluding(t, r, "svc", 1, &keyX, "c"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)
}

// TestRegistrySelectExcludeAllHealthyIsNoHealthy locks the failure shape when
// the exclusion removes every healthy instance: no_healthy at the current
// revision with a reason that says the exclusion did it, no fabricated
// target, and no movement of the cursor or any session binding. It also locks
// that a service with no healthy instance at all keeps the ordinary reason
// even when a list is submitted.
func TestRegistrySelectExcludeAllHealthyIsNoHealthy(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")

	// x -> a; the cursor rests on a.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)

	assertAllExcluded := func(out SelectOutcome) {
		t.Helper()
		if out.OK || out.Kind != OutcomeNoHealthy {
			t.Fatalf("should be no_healthy, got %+v", out)
		}
		if out.Revision != 1 {
			t.Fatalf("no_healthy must report the current revision, got %+v", out)
		}
		if !strings.Contains(out.Reason, "excludeInstanceIds") {
			t.Fatalf("reason must say the exclusion removed every healthy instance, got %q", out.Reason)
		}
		if out.InstanceID != "" || out.Address != "" || out.Sequence != 0 {
			t.Fatalf("no_healthy must fabricate no target, got id=%q addr=%q seq=%d", out.InstanceID, out.Address, out.Sequence)
		}
	}

	// Plain and keyed requests excluding every healthy instance fail the same
	// way; the keyed failure must not rewrite x's binding.
	assertAllExcluded(selectExcluding(t, r, "svc", 1, nil, "a", "b"))
	keyX := "x"
	assertAllExcluded(selectExcluding(t, r, "svc", 1, &keyX, "b", "a"))

	// The failures moved nothing: the plain rotation still continues just
	// after a to b, and x still reuses its preserved binding a.
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)

	// With no healthy instance at all, a submitted list does not change the
	// ordinary no_healthy reason.
	markHealth(t, r, "svc", "a", 1, 3, false, "down")
	markHealth(t, r, "svc", "b", 1, 4, false, "down")
	out := selectExcluding(t, r, "svc", 1, nil, "a")
	if out.OK || out.Kind != OutcomeNoHealthy {
		t.Fatalf("should be no_healthy, got %+v", out)
	}
	if strings.Contains(out.Reason, "excludeInstanceIds") {
		t.Fatalf("no healthy instance at all must keep the ordinary reason, got %q", out.Reason)
	}
}

// TestRegistrySelectExcludeValidation locks the list's field rules: ids are
// trimmed and deduplicated, unknown ids are ignored, an empty list selects
// exactly as if the field were absent, a blank-after-trim id is invalid with
// a reason naming excludeInstanceIds, and the check runs before any revision
// comparison so a bad list can never partially apply.
func TestRegistrySelectExcludeValidation(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	markHealth(t, r, "svc", "c", 1, 3, true, "")

	// A blank-after-trim id is invalid and names the excludeInstanceIds
	// problem; the valid ids alongside it cannot make the item apply.
	for _, excludes := range [][]string{{"  "}, {"b", " ", "c"}} {
		if _, err := r.ValidateSelectionWithExclusions("svc", 1, nil, excludes); err == nil {
			t.Fatalf("excludes %v should be invalid", excludes)
		} else if !strings.Contains(err.Error(), "excludeInstanceIds") {
			t.Fatalf("reason must name excludeInstanceIds, got %q", err)
		}
	}

	// The blank-id check precedes the revision comparison: a bad list with a
	// mismatching revision is still invalid, never a conflict.
	if _, err := r.ValidateSelectionWithExclusions("svc", 99, nil, []string{"\t"}); err == nil {
		t.Fatalf("blank id with wrong revision should still be invalid")
	} else if !strings.Contains(err.Error(), "excludeInstanceIds") {
		t.Fatalf("reason must name excludeInstanceIds, got %q", err)
	}

	// Ids are trimmed and deduplicated, and unknown ids are ignored: this
	// list excludes exactly b. The first rotation therefore lands on a.
	assertPick(t, selectExcluding(t, r, "svc", 1, nil, " b ", "b", "ghost"), "a", "h1:1", 1)

	// An empty list selects exactly as if the field were absent: the rotation
	// continues just after a to b — the excluded id from before is eligible.
	assertPick(t, selectExcluding(t, r, "svc", 1, nil), "b", "h2:2", 2)
	assertPick(t, selectExcluding(t, r, "svc", 1, nil, []string{}...), "c", "h3:3", 3)

	// The failed validations never moved the cursor: the next plain selection
	// wraps past c to a rather than restarting anywhere.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 1)
}
