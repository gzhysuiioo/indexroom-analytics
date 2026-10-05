package indexroom

import "testing"

// TestRegistryHealthIsolationAcrossServices locks the rule that an instance id
// is meaningful only inside its own service: two services may both register
// i1 — even at the very same address — and still keep independent health
// records, sequences and reasons.
func TestRegistryHealthIsolationAcrossServices(t *testing.T) {
	r := NewRegistry()
	for _, svc := range []string{"alpha", "beta"} {
		registerService(t, r, svc, 0, []Instance{{ID: "i1", Address: "shared:8080"}})
	}

	// alpha's i1 accepts an unhealthy observation at sequence 20.
	markHealth(t, r, "alpha", "i1", 1, 20, false, "alpha down")

	// beta's i1 still accepts sequence 1: alpha's higher sequence must not
	// make it stale, and alpha's reason must not leak into beta's record.
	upd, err := r.ValidateHealth("beta", "i1", 1, 1, true, "")
	if err != nil {
		t.Fatalf("validate beta health: %v", err)
	}
	out := r.ApplyHealth(upd)
	if !out.OK || !out.Changed || out.Sequence != 1 || out.Revision != 1 {
		t.Fatalf("beta seq 1 must not be stale against alpha's 20: %+v", out)
	}
	if inst := instanceHealth(t, r, "beta", "i1"); inst.Health != HealthHealthy || inst.Sequence != 1 || inst.Reason != "" {
		t.Fatalf("beta record must be its own, got %+v", inst)
	}
	if inst := instanceHealth(t, r, "alpha", "i1"); inst.Health != HealthUnhealthy || inst.Sequence != 20 || inst.Reason != "alpha down" {
		t.Fatalf("alpha record changed by beta's observation: %+v", inst)
	}

	// Each service compares only against its own accepted record: beta turns
	// unhealthy at its own next sequence 2 while alpha sits at 20, and alpha
	// recovers at its own next sequence 21.
	markHealth(t, r, "beta", "i1", 1, 2, false, "beta down")
	markHealth(t, r, "alpha", "i1", 1, 21, true, "")
	if inst := instanceHealth(t, r, "beta", "i1"); inst.Health != HealthUnhealthy || inst.Sequence != 2 || inst.Reason != "beta down" {
		t.Fatalf("beta record: %+v", inst)
	}
	if inst := instanceHealth(t, r, "alpha", "i1"); inst.Health != HealthHealthy || inst.Sequence != 21 || inst.Reason != "" {
		t.Fatalf("alpha record: %+v", inst)
	}

	// Health updates never bump either service's registration revision.
	for _, svc := range []string{"alpha", "beta"} {
		if rev := r.RevisionOf(svc); rev != 1 {
			t.Fatalf("%s revision bumped by health updates: %d", svc, rev)
		}
	}
}

// TestRegistryHealthFailureIsolationAcrossServices locks that a stale or
// conflicting observation rejected by one service overwrites nothing there
// and touches nothing in the other service, and that the other service keeps
// accepting observations at its own next sequence right afterwards.
func TestRegistryHealthFailureIsolationAcrossServices(t *testing.T) {
	r := NewRegistry()
	for _, svc := range []string{"alpha", "beta"} {
		registerService(t, r, svc, 0, []Instance{{ID: "i1", Address: "shared:8080"}})
	}
	markHealth(t, r, "alpha", "i1", 1, 20, false, "alpha down")
	markHealth(t, r, "beta", "i1", 1, 5, true, "")

	// An older sequence for alpha is stale and reports alpha's own current
	// sequence, not anything from beta.
	upd, _ := r.ValidateHealth("alpha", "i1", 1, 19, true, "")
	out := r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeStale || out.Sequence != 20 || out.Revision != 1 {
		t.Fatalf("stale: %+v", out)
	}
	// Alpha's accepted sequence reused with different content is a conflict.
	upd, _ = r.ValidateHealth("alpha", "i1", 1, 20, true, "")
	out = r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeConflict || out.Sequence != 20 || out.Revision != 1 {
		t.Fatalf("conflict: %+v", out)
	}

	// Neither rejection overwrote alpha's record nor touched beta's.
	if inst := instanceHealth(t, r, "alpha", "i1"); inst.Health != HealthUnhealthy || inst.Sequence != 20 || inst.Reason != "alpha down" {
		t.Fatalf("alpha record mutated by rejected observations: %+v", inst)
	}
	if inst := instanceHealth(t, r, "beta", "i1"); inst.Health != HealthHealthy || inst.Sequence != 5 || inst.Reason != "" {
		t.Fatalf("beta record touched by alpha's rejections: %+v", inst)
	}

	// Beta still accepts its own next sequence immediately after alpha's
	// failures.
	markHealth(t, r, "beta", "i1", 1, 6, false, "beta down")
	if inst := instanceHealth(t, r, "beta", "i1"); inst.Health != HealthUnhealthy || inst.Sequence != 6 || inst.Reason != "beta down" {
		t.Fatalf("beta next sequence after alpha failures: %+v", inst)
	}
}

// TestRegistrySelectReflectsPerServiceHealth locks that target selection uses
// each service's own accepted health: a service whose only instance is
// unhealthy reports no_healthy while the other service still selects its own
// healthy i1 with its own address and latest sequence, and a later recovery
// of the first service changes nothing for the second.
func TestRegistrySelectReflectsPerServiceHealth(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "alpha", 0, []Instance{{ID: "i1", Address: "a1:8080"}})
	registerService(t, r, "beta", 0, []Instance{{ID: "i1", Address: "b1:8080"}})
	markHealth(t, r, "alpha", "i1", 1, 20, false, "alpha down")
	markHealth(t, r, "beta", "i1", 1, 3, true, "")

	// alpha has no healthy instance; beta selects its own i1.
	sel, _ := r.ValidateSelection("alpha", 1)
	if out := r.Select(sel); out.OK || out.Kind != OutcomeNoHealthy ||
		out.Revision != 1 || out.InstanceID != "" || out.Address != "" {
		t.Fatalf("alpha select: %+v", out)
	}
	if out := selected(t, r, "beta", 1); out.InstanceID != "i1" || out.Address != "b1:8080" || out.Sequence != 3 || out.Revision != 1 {
		t.Fatalf("beta select: %+v", out)
	}

	// alpha recovers at its own next sequence and becomes selectable again;
	// beta's record and rotation are unaffected.
	markHealth(t, r, "alpha", "i1", 1, 21, true, "")
	if out := selected(t, r, "alpha", 1); out.InstanceID != "i1" || out.Address != "a1:8080" || out.Sequence != 21 {
		t.Fatalf("alpha select after recovery: %+v", out)
	}
	if inst := instanceHealth(t, r, "beta", "i1"); inst.Health != HealthHealthy || inst.Sequence != 3 {
		t.Fatalf("beta record changed by alpha's recovery: %+v", inst)
	}
	if out := selected(t, r, "beta", 1); out.InstanceID != "i1" || out.Address != "b1:8080" || out.Sequence != 3 {
		t.Fatalf("beta select after alpha recovery: %+v", out)
	}
}
