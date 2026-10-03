package indexroom

import (
	"reflect"
	"testing"
)

// findServiceView locates one service in a snapshot, failing the test when it
// is absent.
func findServiceView(t *testing.T, views []ServiceView, service string) ServiceView {
	t.Helper()
	for _, view := range views {
		if view.Service == service {
			return view
		}
	}
	t.Fatalf("service %q not found in snapshot %+v", service, views)
	return ServiceView{}
}

// findInstanceView locates one instance in a service view, failing the test
// when it is absent.
func findInstanceView(t *testing.T, view ServiceView, id string) InstanceView {
	t.Helper()
	for _, inst := range view.Instances {
		if inst.ID == id {
			return inst
		}
	}
	t.Fatalf("instance %q not found in view %+v", id, view.Instances)
	return InstanceView{}
}

// TestSnapshotIsolationAcrossHealthUpdate locks that a saved snapshot keeps
// the health state accepted at read time: a later observation only shows up in
// snapshots taken after it.
func TestSnapshotIsolationAcrossHealthUpdate(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 5, false, "connection refused")
	markHealth(t, r, "svc", "b", 1, 3, true, "")

	before := r.Snapshot()

	// The instance recovers: unhealthy at sequence 5 becomes healthy at 6.
	markHealth(t, r, "svc", "a", 1, 6, true, "")

	// The saved snapshot still reports the state accepted when it was read.
	oldView := findServiceView(t, before, "svc")
	if oldView.Revision != 1 {
		t.Fatalf("saved snapshot revision changed: %+v", oldView)
	}
	if inst := findInstanceView(t, oldView, "a"); inst.Health != HealthUnhealthy ||
		inst.Sequence != 5 || inst.Reason != "connection refused" {
		t.Fatalf("saved snapshot must keep the old observation: %+v", inst)
	}
	if inst := findInstanceView(t, oldView, "b"); inst.Health != HealthHealthy || inst.Sequence != 3 {
		t.Fatalf("saved snapshot must keep b's observation: %+v", inst)
	}

	// A fresh snapshot reflects the update: healthy, sequence 6, reason cleared.
	newView := findServiceView(t, r.Snapshot(), "svc")
	if inst := findInstanceView(t, newView, "a"); inst.Health != HealthHealthy ||
		inst.Sequence != 6 || inst.Reason != "" {
		t.Fatalf("new snapshot should show the accepted update: %+v", inst)
	}
}

// TestSnapshotIsolationAcrossReplacement locks that a saved snapshot keeps the
// revision, members and addresses from read time even after the instance list
// is replaced, while a fresh snapshot follows the replacement rules.
func TestSnapshotIsolationAcrossReplacement(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"},
		{ID: "b", Address: "h2:2"},
		{ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 4, true, "")
	markHealth(t, r, "svc", "b", 1, 7, false, "flapping")

	before := r.Snapshot()

	// Replace: a keeps id and address, b keeps its id but moves address, c is
	// dropped and d joins.
	registerService(t, r, "svc", 1, []Instance{
		{ID: "a", Address: "h1:1"},
		{ID: "b", Address: "h9:9"},
		{ID: "d", Address: "h4:4"},
	})

	// The saved snapshot is untouched: revision 1, the original three members
	// with their original addresses and observations.
	oldView := findServiceView(t, before, "svc")
	if oldView.Revision != 1 || len(oldView.Instances) != 3 {
		t.Fatalf("saved snapshot mutated by replacement: %+v", oldView)
	}
	if inst := findInstanceView(t, oldView, "b"); inst.Address != "h2:2" ||
		inst.Health != HealthUnhealthy || inst.Sequence != 7 || inst.Reason != "flapping" {
		t.Fatalf("saved snapshot must keep b's old address and observation: %+v", inst)
	}
	if inst := findInstanceView(t, oldView, "c"); inst.Address != "h3:3" {
		t.Fatalf("saved snapshot must keep the removed member c: %+v", inst)
	}

	// The fresh snapshot shows revision 2 and the replacement result: a kept
	// its health record, b (same id, new address) and d start unknown/0.
	newView := findServiceView(t, r.Snapshot(), "svc")
	if newView.Revision != 2 || len(newView.Instances) != 3 {
		t.Fatalf("new snapshot should show the replacement: %+v", newView)
	}
	if inst := findInstanceView(t, newView, "a"); inst.Health != HealthHealthy || inst.Sequence != 4 {
		t.Fatalf("a kept id and address, so its health record survives: %+v", inst)
	}
	for _, id := range []string{"b", "d"} {
		if inst := findInstanceView(t, newView, id); inst.Health != HealthUnknown ||
			inst.Sequence != 0 || inst.Reason != "" {
			t.Fatalf("%s should start unknown/0/empty after the replacement: %+v", id, inst)
		}
	}
	if inst := findInstanceView(t, newView, "b"); inst.Address != "h9:9" {
		t.Fatalf("new snapshot should show b's new address: %+v", inst)
	}
}

// TestSnapshotMutationDoesNotLeakIntoRegistry locks that a caller may reshape
// the returned snapshot for display purposes — renaming the service, editing
// addresses, health, sequences, reasons and the member list — without any of
// it leaking back into the registry.
func TestSnapshotMutationDoesNotLeakIntoRegistry(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 11, true, "")
	markHealth(t, r, "svc", "b", 1, 22, false, "down")

	views := r.Snapshot()
	if len(views) != 1 || len(views[0].Instances) != 2 {
		t.Fatalf("unexpected snapshot: %+v", views)
	}

	// Deface every mutable part of the returned value.
	views[0].Service = "forged"
	views[0].Revision = 99
	views[0].Instances[0].ID = "forged-id"
	views[0].Instances[0].Address = "forged:1"
	views[0].Instances[0].Health = HealthUnhealthy
	views[0].Instances[0].Sequence = 777
	views[0].Instances[0].Reason = "forged reason"
	views[0].Instances[1].Health = HealthHealthy
	views[0].Instances[1].Sequence = 888
	views[0].Instances[1].Reason = "cleared?"
	views[0].Instances = append(views[0].Instances, InstanceView{ID: "ghost", Address: "ghost:1", Health: HealthHealthy, Sequence: 999})
	views[0].Instances = views[0].Instances[:1]
	views = append(views, ServiceView{Service: "extra", Revision: 5})

	// The registry still answers with the real accepted state.
	fresh := r.Snapshot()
	if len(fresh) != 1 {
		t.Fatalf("service list changed by caller mutation: %+v", fresh)
	}
	view := fresh[0]
	if view.Service != "svc" || view.Revision != 1 || len(view.Instances) != 2 {
		t.Fatalf("service view changed by caller mutation: %+v", view)
	}
	if inst := findInstanceView(t, view, "a"); inst.Address != "h1:1" ||
		inst.Health != HealthHealthy || inst.Sequence != 11 || inst.Reason != "" {
		t.Fatalf("a's accepted state changed by caller mutation: %+v", inst)
	}
	if inst := findInstanceView(t, view, "b"); inst.Address != "h2:2" ||
		inst.Health != HealthUnhealthy || inst.Sequence != 22 || inst.Reason != "down" {
		t.Fatalf("b's accepted state changed by caller mutation: %+v", inst)
	}
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("revision changed by caller mutation: %d", rev)
	}

	// Selection also sees the real state: a is the only healthy target and
	// carries its real address and sequence, never the forged ones.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" || out.Address != "h1:1" || out.Sequence != 11 {
		t.Fatalf("select after caller mutation: %+v", out)
	}
}

// TestConsecutiveSnapshotsAreIndependent locks that two snapshots taken back
// to back share no memory: editing one leaves the other — and the registry —
// intact.
func TestConsecutiveSnapshotsAreIndependent(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 11, true, "")
	markHealth(t, r, "svc", "b", 1, 22, false, "down")

	first := r.Snapshot()
	second := r.Snapshot()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("consecutive snapshots of unchanged state differ:\nfirst %+v\nsecond %+v", first, second)
	}

	// Rewrite the first snapshot completely.
	first[0].Service = "forged"
	first[0].Revision = 42
	first[0].Instances[0] = InstanceView{ID: "x", Address: "x:1", Health: HealthUnhealthy, Sequence: 9, Reason: "x"}
	first[0].Instances = first[0].Instances[:1]

	// The second snapshot is untouched and still matches a fresh read.
	fresh := r.Snapshot()
	if !reflect.DeepEqual(second, fresh) {
		t.Fatalf("mutating one snapshot polluted the other:\nsecond %+v\nfresh %+v", second, fresh)
	}
	view := findServiceView(t, second, "svc")
	if view.Revision != 1 || len(view.Instances) != 2 {
		t.Fatalf("second snapshot changed: %+v", view)
	}
	if inst := findInstanceView(t, view, "b"); inst.Health != HealthUnhealthy || inst.Sequence != 22 || inst.Reason != "down" {
		t.Fatalf("second snapshot lost b's observation: %+v", inst)
	}
}

// TestSnapshotReadAndMutationHaveNoSideEffects locks that reading snapshots —
// and editing what was read — neither consumes revisions, nor rewrites
// accepted health records, nor moves the rotation cursor.
func TestSnapshotReadAndMutationHaveNoSideEffects(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 11, true, "")
	markHealth(t, r, "svc", "b", 1, 22, true, "")

	// One successful selection lands the cursor on a.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" {
		t.Fatalf("first select: %+v", out)
	}

	// Read snapshots repeatedly and deface each one.
	for i := 0; i < 3; i++ {
		views := r.Snapshot()
		views[0].Revision = 100 + i
		views[0].Instances[0].Sequence = 500
		views[0].Instances[0].Health = HealthUnhealthy
	}

	// No revision was consumed and the accepted health records are intact.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("snapshot reads consumed a revision: %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthHealthy || inst.Sequence != 11 {
		t.Fatalf("snapshot reads rewrote a's health record: %+v", inst)
	}

	// The cursor never moved: the next selection continues just after a.
	if out := selected(t, r, "svc", 1); out.InstanceID != "b" || out.Address != "h2:2" || out.Sequence != 22 {
		t.Fatalf("snapshot reads advanced the rotation: %+v", out)
	}
}

// TestSnapshotBoundariesAndOrdering locks the snapshot's edge behavior: an
// empty registry yields an empty list, a registered service with no instances
// still appears with its revision, and results are sorted by service name and
// instance id.
func TestSnapshotBoundariesAndOrdering(t *testing.T) {
	r := NewRegistry()
	if views := r.Snapshot(); len(views) != 0 {
		t.Fatalf("empty registry should return an empty list: %+v", views)
	}

	// Registered out of order, with instances out of order; "empty" keeps its
	// place even after its instance list is replaced by an empty one.
	registerService(t, r, "zeta", 0, []Instance{
		{ID: "i2", Address: "h2:2"},
		{ID: "i1", Address: "h1:1"},
	})
	registerService(t, r, "empty", 0, []Instance{{ID: "x", Address: "h:1"}})
	registerService(t, r, "empty", 1, nil)
	registerService(t, r, "alpha", 0, []Instance{{ID: "a", Address: "h:1"}})

	views := r.Snapshot()
	if len(views) != 3 {
		t.Fatalf("services: %+v", views)
	}
	names := []string{views[0].Service, views[1].Service, views[2].Service}
	if want := []string{"alpha", "empty", "zeta"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("services not sorted by name: got %v want %v", names, want)
	}

	// The empty-list service is present with its current revision.
	empty := views[1]
	if empty.Revision != 2 || len(empty.Instances) != 0 {
		t.Fatalf("empty service should appear at revision 2 with no instances: %+v", empty)
	}

	// Instances are sorted by id.
	zeta := views[2]
	if len(zeta.Instances) != 2 || zeta.Instances[0].ID != "i1" || zeta.Instances[1].ID != "i2" {
		t.Fatalf("instances not sorted by id: %+v", zeta.Instances)
	}
}
