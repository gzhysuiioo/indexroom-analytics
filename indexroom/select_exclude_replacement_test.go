package indexroom

import (
	"strings"
	"testing"
)

// This file is the regression guard for the rotation position once a service's
// instance list has been REPLACED while the rotation is in use, combined with
// a one-request excludeInstanceIds list. Earlier guards cover exclusion on a
// stable list (select_exclude_test.go) and removal/candidate changes without an
// exclusion (TestRegistrySelectCursorSurvivesReplacementAndRemoval); these tests
// lock their intersection purely through the public Apply/Select/Snapshot
// results:
//
//   - removing an instance (including the last actually chosen one) and
//     otherwise changing the candidate set never restarts the rotation at the
//     smallest id — an excluded selection still continues just after the last
//     actually rotated id, and the exclusion scopes only that one request;
//   - a stale-revision select after a replacement is a conflict naming both
//     revisions, and an all-healthy-excluded select at the current revision is
//     no_healthy naming the exclusion; both report the current revision,
//     fabricate no target and move the cursor, so a later valid select resumes
//     just after the last actually chosen id instead of skipping an instance or
//     returning to the initial position;
//   - instances that keep both id and address across the replacement keep the
//     health observations accepted before it, so an exclusion can never turn an
//     instance unhealthy or remove it from the final service list;
//   - the result is order-independent with respect to how the replacement list
//     is submitted, because matching is by id and rotation is by ascending id.

// replacementAddress is the address each scenario instance id keeps across the
// replacement: a->h1:1, b->h2:2, c->h3:3, d->h4:4.
var replacementAddress = map[string]string{
	"a": "h1:1",
	"b": "h2:2",
	"c": "h3:3",
	"d": "h4:4",
}

// replacementSequence is the distinct healthy sequence each scenario instance
// accepts before the replacement, so a returned sequence identifies the
// actually selected instance rather than just any healthy one.
var replacementSequence = map[string]int64{
	"a": 1,
	"b": 2,
	"c": 3,
	"d": 4,
}

// setupReplacedABCD registers a,b,c,d healthy, rotates twice (a then b, so the
// last actually rotated id is b), then replaces the list at revision 1 with
// the given submission order of a,c,d — b is removed and every kept id keeps
// its address, so the revision becomes 2 and a,c,d keep their accepted healthy
// records (sequences 1, 3, 4). The registry is returned at revision 2 with the
// cursor resting on the now-removed b and the healthy candidate set {a, c, d}.
func setupReplacedABCD(t *testing.T, order ...string) *Registry {
	t.Helper()
	r := NewRegistry()
	instances := []Instance{
		{ID: "a", Address: replacementAddress["a"]},
		{ID: "b", Address: replacementAddress["b"]},
		{ID: "c", Address: replacementAddress["c"]},
		{ID: "d", Address: replacementAddress["d"]},
	}
	registerService(t, r, "svc", 0, instances)
	for _, id := range []string{"a", "b", "c", "d"} {
		markHealth(t, r, "svc", id, 1, replacementSequence[id], true, "")
	}

	// Two ordinary selections: a then b. The cursor rests on b.
	assertPick(t, selected(t, r, "svc", 1), "a", replacementAddress["a"], replacementSequence["a"])
	assertPick(t, selected(t, r, "svc", 1), "b", replacementAddress["b"], replacementSequence["b"])

	if len(order) == 0 {
		order = []string{"a", "c", "d"}
	}
	replacement := make([]Instance, 0, len(order))
	for _, id := range order {
		replacement = append(replacement, Instance{ID: id, Address: replacementAddress[id]})
	}
	registerService(t, r, "svc", 1, replacement)
	if rev := r.RevisionOf("svc"); rev != 2 {
		t.Fatalf("revision after removing b: got %d, want 2", rev)
	}
	return r
}

// snapshotByID returns a service's instances keyed by id, so state can be
// asserted by id without depending on snapshot order.
func snapshotByID(t *testing.T, r *Registry, service string) map[string]InstanceView {
	t.Helper()
	byID := make(map[string]InstanceView)
	for _, view := range r.Snapshot() {
		if view.Service != service {
			continue
		}
		for _, inst := range view.Instances {
			byID[inst.ID] = inst
		}
	}
	return byID
}

// TestRegistryExcludedSelectAfterRemovalContinuesPastRemovedCursor locks the
// core continuation convention: with a,b,c,d healthy and the last actually
// rotated id b, replacing the list to a,c,d (b removed) and then selecting with
// c excluded must continue just AFTER the removed b and choose d — not reset to
// the smallest id (which would pick a) and not pick the merely excluded c. The
// exclusion scopes that one request only: the next plain selection wraps to a,
// and the one after returns c (which was never removed or made unhealthy).
func TestRegistryExcludedSelectAfterRemovalContinuesPastRemovedCursor(t *testing.T) {
	r := setupReplacedABCD(t)

	// Among the non-excluded healthy ids {a, d}, the first id strictly after the
	// removed b is d. The returned address and sequence must be d's own and the
	// revision the post-replacement value 2.
	out := selectExcluding(t, r, "svc", 2, nil, "c")
	assertPick(t, out, "d", replacementAddress["d"], replacementSequence["d"])
	if out.Revision != 2 {
		t.Fatalf("excluded selection must report the current revision 2: %+v", out)
	}

	// Without an exclusion list the rotation wraps from d to the smallest
	// healthy id, then continues to c — the previously excluded c is fully
	// eligible and carries its own record.
	assertPick(t, selected(t, r, "svc", 2), "a", replacementAddress["a"], replacementSequence["a"])
	assertPick(t, selected(t, r, "svc", 2), "c", replacementAddress["c"], replacementSequence["c"])
}

// TestRegistryReplacementKeepsAcceptedHealthAndExclusionChangesNothingDurable
// locks that the replacement preserves health only by (id, address): a, c and d
// keep the healthy records accepted before the replacement (sequences 1, 3, 4),
// the removed b disappears, and the one-request exclusion of c neither marks c
// unhealthy nor deletes it from the final service list.
func TestRegistryReplacementKeepsAcceptedHealthAndExclusionChangesNothingDurable(t *testing.T) {
	r := setupReplacedABCD(t, "d", "a", "c")
	assertPick(t, selectExcluding(t, r, "svc", 2, nil, "c"),
		"d", replacementAddress["d"], replacementSequence["d"])

	byID := snapshotByID(t, r, "svc")
	if len(byID) != 3 {
		t.Fatalf("final list should be exactly {a,c,d}: %+v", byID)
	}
	if _, present := byID["b"]; present {
		t.Fatalf("removed b must not survive in the final list: %+v", byID["b"])
	}
	want := map[string]InstanceView{
		"a": {ID: "a", Address: replacementAddress["a"], Health: HealthHealthy, Sequence: replacementSequence["a"]},
		"c": {ID: "c", Address: replacementAddress["c"], Health: HealthHealthy, Sequence: replacementSequence["c"]},
		"d": {ID: "d", Address: replacementAddress["d"], Health: HealthHealthy, Sequence: replacementSequence["d"]},
	}
	for id, w := range want {
		got, ok := byID[id]
		if !ok {
			t.Fatalf("kept instance %s missing from the final list", id)
		}
		if got != w {
			t.Fatalf("instance %s must keep its accepted record: got %+v want %+v", id, got, w)
		}
	}
}

// TestRegistryExcludedContinuationAfterRemovalIsOrderIndependent locks that the
// submission order of the replacement list cannot change the selection result:
// every permutation of {a, c, d} must still yield d for the c-excluded request
// (cursor on removed b), then a (wrap) and c on the plain requests. Matching is
// by id and rotation is by ascending id, never by list position.
func TestRegistryExcludedContinuationAfterRemovalIsOrderIndependent(t *testing.T) {
	for _, order := range [][]string{
		{"a", "c", "d"},
		{"a", "d", "c"},
		{"c", "a", "d"},
		{"c", "d", "a"},
		{"d", "a", "c"},
		{"d", "c", "a"},
	} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			r := setupReplacedABCD(t, order...)
			assertPick(t, selectExcluding(t, r, "svc", 2, nil, "c"),
				"d", replacementAddress["d"], replacementSequence["d"])
			assertPick(t, selected(t, r, "svc", 2),
				"a", replacementAddress["a"], replacementSequence["a"])
			assertPick(t, selected(t, r, "svc", 2),
				"c", replacementAddress["c"], replacementSequence["c"])
		})
	}
}

// assertNoTarget verifies a failed selection reports the current revision and a
// reason but fabricates neither an instance id, an address nor a sequence.
func assertNoTarget(t *testing.T, out SelectOutcome, kind OutcomeKind, revision int) {
	t.Helper()
	if out.OK || out.Kind != kind {
		t.Fatalf("want %s, got %+v", kind, out)
	}
	if out.Revision != revision {
		t.Fatalf("%s must report the current revision %d: %+v", kind, revision, out)
	}
	if out.Reason == "" {
		t.Fatalf("%s must state a reason: %+v", kind, out)
	}
	if out.InstanceID != "" || out.Address != "" || out.Sequence != 0 {
		t.Fatalf("%s must fabricate no target, got id=%q addr=%q seq=%d", kind, out.InstanceID, out.Address, out.Sequence)
	}
}

// TestRegistrySelectFailuresAfterReplacementKeepPosition locks the failure
// position guarantee after a list replacement:
//
//   - a select still using the pre-replacement revision 1 is a conflict naming
//     the expected and actual revisions and reporting the current revision, but
//     returning no target id, address or sequence;
//   - a select at the current revision that excludes every healthy instance
//     (a, c, d) is no_healthy with a reason saying the exclusion removed them
//     all, likewise at the current revision with no fabricated target;
//   - after both failures a valid select resumes just after the last ACTUALLY
//     chosen id (b, removed): the first candidate is c, proving the failures
//     neither skipped c nor reset the position to a, and the rotation then
//     continues to d and wraps to a.
func TestRegistrySelectFailuresAfterReplacementKeepPosition(t *testing.T) {
	r := setupReplacedABCD(t)

	// Stale revision 1 against the service now at revision 2.
	stale, err := r.ValidateSelection("svc", 1)
	if err != nil {
		t.Fatalf("validate stale-revision select: %v", err)
	}
	conflict := r.Select(stale)
	assertNoTarget(t, conflict, OutcomeConflict, 2)
	if conflict.Expected != 1 || conflict.Actual != 2 {
		t.Fatalf("conflict must name expected 1 and actual 2: %+v", conflict)
	}
	if !strings.Contains(conflict.Reason, "revision 2, not 1") {
		t.Fatalf("conflict reason must state both revisions, got %q", conflict.Reason)
	}

	// Correct revision but every healthy instance excluded for this request.
	noHealthy := selectExcluding(t, r, "svc", 2, nil, "a", "c", "d")
	assertNoTarget(t, noHealthy, OutcomeNoHealthy, 2)
	if !strings.Contains(noHealthy.Reason, "excludeInstanceIds") {
		t.Fatalf("no_healthy reason must say the exclusion removed every healthy instance, got %q", noHealthy.Reason)
	}

	// The failures moved nothing: the last actual rotation still selected b
	// (now removed), so the next valid select takes the first healthy id after
	// b — c — and must not skip it or reset to a.
	assertPick(t, selected(t, r, "svc", 2), "c", replacementAddress["c"], replacementSequence["c"])
	assertPick(t, selected(t, r, "svc", 2), "d", replacementAddress["d"], replacementSequence["d"])
	assertPick(t, selected(t, r, "svc", 2), "a", replacementAddress["a"], replacementSequence["a"])
}

// TestRegistryExcludedAllHealthyFailureThenValidSelectKeepsPosition pins the
// position guarantee to the exclusion failure on its own: excluding every
// healthy instance must not advance the cursor, so the following c-excluded
// request still resumes just after the removed b at d (not at a), and c —
// excluded by the failed request — is healthy and selectable again immediately.
func TestRegistryExcludedAllHealthyFailureThenValidSelectKeepsPosition(t *testing.T) {
	r := setupReplacedABCD(t)

	failed := selectExcluding(t, r, "svc", 2, nil, "a", "c", "d")
	assertNoTarget(t, failed, OutcomeNoHealthy, 2)
	if !strings.Contains(failed.Reason, "excludeInstanceIds") {
		t.Fatalf("no_healthy reason must name the exclusion, got %q", failed.Reason)
	}

	// After the failure the c-excluded rotation still resumes just after the
	// removed b at d; the failure neither advanced the cursor nor removed c.
	assertPick(t, selectExcluding(t, r, "svc", 2, nil, "c"),
		"d", replacementAddress["d"], replacementSequence["d"])
	// c is healthy and registered again immediately, then the rotation wraps.
	assertPick(t, selected(t, r, "svc", 2), "a", replacementAddress["a"], replacementSequence["a"])
	assertPick(t, selected(t, r, "svc", 2), "c", replacementAddress["c"], replacementSequence["c"])

	// The failed exclusion changed no durable state: the final list is exactly
	// {a,c,d}, all healthy at the sequences accepted before the replacement.
	byID := snapshotByID(t, r, "svc")
	if len(byID) != 3 {
		t.Fatalf("final list should be exactly {a,c,d}: %+v", byID)
	}
	for _, id := range []string{"a", "c", "d"} {
		if inst := byID[id]; inst.Health != HealthHealthy ||
			inst.Address != replacementAddress[id] || inst.Sequence != replacementSequence[id] {
			t.Fatalf("instance %s must keep its accepted record: %+v", id, inst)
		}
	}
}
