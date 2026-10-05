package indexroom

import "testing"

// This file is the domain-level regression guard for health-record isolation
// between services that reuse the same instance identifier. An instance id is
// only meaningful inside its own service: two services may both register "i1"
// — even with the very same address — and each must keep its own health state,
// sequence and reason. These tests drive the public Validate/Apply/Select and
// Snapshot APIs to lock that isolation:
//
//   - interleaved observations compare only against the owning service's
//     accepted record: one service's i1 at sequence 20 never makes the other
//     service's i1 sequence 1 stale, and no reason leaks across;
//   - stale and same-sequence conflicting reports fail against the targeted
//     service's own current sequence and overwrite neither service's record,
//     while a later valid observation in the same service still lands;
//   - target selection reflects each service's own accepted health: the
//     all-unhealthy service answers no_healthy while the other still selects
//     its own healthy instance with its own address and latest sequence, and a
//     recovery in one service never changes the other's state;
//   - health observations never bump either service's registration revision.

// reportHealth validates and applies one health observation, returning the raw
// outcome so both accepted and rejected reports can be asserted.
func reportHealth(t *testing.T, r *Registry, service, id string, revision int64, sequence int64, healthy bool, reason string) HealthOutcome {
	t.Helper()
	upd, err := r.ValidateHealth(service, id, revision, sequence, healthy, reason)
	if err != nil {
		t.Fatalf("validate health %s/%s: %v", service, id, err)
	}
	return r.ApplyHealth(upd)
}

// trySelect validates and runs one plain selection, returning the raw outcome
// so both successful and failed selections can be asserted.
func trySelect(t *testing.T, r *Registry, service string, revision int64) SelectOutcome {
	t.Helper()
	sel, err := r.ValidateSelection(service, revision)
	if err != nil {
		t.Fatalf("validate select %s: %v", service, err)
	}
	return r.Select(sel)
}

// assertHealth locks one accepted observation's result: success, the change
// flag, the owning service's revision and the instance's new sequence.
func assertHealth(t *testing.T, out HealthOutcome, revision int, sequence int64) {
	t.Helper()
	if !out.OK || !out.Changed || out.Revision != revision || out.Sequence != sequence {
		t.Fatalf("health outcome = %+v, want ok changed revision %d sequence %d", out, revision, sequence)
	}
}

// TestRegistryHealthSameIDAndAddressIsolatedAcrossServices interleaves
// observations for two services that both register an instance "i1" at the
// SAME address. Every observation must be judged against the owning service's
// own accepted record only.
func TestRegistryHealthSameIDAndAddressIsolatedAcrossServices(t *testing.T) {
	r := NewRegistry()
	for _, svc := range []string{"svc-a", "svc-b"} {
		registerService(t, r, svc, 0, []Instance{{ID: "i1", Address: "h1:8080"}})
	}

	// svc-a's i1 accepts an unhealthy observation at sequence 20; the trimmed
	// reason is stored on svc-a's record only.
	assertHealth(t, reportHealth(t, r, "svc-a", "i1", 1, 20, false, " 心跳超时 "), 1, 20)

	// svc-b's i1 still accepts sequence 1: svc-a's higher sequence must not
	// make it stale, and svc-a's reason must not leak into svc-b's record.
	assertHealth(t, reportHealth(t, r, "svc-b", "i1", 1, 1, true, ""), 1, 1)
	if inst := instanceHealth(t, r, "svc-b", "i1"); inst.Health != HealthHealthy || inst.Sequence != 1 || inst.Reason != "" {
		t.Fatalf("svc-b/i1 should be healthy at its own sequence 1 with no reason, got %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc-a", "i1"); inst.Health != HealthUnhealthy || inst.Sequence != 20 || inst.Reason != "心跳超时" {
		t.Fatalf("svc-a/i1 should be unhealthy at sequence 20 with its trimmed reason, got %+v", inst)
	}

	// A stale report to svc-a is judged against svc-a's own current sequence
	// 20 — not svc-b's 1 — and reports that sequence.
	if out := reportHealth(t, r, "svc-a", "i1", 1, 5, true, ""); out.OK || out.Kind != OutcomeStale ||
		out.Sequence != 20 || out.Revision != 1 {
		t.Fatalf("stale report should state svc-a's own current sequence 20, got %+v", out)
	}
	// The same sequence 20 with different health content conflicts against
	// svc-a's record; svc-b's identical-id record is not consulted.
	if out := reportHealth(t, r, "svc-a", "i1", 1, 20, false, "别的理由"); out.OK || out.Kind != OutcomeConflict ||
		out.Sequence != 20 || out.Revision != 1 {
		t.Fatalf("conflicting report should state svc-a's own current sequence 20, got %+v", out)
	}
	// Neither rejection overwrote svc-a's record nor touched svc-b's.
	if inst := instanceHealth(t, r, "svc-a", "i1"); inst.Health != HealthUnhealthy || inst.Sequence != 20 || inst.Reason != "心跳超时" {
		t.Fatalf("rejected reports must not overwrite svc-a/i1, got %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc-b", "i1"); inst.Health != HealthHealthy || inst.Sequence != 1 || inst.Reason != "" {
		t.Fatalf("svc-a's rejected reports must not touch svc-b/i1, got %+v", inst)
	}

	// Immediately after svc-a's failures, svc-b's next observation takes
	// effect at its OWN next sequence 2 — svc-a's accepted 20 is irrelevant.
	assertHealth(t, reportHealth(t, r, "svc-b", "i1", 1, 2, false, "磁盘故障"), 1, 2)
	// And svc-a recovers at its own next sequence 21 while svc-b's record
	// stays exactly where svc-b left it.
	assertHealth(t, reportHealth(t, r, "svc-a", "i1", 1, 21, true, ""), 1, 21)

	// The snapshot shows two fully independent records under the same id and
	// address; neither health report bumped a registration revision.
	views := r.Snapshot()
	if len(views) != 2 {
		t.Fatalf("services: %+v", views)
	}
	if views[0].Service != "svc-a" || views[0].Revision != 1 || views[1].Service != "svc-b" || views[1].Revision != 1 {
		t.Fatalf("health reports must not bump revisions: %+v", views)
	}
	wantA := InstanceView{ID: "i1", Address: "h1:8080", Health: HealthHealthy, Sequence: 21}
	if len(views[0].Instances) != 1 || views[0].Instances[0] != wantA {
		t.Fatalf("svc-a/i1: got %+v want %+v", views[0].Instances, wantA)
	}
	wantB := InstanceView{ID: "i1", Address: "h1:8080", Health: HealthUnhealthy, Sequence: 2, Reason: "磁盘故障"}
	if len(views[1].Instances) != 1 || views[1].Instances[0] != wantB {
		t.Fatalf("svc-b/i1: got %+v want %+v", views[1].Instances, wantB)
	}
}

// TestRegistrySelectSameIDReflectsOwnServiceHealth locks selection isolation:
// each service's select sees only its own accepted health records, even when
// both services name their instance "i1" at the same address.
func TestRegistrySelectSameIDReflectsOwnServiceHealth(t *testing.T) {
	r := NewRegistry()
	for _, svc := range []string{"svc-a", "svc-b"} {
		registerService(t, r, svc, 0, []Instance{{ID: "i1", Address: "h1:8080"}})
	}
	markHealth(t, r, "svc-a", "i1", 1, 7, false, "连接失败")
	markHealth(t, r, "svc-b", "i1", 1, 1, true, "")

	// svc-a has no healthy instance of its own: no_healthy, with the current
	// revision and no fabricated target — svc-b's healthy i1 does not count.
	if out := trySelect(t, r, "svc-a", 1); out.OK || out.Kind != OutcomeNoHealthy ||
		out.Revision != 1 || out.InstanceID != "" || out.Address != "" {
		t.Fatalf("svc-a select should be no_healthy, got %+v", out)
	}
	// svc-b selects its own healthy i1 with its own address and its own
	// latest accepted sequence 1 — not svc-a's 7.
	assertPick(t, trySelect(t, r, "svc-b", 1), "i1", "h1:8080", 1)

	// svc-a recovers at its own next sequence and becomes selectable again…
	markHealth(t, r, "svc-a", "i1", 1, 8, true, "")
	assertPick(t, trySelect(t, r, "svc-a", 1), "i1", "h1:8080", 8)
	// …without changing svc-b's record or its next selection.
	assertPick(t, trySelect(t, r, "svc-b", 1), "i1", "h1:8080", 1)
	if inst := instanceHealth(t, r, "svc-b", "i1"); inst.Health != HealthHealthy || inst.Sequence != 1 || inst.Reason != "" {
		t.Fatalf("svc-a's recovery must not affect svc-b/i1, got %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc-a", "i1"); inst.Health != HealthHealthy || inst.Sequence != 8 || inst.Reason != "" {
		t.Fatalf("svc-a/i1 should be healthy at sequence 8 with the reason cleared, got %+v", inst)
	}
}

// TestRegistryHealthRejectedReportLeavesBothServicesSelectable is the
// selection-facing side of failure isolation: a stale and a conflicting
// "unhealthy" report aimed at one service's i1 must not knock that instance
// out of its own service's candidate set, and must never reach the other
// service's identical-id instance.
func TestRegistryHealthRejectedReportLeavesBothServicesSelectable(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc-a", 0, []Instance{{ID: "i1", Address: "h1:8080"}})
	registerService(t, r, "svc-b", 0, []Instance{{ID: "i1", Address: "h9:9090"}})
	markHealth(t, r, "svc-a", "i1", 1, 10, true, "")
	markHealth(t, r, "svc-b", "i1", 1, 3, true, "")

	// svc-a's i1 is healthy at sequence 10. A stale "unhealthy" report and a
	// same-sequence conflicting one are both rejected against svc-a's own
	// record; svc-b's sequence 3 plays no part in either judgement.
	if out := reportHealth(t, r, "svc-a", "i1", 1, 4, false, "误报重发"); out.OK || out.Kind != OutcomeStale ||
		out.Sequence != 10 || out.Revision != 1 {
		t.Fatalf("stale report: %+v", out)
	}
	if out := reportHealth(t, r, "svc-a", "i1", 1, 10, false, "误报重发"); out.OK || out.Kind != OutcomeConflict ||
		out.Sequence != 10 || out.Revision != 1 {
		t.Fatalf("conflicting report: %+v", out)
	}

	// Both services still select their own healthy i1 with its own address
	// and its own accepted sequence.
	assertPick(t, trySelect(t, r, "svc-a", 1), "i1", "h1:8080", 10)
	assertPick(t, trySelect(t, r, "svc-b", 1), "i1", "h9:9090", 3)

	// A later valid observation in svc-b lands at its own next sequence,
	// unaffected by svc-a's accepted 10 or its rejected reports.
	assertHealth(t, reportHealth(t, r, "svc-b", "i1", 1, 4, false, "磁盘故障"), 1, 4)
	if out := trySelect(t, r, "svc-b", 1); out.OK || out.Kind != OutcomeNoHealthy || out.InstanceID != "" || out.Address != "" {
		t.Fatalf("svc-b should now be no_healthy on its own record, got %+v", out)
	}
	// svc-a is untouched by svc-b's update.
	assertPick(t, trySelect(t, r, "svc-a", 1), "i1", "h1:8080", 10)
	if inst := instanceHealth(t, r, "svc-a", "i1"); inst.Health != HealthHealthy || inst.Sequence != 10 || inst.Reason != "" {
		t.Fatalf("svc-a/i1 should keep its accepted record, got %+v", inst)
	}
}
