package indexroom

import "testing"

// This file locks the snapshot contract of Registry.Snapshot: the returned
// []ServiceView is a value copy of the service state accepted at read time.
// Saving the result or adjusting it for display must never change the
// registry, its accepted health records, the registration revision or the
// target-selection cursor; and a later accepted business operation must never
// reach back into a previously returned snapshot — even when the two moments
// contain an instance with the same id. Every test below pairs a saved
// snapshot with the real state after a normal public operation and asserts
// the old copy and the freshly read state stay independent.

// serviceViewNamed finds one service in a saved snapshot, failing the test.
func serviceViewNamed(t *testing.T, views []ServiceView, service string) ServiceView {
	t.Helper()
	for _, v := range views {
		if v.Service == service {
			return v
		}
	}
	t.Fatalf("service %q not found in snapshot %+v", service, views)
	return ServiceView{}
}

// instanceViewByID finds one instance inside a saved service view.
func instanceViewByID(t *testing.T, view ServiceView, id string) InstanceView {
	t.Helper()
	for _, inst := range view.Instances {
		if inst.ID == id {
			return inst
		}
	}
	t.Fatalf("instance %q not found in service %q snapshot %+v", id, view.Service, view.Instances)
	return InstanceView{}
}

// TestSnapshotFrozenAtReadTimeAcrossHealthObservation fixes the recovery
// scenario: after saving a snapshot showing an unhealthy instance at sequence
// 5 with its reason, a legal newer observation (healthy at sequence 6) changes
// only freshly read snapshots. The saved snapshot keeps health, sequence and
// reason exactly as accepted at read time, and health observations still do
// not bump the registration revision.
func TestSnapshotFrozenAtReadTimeAcrossHealthObservation(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})
	markHealth(t, r, "svc", "a", 1, 5, false, "disk full")

	before := r.Snapshot()
	old := instanceViewByID(t, serviceViewNamed(t, before, "svc"), "a")
	if old.Address != "h:1" || old.Health != HealthUnhealthy || old.Sequence != 5 || old.Reason != "disk full" {
		t.Fatalf("old snapshot should capture the seq-5 observation: %+v", old)
	}

	// A later, legal observation recovers the instance at sequence 6.
	markHealth(t, r, "svc", "a", 1, 6, true, "")

	// The saved snapshot still describes read time, sequence 5 and all.
	savedView := serviceViewNamed(t, before, "svc")
	if savedView.Revision != 1 {
		t.Fatalf("old snapshot revision changed after a health observation: %+v", savedView)
	}
	if got := instanceViewByID(t, savedView, "a"); got.Health != HealthUnhealthy ||
		got.Sequence != 5 || got.Reason != "disk full" || got.Address != "h:1" {
		t.Fatalf("old snapshot followed the newer observation: %+v", got)
	}

	// A new snapshot reflects the accepted recovery: healthy, sequence 6,
	// reason cleared, same address; the revision is still 1.
	afterView := serviceViewNamed(t, r.Snapshot(), "svc")
	if afterView.Revision != 1 {
		t.Fatalf("health observations must not bump the registration revision: %+v", afterView)
	}
	if got := instanceViewByID(t, afterView, "a"); got.Health != HealthHealthy ||
		got.Sequence != 6 || got.Reason != "" || got.Address != "h:1" {
		t.Fatalf("new snapshot should show the seq-6 recovery: %+v", got)
	}
}

// TestSnapshotFrozenAtReadTimeAcrossInstanceReplacement fixes the replacement
// scenario: after saving a snapshot, a successful instance-list replacement
// must not alter the saved revision, members or addresses — even for an
// instance whose id survives. The new snapshot follows the established
// replacement rules instead: same id and address keeps its health record, a
// changed address restarts at unknown/0/no reason, dropped ids disappear and
// new ids start fresh.
func TestSnapshotFrozenAtReadTimeAcrossInstanceReplacement(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h:1"},
		{ID: "b", Address: "h:2"},
		{ID: "c", Address: "h:3"},
	})
	markHealth(t, r, "svc", "a", 1, 10, true, "")
	markHealth(t, r, "svc", "b", 1, 20, false, "connection refused")
	markHealth(t, r, "svc", "c", 1, 30, true, "")

	before := r.Snapshot()
	if oldView := serviceViewNamed(t, before, "svc"); oldView.Revision != 1 || len(oldView.Instances) != 3 {
		t.Fatalf("old snapshot baseline: %+v", oldView)
	}

	// a keeps id+address (observation survives); same-named b moves address
	// (observation must reset); c is dropped; d joins fresh.
	registerService(t, r, "svc", 1, []Instance{
		{ID: "b", Address: "h:9"},
		{ID: "d", Address: "h:4"},
		{ID: "a", Address: "h:1"},
	})
	if rev := r.RevisionOf("svc"); rev != 2 {
		t.Fatalf("revision after replacement: %d", rev)
	}

	// The saved snapshot still describes read time: revision 1 with exactly
	// the members, addresses and observations accepted then. Same-named b in
	// particular must not follow its replacement-time reset.
	saved := serviceViewNamed(t, before, "svc")
	if saved.Revision != 1 {
		t.Fatalf("old snapshot revision followed the replacement: %+v", saved)
	}
	if len(saved.Instances) != 3 {
		t.Fatalf("old snapshot membership followed the replacement: %+v", saved.Instances)
	}
	wantOld := map[string]InstanceView{
		"a": {ID: "a", Address: "h:1", Health: HealthHealthy, Sequence: 10, Reason: ""},
		"b": {ID: "b", Address: "h:2", Health: HealthUnhealthy, Sequence: 20, Reason: "connection refused"},
		"c": {ID: "c", Address: "h:3", Health: HealthHealthy, Sequence: 30, Reason: ""},
	}
	for id, want := range wantOld {
		if got := instanceViewByID(t, saved, id); got != want {
			t.Fatalf("old snapshot instance %s followed the replacement: got %+v want %+v", id, got, want)
		}
	}

	// A new snapshot follows the existing replacement rules, sorted by id.
	fresh := serviceViewNamed(t, r.Snapshot(), "svc")
	if fresh.Revision != 2 || len(fresh.Instances) != 3 {
		t.Fatalf("new snapshot should show the replacement at revision 2: %+v", fresh)
	}
	wantNew := map[string]InstanceView{
		"a": {ID: "a", Address: "h:1", Health: HealthHealthy, Sequence: 10, Reason: ""},
		"b": {ID: "b", Address: "h:9", Health: HealthUnknown, Sequence: 0, Reason: ""},
		"d": {ID: "d", Address: "h:4", Health: HealthUnknown, Sequence: 0, Reason: ""},
	}
	for id, want := range wantNew {
		if got := instanceViewByID(t, fresh, id); got != want {
			t.Fatalf("new snapshot instance %s: got %+v want %+v", id, got, want)
		}
	}
}

// TestSnapshotCallerMutationCannotChangeRegistry fixes the display-side
// convention: a caller may freely overwrite the service name, revision,
// instance identity, address, health, sequence, reason and list membership of
// a returned snapshot. None of that reaches the registry — a fresh snapshot
// still shows the accepted truth and target selection still returns the real
// healthy instance's address and sequence.
func TestSnapshotCallerMutationCannotChangeRegistry(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h:1"},
		{ID: "b", Address: "h:2"},
	})
	markHealth(t, r, "svc", "a", 1, 7, true, "")
	markHealth(t, r, "svc", "b", 1, 8, false, "down")

	saved := r.Snapshot()
	view := &saved[0]
	view.Service = "tampered" // service information
	view.Revision = 999
	view.Instances[0].ID = "z"
	view.Instances[0].Address = "x:9" // instance address
	view.Instances[0].Health = HealthUnhealthy
	view.Instances[0].Sequence = 123 // health state and sequence
	view.Instances[0].Reason = "fake"
	view.Instances = append(view.Instances, InstanceView{ID: "q", Address: "q:1"}) // add a member
	view.Instances = view.Instances[:1]                                            // drop the real second member

	// The registry still serves the accepted state at the accepted revision.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("mutating a snapshot changed the registration revision: %d", rev)
	}
	fresh := serviceViewNamed(t, r.Snapshot(), "svc")
	if fresh.Service != "svc" || fresh.Revision != 1 || len(fresh.Instances) != 2 {
		t.Fatalf("accepted service state changed after mutating a snapshot: %+v", fresh)
	}
	want := map[string]InstanceView{
		"a": {ID: "a", Address: "h:1", Health: HealthHealthy, Sequence: 7, Reason: ""},
		"b": {ID: "b", Address: "h:2", Health: HealthUnhealthy, Sequence: 8, Reason: "down"},
	}
	for id, w := range want {
		if got := instanceViewByID(t, fresh, id); got != w {
			t.Fatalf("accepted instance %s changed after mutating a snapshot: got %+v want %+v", id, got, w)
		}
	}

	// Target selection reads the registry, not the caller's tampered copy:
	// with no prior selection it starts at the smallest healthy id, a, using
	// its real address and accepted sequence.
	sel, _ := r.ValidateSelection("svc", 1)
	out := r.Select(sel)
	if !out.OK || out.InstanceID != "a" || out.Address != "h:1" || out.Sequence != 7 {
		t.Fatalf("selection followed the tampered snapshot instead of accepted state: %+v", out)
	}
}

// TestConsecutiveSnapshotsAreIndependent fixes that two reads back to back own
// separate storage: adjusting fields or list membership of one snapshot must
// neither contaminate the other nor leak back into the first once the second
// is adjusted as well.
func TestConsecutiveSnapshotsAreIndependent(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h:1"},
		{ID: "b", Address: "h:2"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, false, "down")

	first := r.Snapshot()
	second := r.Snapshot()

	// Adjust the first snapshot in every shape a display caller might.
	first[0].Service = "renamed"
	first[0].Revision = 42
	first[0].Instances[0].Address = "changed:1"
	first[0].Instances[0].Health = HealthUnhealthy
	first[0].Instances[0].Sequence = 99
	first[0].Instances[0].Reason = "made up"
	first[0].Instances = append(first[0].Instances, InstanceView{ID: "ghost"})
	first[0].Instances[1] = InstanceView{}

	// The second snapshot keeps its own read-time values.
	if len(second) != 1 || second[0].Service != "svc" || second[0].Revision != 1 || len(second[0].Instances) != 2 {
		t.Fatalf("second snapshot contaminated by adjusting the first: %+v", second)
	}
	wantSecond := map[string]InstanceView{
		"a": {ID: "a", Address: "h:1", Health: HealthHealthy, Sequence: 1, Reason: ""},
		"b": {ID: "b", Address: "h:2", Health: HealthUnhealthy, Sequence: 2, Reason: "down"},
	}
	for id, w := range wantSecond {
		if got := instanceViewByID(t, second[0], id); got != w {
			t.Fatalf("second snapshot instance %s contaminated: got %+v want %+v", id, got, w)
		}
	}

	// Adjusting the second snapshot must not reach back into the first.
	second[0].Instances[0].Sequence = 77
	if got := first[0].Instances[0]; got.Sequence != 99 || got.Health != HealthUnhealthy || got.Reason != "made up" {
		t.Fatalf("the two snapshots share storage; adjusting the second reached the first: %+v", got)
	}
}

// TestSnapshotReadsDoNotAdvanceRevisionHealthOrCursor fixes that reading and
// adjusting snapshots is observation-only: it must not consume a registration
// revision, alter accepted health records or move the per-service selection
// cursor.
func TestSnapshotReadsDoNotAdvanceRevisionHealthOrCursor(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h:1"},
		{ID: "b", Address: "h:2"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")

	// One successful selection parks the rotation cursor on a.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" {
		t.Fatalf("setup select: %+v", out)
	}

	// Several reads plus arbitrary caller-side adjustments.
	for range 3 {
		snap := r.Snapshot()
		snap[0].Revision = 999
		snap[0].Instances[0].Sequence = 999
		snap[0].Instances = append(snap[0].Instances, InstanceView{ID: "extra"})
	}

	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("snapshot reads changed the registration revision: %d", rev)
	}
	for id, seq := range map[string]int64{"a": 1, "b": 2} {
		if inst := instanceHealth(t, r, "svc", id); inst.Health != HealthHealthy || inst.Sequence != seq {
			t.Fatalf("accepted health for %s changed after snapshot reads: %+v", id, inst)
		}
	}
	// The cursor is still parked just after a, so the next healthy target is
	// b: the reads neither advanced the rotation nor restarted it at a.
	if out := selected(t, r, "svc", 1); out.InstanceID != "b" ||
		out.Address != "h:2" || out.Sequence != 2 || out.Revision != 1 {
		t.Fatalf("snapshot reads moved the selection cursor: %+v", out)
	}
}

// TestSnapshotEmptyRegistryReturnsEmptyList fixes the empty-registry
// boundary: a read returns an empty list, repeatedly.
func TestSnapshotEmptyRegistryReturnsEmptyList(t *testing.T) {
	r := NewRegistry()
	if views := r.Snapshot(); len(views) != 0 {
		t.Fatalf("empty registry should snapshot to an empty list, got %+v", views)
	}
	if again := r.Snapshot(); len(again) != 0 {
		t.Fatalf("second read of an empty registry should stay empty, got %+v", again)
	}
}

// TestSnapshotEmptyInstanceServiceKeepsRevision fixes that a registered
// service with an empty instance list still appears in snapshots at its
// current revision, both when created empty and after an emptied replacement.
func TestSnapshotEmptyInstanceServiceKeepsRevision(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, nil) // created empty at revision 1
	if views := r.Snapshot(); len(views) != 1 ||
		views[0].Service != "svc" || views[0].Revision != 1 || len(views[0].Instances) != 0 {
		t.Fatalf("empty-instance service should appear at revision 1: %+v", views)
	}

	// Populate, then replace with an empty list: the service stays in the
	// result and carries the post-replacement revision.
	registerService(t, r, "svc", 1, []Instance{{ID: "a", Address: "h:1"}})
	registerService(t, r, "svc", 2, nil)
	views := r.Snapshot()
	if len(views) != 1 || views[0].Service != "svc" || views[0].Revision != 3 {
		t.Fatalf("emptied service should appear at the new revision 3: %+v", views)
	}
	if len(views[0].Instances) != 0 {
		t.Fatalf("emptied service should carry no instances: %+v", views[0].Instances)
	}
}

// TestSnapshotSortedByServiceNameAndInstanceID fixes the read ordering:
// services ascend by name and instances ascend by id, regardless of
// registration order, and an empty-instance service sorts in normally.
func TestSnapshotSortedByServiceNameAndInstanceID(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "zeta", 0, []Instance{
		{ID: "i9", Address: "h:9"},
		{ID: "i1", Address: "h:1"},
		{ID: "i5", Address: "h:5"},
	})
	registerService(t, r, "alpha", 0, []Instance{
		{ID: "b", Address: "h:2"},
		{ID: "a", Address: "h:1"},
	})
	registerService(t, r, "mid", 0, nil)

	views := r.Snapshot()
	gotServices := make([]string, len(views))
	for i, v := range views {
		gotServices[i] = v.Service
	}
	wantServices := []string{"alpha", "mid", "zeta"}
	if len(gotServices) != len(wantServices) {
		t.Fatalf("services: got %v want %v", gotServices, wantServices)
	}
	for i := range wantServices {
		if gotServices[i] != wantServices[i] {
			t.Fatalf("services not sorted by name: got %v want %v", gotServices, wantServices)
		}
	}

	zeta := serviceViewNamed(t, views, "zeta")
	gotIDs := make([]string, len(zeta.Instances))
	for i, inst := range zeta.Instances {
		gotIDs[i] = inst.ID
	}
	wantIDs := []string{"i1", "i5", "i9"}
	for i := range wantIDs {
		if gotIDs[i] != wantIDs[i] {
			t.Fatalf("instances not sorted by id: got %v want %v", gotIDs, wantIDs)
		}
	}

	alpha := serviceViewNamed(t, views, "alpha")
	if len(alpha.Instances) != 2 || alpha.Instances[0].ID != "a" || alpha.Instances[1].ID != "b" {
		t.Fatalf("alpha instance ordering: %+v", alpha.Instances)
	}
	if mid := serviceViewNamed(t, views, "mid"); len(mid.Instances) != 0 || mid.Revision != 1 {
		t.Fatalf("empty-instance service should sort in at its revision: %+v", mid)
	}
}
