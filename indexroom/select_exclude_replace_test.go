package indexroom

import (
	"fmt"
	"strings"
	"testing"
)

// This file is the regression guard for running an excludeInstanceIds
// selection after the service's instance list has been REPLACED: the
// per-service rotation cursor intentionally survives the replacement, so a
// selection carrying an exclude list must continue just after the last
// actually rotated id — even when that id was removed by the replacement —
// instead of restarting at the smallest id. The exclusion itself stays scoped
// to its one request: it neither degrades the excluded instance's health
// record nor removes it from the service. These tests lock the contract
// purely through the public Validate/Apply/Select and Snapshot results:
//
//   - with a, b, c, d healthy and the cursor resting on b, replacing the list
//     (removing b, keeping every other id and address) leaves the position on
//     the removed b: excluding c chooses d, the next plain selection wraps to
//     a, and the one after chooses the previously excluded c;
//   - every successful pick reports the post-replacement revision and the
//     address and accepted health sequence of the instance actually chosen;
//   - the replacement keeps the surviving instances' health records, and the
//     exclude request changes none of them;
//   - the instance submission order, in the original registration and in the
//     replacement, does not change the selection results;
//   - a selection using the pre-replacement revision is a conflict naming
//     both revisions, and excluding every healthy instance is no_healthy
//     naming the exclusion; both failures report the current revision,
//     fabricate no target, and move no cursor, so the next valid selection
//     continues just after the last actually rotated id.

// runReplaceExcludeScenario drives the shared replacement-plus-exclusion
// scenario against a fresh registry with the given instance submission orders
// and locks the full pick sequence and the final service state.
func runReplaceExcludeScenario(t *testing.T, initial, replacement []Instance) {
	t.Helper()
	r := NewRegistry()
	registerService(t, r, "svc", 0, initial)
	markHealth(t, r, "svc", "a", 1, 11, true, "")
	markHealth(t, r, "svc", "b", 1, 12, true, "")
	markHealth(t, r, "svc", "c", 1, 13, true, "")
	markHealth(t, r, "svc", "d", 1, 14, true, "")

	// Ordinary selections take a then b; the cursor rests on b.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 11)
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 12)

	// Replace the list at the current revision: b is removed, every other
	// instance keeps its id and address. The replacement bumps the revision.
	reg, err := r.ValidateRegistration("svc", 1, replacement)
	if err != nil {
		t.Fatalf("validate replacement: %v", err)
	}
	if out := r.Apply(reg); !out.OK || !out.Changed || out.Revision != 2 {
		t.Fatalf("replacement must succeed and bump the revision to 2: %+v", out)
	}

	// assertPickRev additionally locks that the pick reports the
	// post-replacement revision, not the stale pre-replacement one.
	assertPickRev := func(out SelectOutcome, id, addr string, seq int64) {
		t.Helper()
		assertPick(t, out, id, addr, seq)
		if out.Revision != 2 {
			t.Fatalf("select must report the post-replacement revision 2, got %+v", out)
		}
	}

	// Excluding c continues just after the removed b: d is chosen with its own
	// address and accepted sequence. The position neither restarts at the
	// smallest id (a) nor skips to the excluded c.
	assertPickRev(selectExcluding(t, r, "svc", 2, nil, "c"), "d", "h4:4", 14)
	// A plain selection wraps past the end to a, and the one after chooses c:
	// the exclusion was scoped to its one request and never removed c from the
	// rotation.
	assertPickRev(selected(t, r, "svc", 2), "a", "h1:1", 11)
	assertPickRev(selected(t, r, "svc", 2), "c", "h3:3", 13)

	// The exclusion changed nothing durable: the revision is the replaced one,
	// b is gone, and the surviving instances keep the health records accepted
	// before the replacement — c in particular is still healthy at sequence 13.
	if rev := r.RevisionOf("svc"); rev != 2 {
		t.Fatalf("revision after replacement: %d", rev)
	}
	views := r.Snapshot()
	if len(views) != 1 || views[0].Service != "svc" || views[0].Revision != 2 {
		t.Fatalf("service view: %+v", views)
	}
	want := []InstanceView{
		{ID: "a", Address: "h1:1", Health: HealthHealthy, Sequence: 11},
		{ID: "c", Address: "h3:3", Health: HealthHealthy, Sequence: 13},
		{ID: "d", Address: "h4:4", Health: HealthHealthy, Sequence: 14},
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

// TestRegistrySelectExcludeAfterReplacementKeepsRotation locks the main
// scenario: the exclude-carrying selection after the replacement continues
// the rotation from the removed b rather than restarting, and the exclusion
// leaves no durable trace. The whole scenario runs under two different
// instance submission orders — for the original registration and for the
// replacement — and must produce the identical pick sequence, because the
// rotation runs by ascending id, never by submission order.
func TestRegistrySelectExcludeAfterReplacementKeepsRotation(t *testing.T) {
	orders := []struct {
		initial     []Instance
		replacement []Instance
	}{
		{
			initial: []Instance{
				{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"},
				{ID: "c", Address: "h3:3"}, {ID: "d", Address: "h4:4"},
			},
			replacement: []Instance{
				{ID: "a", Address: "h1:1"}, {ID: "c", Address: "h3:3"}, {ID: "d", Address: "h4:4"},
			},
		},
		{
			initial: []Instance{
				{ID: "d", Address: "h4:4"}, {ID: "b", Address: "h2:2"},
				{ID: "a", Address: "h1:1"}, {ID: "c", Address: "h3:3"},
			},
			replacement: []Instance{
				{ID: "d", Address: "h4:4"}, {ID: "a", Address: "h1:1"}, {ID: "c", Address: "h3:3"},
			},
		},
	}
	for i, tc := range orders {
		t.Run(fmt.Sprintf("submission order %d", i+1), func(t *testing.T) {
			runReplaceExcludeScenario(t, tc.initial, tc.replacement)
		})
	}
}

// TestRegistrySelectExcludeFailuresAfterReplacementKeepPosition locks the
// failure shapes after the replacement and their effect on the position: a
// selection carrying the pre-replacement revision conflicts with both
// revisions, and a correct-revision selection excluding every healthy
// instance is no_healthy with a reason naming the exclusion. Both report the
// current revision and fabricate no target id, address or sequence, and
// neither moves the cursor — the next valid selection continues just after
// the last actually rotated id.
func TestRegistrySelectExcludeFailuresAfterReplacementKeepPosition(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"},
		{ID: "c", Address: "h3:3"}, {ID: "d", Address: "h4:4"},
	})
	markHealth(t, r, "svc", "a", 1, 11, true, "")
	markHealth(t, r, "svc", "b", 1, 12, true, "")
	markHealth(t, r, "svc", "c", 1, 13, true, "")
	markHealth(t, r, "svc", "d", 1, 14, true, "")

	// The cursor rests on b, which the replacement then removes.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 11)
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 12)
	registerService(t, r, "svc", 1, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "c", Address: "h3:3"}, {ID: "d", Address: "h4:4"},
	})

	assertNoTarget := func(out SelectOutcome) {
		t.Helper()
		if out.InstanceID != "" || out.Address != "" || out.Sequence != 0 {
			t.Fatalf("failed select must fabricate no target, got id=%q addr=%q seq=%d",
				out.InstanceID, out.Address, out.Sequence)
		}
	}

	// A caller still using the pre-replacement revision gets a conflict that
	// states the expected and actual revisions and reports the current one.
	sel, err := r.ValidateSelection("svc", 1)
	if err != nil {
		t.Fatalf("validate stale-revision select: %v", err)
	}
	out := r.Select(sel)
	if out.OK || out.Kind != OutcomeConflict ||
		out.Expected != 1 || out.Actual != 2 || out.Revision != 2 {
		t.Fatalf("stale revision must conflict with both revisions: %+v", out)
	}
	if !strings.Contains(out.Reason, "revision 2, not 1") {
		t.Fatalf("conflict reason must state both revisions, got %q", out.Reason)
	}
	assertNoTarget(out)

	// The correct revision with every healthy instance excluded is no_healthy;
	// the reason says the exclusion removed them all.
	out = selectExcluding(t, r, "svc", 2, nil, "a", "c", "d")
	if out.OK || out.Kind != OutcomeNoHealthy || out.Revision != 2 {
		t.Fatalf("excluding every healthy instance must be no_healthy at the current revision: %+v", out)
	}
	if !strings.Contains(out.Reason, "excludeInstanceIds") {
		t.Fatalf("no_healthy reason must say the exclusion removed every healthy instance, got %q", out.Reason)
	}
	assertNoTarget(out)

	// Both failures left the cursor on the removed b: the next valid selection
	// continues just after it and picks c — not d, which would mean skipping
	// the instance that was due, and not a, which would mean restarting from
	// the smallest id.
	assertPick(t, selected(t, r, "svc", 2), "c", "h3:3", 13)
	assertPick(t, selected(t, r, "svc", 2), "d", "h4:4", 14)
}
