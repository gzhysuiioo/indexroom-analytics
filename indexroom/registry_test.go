package indexroom

import (
	"strings"
	"testing"
)

func validInstances() []Instance {
	return []Instance{
		{ID: "i2", Address: "h2.example:8080"},
		{ID: "i1", Address: "10.0.0.1:443"},
	}
}

func TestRegistryCreateAndUpdate(t *testing.T) {
	r := NewRegistry()

	reg, err := r.ValidateRegistration("svc", 0, validInstances())
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := r.Apply(reg)
	if !out.OK || !out.Changed || out.Revision != 1 {
		t.Fatalf("create: got %+v", out)
	}

	// Same content in different order: success, no revision bump.
	reg, err = r.ValidateRegistration("svc", 1, []Instance{
		{ID: "i1", Address: "10.0.0.1:443"},
		{ID: "i2", Address: "h2.example:8080"},
	})
	if err != nil {
		t.Fatalf("validate unchanged: %v", err)
	}
	out = r.Apply(reg)
	if !out.OK || out.Changed || out.Revision != 1 {
		t.Fatalf("unchanged: got %+v", out)
	}

	// Content change bumps revision and fully replaces the list.
	reg, err = r.ValidateRegistration("svc", 1, []Instance{
		{ID: "i1", Address: "10.0.0.2:443"},
		{ID: "i3", Address: "[2001:db8::1]:9090"},
	})
	if err != nil {
		t.Fatalf("validate changed: %v", err)
	}
	out = r.Apply(reg)
	if !out.OK || !out.Changed || out.Revision != 2 {
		t.Fatalf("changed: got %+v", out)
	}

	views := r.Snapshot()
	if len(views) != 1 {
		t.Fatalf("services: %+v", views)
	}
	got := views[0]
	if got.Service != "svc" || got.Revision != 2 {
		t.Fatalf("service: %+v", got)
	}
	want := []InstanceView{
		{ID: "i1", Address: "10.0.0.2:443", Health: HealthUnknown},
		{ID: "i3", Address: "[2001:db8::1]:9090", Health: HealthUnknown},
	}
	if len(got.Instances) != len(want) {
		t.Fatalf("instances: %+v", got.Instances)
	}
	for i := range want {
		if got.Instances[i] != want[i] {
			t.Fatalf("instance %d: got %+v want %+v", i, got.Instances[i], want[i])
		}
	}
}

func TestRegistryRevisionConflict(t *testing.T) {
	r := NewRegistry()
	reg, _ := r.ValidateRegistration("svc", 0, validInstances())
	r.Apply(reg)

	// New service must be created with expectedRevision 0.
	reg, _ = r.ValidateRegistration("new", 2, nil)
	out := r.Apply(reg)
	if out.OK || out.Kind != OutcomeConflict || out.Actual != 0 || out.Expected != 2 || out.Revision != 0 {
		t.Fatalf("new conflict: %+v", out)
	}

	// Existing service requires the current revision.
	reg, _ = r.ValidateRegistration("svc", 5, []Instance{{ID: "i9", Address: "h:1"}})
	out = r.Apply(reg)
	if out.OK || out.Kind != OutcomeConflict || out.Actual != 1 || out.Expected != 5 || out.Revision != 1 {
		t.Fatalf("existing conflict: %+v", out)
	}

	// The failed registration must not have changed the list.
	views := r.Snapshot()
	if len(views) != 1 || views[0].Revision != 1 || len(views[0].Instances) != 2 {
		t.Fatalf("list mutated after conflict: %+v", views)
	}
}

func TestRegistryEmptyListKeepsService(t *testing.T) {
	r := NewRegistry()
	reg, _ := r.ValidateRegistration("svc", 0, validInstances())
	r.Apply(reg)

	reg, _ = r.ValidateRegistration("svc", 1, nil)
	out := r.Apply(reg)
	if !out.OK || !out.Changed || out.Revision != 2 {
		t.Fatalf("empty update: %+v", out)
	}
	views := r.Snapshot()
	if len(views) != 1 || len(views[0].Instances) != 0 {
		t.Fatalf("service should remain with empty instances: %+v", views)
	}

	// Re-registering empty list is unchanged.
	reg, _ = r.ValidateRegistration("svc", 2, nil)
	out = r.Apply(reg)
	if !out.OK || out.Changed || out.Revision != 2 {
		t.Fatalf("empty unchanged: %+v", out)
	}
}

func TestRegistryValidation(t *testing.T) {
	r := NewRegistry()
	cases := []struct {
		name      string
		service   string
		revision  int64
		instances []Instance
	}{
		{"empty service", "   ", 0, nil},
		{"negative revision", "svc", -1, nil},
		{"empty instance id", "svc", 0, []Instance{{ID: "  ", Address: "h:1"}}},
		{"duplicate ids", "svc", 0, []Instance{{ID: "a", Address: "h:1"}, {ID: "a", Address: "h:2"}}},
		{"empty address", "svc", 0, []Instance{{ID: "a", Address: "  "}}},
		{"missing port", "svc", 0, []Instance{{ID: "a", Address: "host"}}},
		{"empty host", "svc", 0, []Instance{{ID: "a", Address: ":8080"}}},
		{"non-numeric port", "svc", 0, []Instance{{ID: "a", Address: "host:http"}}},
		{"port zero", "svc", 0, []Instance{{ID: "a", Address: "host:0"}}},
		{"port too high", "svc", 0, []Instance{{ID: "a", Address: "host:65536"}}},
		{"port with sign", "svc", 0, []Instance{{ID: "a", Address: "host:+80"}}},
		{"port with spaces", "svc", 0, []Instance{{ID: "a", Address: "host: 80"}}},
		{"unbracketed ipv6", "svc", 0, []Instance{{ID: "a", Address: "::1:8080"}}},
		{"bad bracketed ipv6", "svc", 0, []Instance{{ID: "a", Address: "[zzzz]:8080"}}},
		{"bracketed without port", "svc", 0, []Instance{{ID: "a", Address: "[::1]"}}},
		{"bad domain char", "svc", 0, []Instance{{ID: "a", Address: "my_host:80"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.ValidateRegistration(tc.service, tc.revision, tc.instances); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}

	// Whitespace is trimmed and valid addresses are accepted.
	reg, err := r.ValidateRegistration("  svc  ", 0, []Instance{
		{ID: "  a  ", Address: "  host.example:8080  "},
		{ID: "b", Address: "127.0.0.1:443"},
		{ID: "c", Address: "[::1]:8080"},
	})
	if err != nil {
		t.Fatalf("valid addresses rejected: %v", err)
	}
	if reg.Service != "svc" || reg.Instances[0].ID != "a" || reg.Instances[0].Address != "host.example:8080" {
		t.Fatalf("trim mismatch: %+v", reg)
	}

	// A failed validation must not consume a revision or create a service.
	if _, err := r.ValidateRegistration("svc", 0, []Instance{{ID: "a", Address: "bad"}}); err == nil {
		t.Fatalf("expected error")
	}
	if rev := r.RevisionOf("svc"); rev != 0 {
		t.Fatalf("revision consumed: %d", rev)
	}
}

func TestRegistrySameIDAcrossServices(t *testing.T) {
	r := NewRegistry()
	for _, svc := range []string{"a", "b"} {
		reg, _ := r.ValidateRegistration(svc, 0, []Instance{{ID: "x", Address: "h:1"}})
		if out := r.Apply(reg); !out.OK || out.Revision != 1 {
			t.Fatalf("service %s: %+v", svc, out)
		}
	}
	views := r.Snapshot()
	if len(views) != 2 {
		t.Fatalf("services: %+v", views)
	}
}

func TestRegistryInvalidThenConflictOrder(t *testing.T) {
	// Content validity is checked before the revision: an invalid request with a
	// wrong revision reports invalid, not conflict.
	r := NewRegistry()
	reg, _ := r.ValidateRegistration("svc", 0, validInstances())
	r.Apply(reg)

	reg, err := r.ValidateRegistration("svc", 99, []Instance{{ID: "", Address: "h:1"}})
	if err == nil {
		t.Fatalf("expected validation error")
	}
	out := r.Apply(Registration{Service: "svc", Revision: 99})
	if out.Kind != OutcomeConflict {
		t.Fatalf("expected conflict after valid content, got %+v", out)
	}
}

// registerService is a helper that creates a service with the given instances.
func registerService(t *testing.T, r *Registry, service string, rev int64, instances []Instance) {
	t.Helper()
	reg, err := r.ValidateRegistration(service, rev, instances)
	if err != nil {
		t.Fatalf("validate %s: %v", service, err)
	}
	out := r.Apply(reg)
	if !out.OK {
		t.Fatalf("apply %s: %+v", service, out)
	}
}

func instanceHealth(t *testing.T, r *Registry, service, id string) InstanceView {
	t.Helper()
	for _, view := range r.Snapshot() {
		if view.Service != service {
			continue
		}
		for _, inst := range view.Instances {
			if inst.ID == id {
				return inst
			}
		}
		t.Fatalf("instance %q not found in service %q", id, service)
	}
	t.Fatalf("service %q not found", service)
	return InstanceView{}
}

func TestRegistryHealthInitialState(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})

	inst := instanceHealth(t, r, "svc", "a")
	if inst.Health != HealthUnknown || inst.Sequence != 0 || inst.Reason != "" {
		t.Fatalf("fresh instance should be unknown/0/empty, got %+v", inst)
	}
}

func TestRegistryHealthUpdates(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})

	// First observation: healthy.
	upd, err := r.ValidateHealth("svc", "a", 1, 1, true, "")
	if err != nil {
		t.Fatalf("validate healthy: %v", err)
	}
	out := r.ApplyHealth(upd)
	if !out.OK || !out.Changed || out.Sequence != 1 || out.Revision != 1 {
		t.Fatalf("healthy: %+v", out)
	}
	inst := instanceHealth(t, r, "svc", "a")
	if inst.Health != HealthHealthy || inst.Sequence != 1 || inst.Reason != "" {
		t.Fatalf("after healthy: %+v", inst)
	}

	// Healthy with a reason still clears it.
	upd, err = r.ValidateHealth("svc", "a", 1, 2, true, "  should be cleared  ")
	if err != nil {
		t.Fatalf("validate healthy with reason: %v", err)
	}
	out = r.ApplyHealth(upd)
	if !out.OK || !out.Changed || out.Sequence != 2 {
		t.Fatalf("healthy seq 2: %+v", out)
	}
	inst = instanceHealth(t, r, "svc", "a")
	if inst.Health != HealthHealthy || inst.Reason != "" {
		t.Fatalf("reason should be cleared: %+v", inst)
	}

	// Unhealthy with a reason.
	upd, err = r.ValidateHealth("svc", "a", 1, 3, false, "  connection refused  ")
	if err != nil {
		t.Fatalf("validate unhealthy: %v", err)
	}
	out = r.ApplyHealth(upd)
	if !out.OK || !out.Changed || out.Sequence != 3 {
		t.Fatalf("unhealthy: %+v", out)
	}
	inst = instanceHealth(t, r, "svc", "a")
	if inst.Health != HealthUnhealthy || inst.Sequence != 3 || inst.Reason != "connection refused" {
		t.Fatalf("after unhealthy: %+v", inst)
	}

	// A health update must not bump the registration revision.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("registration revision bumped by health update: %d", rev)
	}
}

func TestRegistryHealthStaleAndSameSequence(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})

	upd, _ := r.ValidateHealth("svc", "a", 1, 5, false, "down")
	r.ApplyHealth(upd)

	// Older sequence is stale and reports the current sequence.
	upd, _ = r.ValidateHealth("svc", "a", 1, 4, true, "")
	out := r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeStale || out.Sequence != 5 {
		t.Fatalf("stale: %+v", out)
	}
	// State unchanged.
	inst := instanceHealth(t, r, "svc", "a")
	if inst.Health != HealthUnhealthy || inst.Sequence != 5 {
		t.Fatalf("state changed after stale: %+v", inst)
	}

	// Same sequence with identical normalized content succeeds without change.
	upd, _ = r.ValidateHealth("svc", "a", 1, 5, false, "  down  ")
	out = r.ApplyHealth(upd)
	if !out.OK || out.Changed || out.Sequence != 5 {
		t.Fatalf("same idempotent: %+v", out)
	}

	// Same sequence with different status conflicts.
	upd, _ = r.ValidateHealth("svc", "a", 1, 5, true, "")
	out = r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeConflict || out.Sequence != 5 {
		t.Fatalf("same seq different status: %+v", out)
	}

	// Same sequence with a different reason conflicts.
	upd, _ = r.ValidateHealth("svc", "a", 1, 5, false, "different reason")
	out = r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeConflict {
		t.Fatalf("same seq different reason: %+v", out)
	}

	// State must still be the original observation.
	inst = instanceHealth(t, r, "svc", "a")
	if inst.Health != HealthUnhealthy || inst.Sequence != 5 || inst.Reason != "down" {
		t.Fatalf("state mutated after failed same-seq: %+v", inst)
	}
}

func TestRegistryHealthNotFound(t *testing.T) {
	r := NewRegistry()

	// Service missing with expectedRevision 0: not_found.
	upd, err := r.ValidateHealth("svc", "a", 0, 1, true, "")
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeNotFound {
		t.Fatalf("service missing: %+v", out)
	}

	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})

	// Instance missing with a matching revision: not_found.
	upd, _ = r.ValidateHealth("svc", "ghost", 1, 1, true, "")
	out = r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeNotFound || out.Revision != 1 {
		t.Fatalf("instance missing: %+v", out)
	}
}

func TestRegistryHealthRevisionConflict(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})

	// Wrong revision conflicts, even though the instance exists.
	upd, _ := r.ValidateHealth("svc", "a", 2, 1, true, "")
	out := r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeConflict || out.Expected != 2 || out.Actual != 1 || out.Revision != 1 {
		t.Fatalf("revision conflict: %+v", out)
	}

	// Service missing with a non-zero expected revision: conflict, not not_found.
	upd, _ = r.ValidateHealth("other", "a", 3, 1, true, "")
	out = r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeConflict || out.Expected != 3 || out.Actual != 0 {
		t.Fatalf("missing service wrong revision: %+v", out)
	}
}

func TestRegistryHealthValidation(t *testing.T) {
	r := NewRegistry()
	cases := []struct {
		name       string
		service    string
		instanceID string
		revision   int64
		sequence   int64
		healthy    bool
		reason     string
	}{
		{"empty service", "   ", "a", 0, 1, true, ""},
		{"empty instance id", "svc", "  ", 0, 1, true, ""},
		{"negative revision", "svc", "a", -1, 1, true, ""},
		{"zero sequence", "svc", "a", 0, 0, true, ""},
		{"negative sequence", "svc", "a", 0, -1, true, ""},
		{"unhealthy empty reason", "svc", "a", 0, 1, false, "   "},
		{"unhealthy missing reason", "svc", "a", 0, 1, false, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.ValidateHealth(tc.service, tc.instanceID, tc.revision, tc.sequence, tc.healthy, tc.reason); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}

	// Whitespace is trimmed; healthy with a reason is accepted and cleared.
	upd, err := r.ValidateHealth("  svc  ", "  a  ", 0, 1, true, "  r  ")
	if err != nil {
		t.Fatalf("valid rejected: %v", err)
	}
	if upd.Service != "svc" || upd.InstanceID != "a" || upd.Reason != "" {
		t.Fatalf("trim mismatch: %+v", upd)
	}

	// Unhealthy reason is trimmed.
	upd, err = r.ValidateHealth("svc", "a", 0, 1, false, "  down  ")
	if err != nil || upd.Reason != "down" {
		t.Fatalf("unhealthy reason: %+v err=%v", upd, err)
	}
}

func TestRegistryHealthPreservedAcrossRegistration(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}})

	// Observe both instances.
	upd, _ := r.ValidateHealth("svc", "a", 1, 1, true, "")
	r.ApplyHealth(upd)
	upd, _ = r.ValidateHealth("svc", "b", 1, 2, false, "down")
	r.ApplyHealth(upd)

	// Re-register with identical content: health and revision preserved.
	reg, _ := r.ValidateRegistration("svc", 1, []Instance{{ID: "b", Address: "h:2"}, {ID: "a", Address: "h:1"}})
	out := r.Apply(reg)
	if !out.OK || out.Changed || out.Revision != 1 {
		t.Fatalf("identical re-registration: %+v", out)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthHealthy || inst.Sequence != 1 {
		t.Fatalf("a health not preserved: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst.Health != HealthUnhealthy || inst.Sequence != 2 || inst.Reason != "down" {
		t.Fatalf("b health not preserved: %+v", inst)
	}

	// Address change resets to unknown/0.
	reg, _ = r.ValidateRegistration("svc", 1, []Instance{{ID: "a", Address: "h:9"}, {ID: "b", Address: "h:2"}})
	out = r.Apply(reg)
	if !out.OK || !out.Changed || out.Revision != 2 {
		t.Fatalf("address change: %+v", out)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthUnknown || inst.Sequence != 0 {
		t.Fatalf("a should reset after address change: %+v", inst)
	}
	// b kept its address, so its observation survives.
	if inst := instanceHealth(t, r, "svc", "b"); inst.Health != HealthUnhealthy || inst.Sequence != 2 {
		t.Fatalf("b should keep health: %+v", inst)
	}

	// A health result collected under the old revision is rejected even though
	// the instance and its new address exist.
	upd, _ = r.ValidateHealth("svc", "a", 1, 3, true, "")
	hout := r.ApplyHealth(upd)
	if hout.OK || hout.Kind != OutcomeConflict || hout.Actual != 2 {
		t.Fatalf("old revision result: %+v", hout)
	}

	// New revision accepts the observation on the reset instance.
	upd, _ = r.ValidateHealth("svc", "a", 2, 3, true, "")
	hout = r.ApplyHealth(upd)
	if !hout.OK || !hout.Changed || hout.Sequence != 3 {
		t.Fatalf("new revision result: %+v", hout)
	}
}

func TestRegistryHealthResetOnRemoveAndReadd(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})
	upd, _ := r.ValidateHealth("svc", "a", 1, 1, false, "down")
	r.ApplyHealth(upd)

	// Remove the instance.
	reg, _ := r.ValidateRegistration("svc", 1, nil)
	out := r.Apply(reg)
	if !out.OK || !out.Changed || out.Revision != 2 {
		t.Fatalf("remove: %+v", out)
	}

	// Re-add: starts from unknown/0, not the old observation.
	reg, _ = r.ValidateRegistration("svc", 2, []Instance{{ID: "a", Address: "h:1"}})
	out = r.Apply(reg)
	if !out.OK || !out.Changed || out.Revision != 3 {
		t.Fatalf("readd: %+v", out)
	}
	inst := instanceHealth(t, r, "svc", "a")
	if inst.Health != HealthUnknown || inst.Sequence != 0 || inst.Reason != "" {
		t.Fatalf("readded instance should be fresh: %+v", inst)
	}
}

func TestRegistryHealthFailedRegistrationKeepsObservations(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})
	upd, _ := r.ValidateHealth("svc", "a", 1, 1, true, "")
	r.ApplyHealth(upd)

	// Conflicting registration must not clear observations.
	reg, _ := r.ValidateRegistration("svc", 99, []Instance{{ID: "a", Address: "h:1"}})
	out := r.Apply(reg)
	if out.OK {
		t.Fatalf("expected conflict")
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthHealthy || inst.Sequence != 1 {
		t.Fatalf("observation cleared after failed registration: %+v", inst)
	}
}

// markHealth records one observation, failing the test on error.
func markHealth(t *testing.T, r *Registry, service, id string, revision int64, sequence int64, healthy bool, reason string) {
	t.Helper()
	upd, err := r.ValidateHealth(service, id, revision, sequence, healthy, reason)
	if err != nil {
		t.Fatalf("validate health %s/%s: %v", service, id, err)
	}
	if out := r.ApplyHealth(upd); !out.OK {
		t.Fatalf("apply health %s/%s: %+v", service, id, out)
	}
}

func selected(t *testing.T, r *Registry, service string, revision int64) SelectOutcome {
	t.Helper()
	sel, err := r.ValidateSelection(service, revision)
	if err != nil {
		t.Fatalf("validate select %s: %v", service, err)
	}
	out := r.Select(sel)
	if !out.OK {
		t.Fatalf("select %s: %+v", service, out)
	}
	return out
}

func TestRegistrySelectRotatesByAscendingID(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "i3", Address: "h3:3"}, {ID: "i1", Address: "h1:1"}, {ID: "i2", Address: "h2:2"},
	})
	markHealth(t, r, "svc", "i1", 1, 10, true, "")
	markHealth(t, r, "svc", "i2", 1, 20, true, "")
	markHealth(t, r, "svc", "i3", 1, 30, true, "")

	want := []struct {
		id  string
		seq int64
	}{
		{"i1", 10}, {"i2", 20}, {"i3", 30}, {"i1", 10}, {"i2", 20},
	}
	addr := map[string]string{"i1": "h1:1", "i2": "h2:2", "i3": "h3:3"}
	for i, w := range want {
		out := selected(t, r, "svc", 1)
		if out.InstanceID != w.id || out.Sequence != w.seq || out.Address != addr[w.id] {
			t.Fatalf("select %d: got id=%s seq=%d addr=%s, want %+v", i, out.InstanceID, out.Sequence, out.Address, w)
		}
		if out.Revision != 1 {
			t.Fatalf("select %d revision: %d", i, out.Revision)
		}
	}
}

func TestRegistrySelectOnlyHealthyEligible(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}, {ID: "c", Address: "h:3"},
	})
	// a and c healthy; b unhealthy; a fresh unknown instance is added later.
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, false, "down")
	markHealth(t, r, "svc", "c", 1, 1, true, "")

	got := []string{
		selected(t, r, "svc", 1).InstanceID,
		selected(t, r, "svc", 1).InstanceID,
		selected(t, r, "svc", 1).InstanceID,
	}
	want := []string{"a", "c", "a"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("selects: got %v want %v", got, want)
		}
	}
}

func TestRegistrySelectSingleHealthyRepeats(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}})
	markHealth(t, r, "svc", "a", 1, 7, true, "")
	markHealth(t, r, "svc", "b", 1, 1, false, "down")
	for i := 0; i < 3; i++ {
		out := selected(t, r, "svc", 1)
		if out.InstanceID != "a" || out.Address != "h:1" || out.Sequence != 7 {
			t.Fatalf("select %d: %+v", i, out)
		}
	}
}

func TestRegistrySelectFailuresDoNotAdvanceCursor(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")

	// First success lands on a.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" {
		t.Fatalf("first select: %+v", out)
	}

	selectFail := func(rev int64, want OutcomeKind) {
		t.Helper()
		sel, _ := r.ValidateSelection("svc", rev)
		out := r.Select(sel)
		if out.OK || out.Kind != want {
			t.Fatalf("want %s, got %+v", want, out)
		}
	}

	// Conflict (wrong revision) and no_healthy must not move the cursor.
	selectFail(99, OutcomeConflict)
	markHealth(t, r, "svc", "a", 1, 2, false, "down")
	markHealth(t, r, "svc", "b", 1, 3, false, "down")
	selectFail(1, OutcomeNoHealthy)
	selectFail(99, OutcomeConflict)
	selectFail(1, OutcomeNoHealthy)

	// Restore: the cursor is still just after a, so b (the next healthy id)
	// is chosen rather than restarting at the smallest id.
	markHealth(t, r, "svc", "a", 1, 4, true, "")
	markHealth(t, r, "svc", "b", 1, 5, true, "")
	if out := selected(t, r, "svc", 1); out.InstanceID != "b" {
		t.Fatalf("cursor should resume after a, got %+v", out)
	}
}

func TestRegistrySelectCursorSurvivesReplacementAndRemoval(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}, {ID: "c", Address: "h:3"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")
	markHealth(t, r, "svc", "c", 1, 1, true, "")
	// Two successes: a then b; cursor is b.
	selected(t, r, "svc", 1)
	selected(t, r, "svc", 1)

	// Replace: drop b (the last-chosen id), keep a and c, add d. Health for
	// kept ids survives; d starts unknown then becomes healthy.
	registerService(t, r, "svc", 1, []Instance{{ID: "a", Address: "h:1"}, {ID: "c", Address: "h:3"}, {ID: "d", Address: "h:4"}})
	if rev := r.RevisionOf("svc"); rev != 2 {
		t.Fatalf("revision after replace: %d", rev)
	}
	markHealth(t, r, "svc", "d", 2, 2, true, "")

	// Healthy set is a, c, d. Rotation continues just after the (now removed)
	// b, so c is chosen, not a.
	if out := selected(t, r, "svc", 2); out.InstanceID != "c" {
		t.Fatalf("continue after removed cursor: %+v", out)
	}
	// d is next after c.
	if out := selected(t, r, "svc", 2); out.InstanceID != "d" {
		t.Fatalf("new instance in position: %+v", out)
	}
	// Wrap to smallest.
	if out := selected(t, r, "svc", 2); out.InstanceID != "a" {
		t.Fatalf("wrap: %+v", out)
	}
}

func TestRegistrySelectRevisionAndPresence(t *testing.T) {
	r := NewRegistry()

	// Unknown service, expectedRevision 0 -> not_found (actual 0).
	sel, _ := r.ValidateSelection("ghost", 0)
	out := r.Select(sel)
	if out.OK || out.Kind != OutcomeNotFound || out.Revision != 0 {
		t.Fatalf("missing service rev 0: %+v", out)
	}

	// Unknown service, non-zero revision -> conflict with actual 0.
	sel, _ = r.ValidateSelection("ghost", 3)
	out = r.Select(sel)
	if out.OK || out.Kind != OutcomeConflict || out.Expected != 3 || out.Actual != 0 || out.Revision != 0 {
		t.Fatalf("missing service rev 3: %+v", out)
	}

	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})

	// Existing service, wrong revision -> conflict carrying actual revision.
	sel, _ = r.ValidateSelection("svc", 5)
	out = r.Select(sel)
	if out.OK || out.Kind != OutcomeConflict || out.Expected != 5 || out.Actual != 1 || out.Revision != 1 {
		t.Fatalf("existing conflict: %+v", out)
	}

	// Matching revision but only unknown instances -> no_healthy, current revision.
	sel, _ = r.ValidateSelection("svc", 1)
	out = r.Select(sel)
	if out.OK || out.Kind != OutcomeNoHealthy || out.Revision != 1 || out.Address != "" || out.InstanceID != "" {
		t.Fatalf("no healthy: %+v", out)
	}
}

func TestRegistrySelectDoesNotMutateState(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})
	markHealth(t, r, "svc", "a", 1, 4, true, "")
	before := r.Snapshot()
	for i := 0; i < 3; i++ {
		selected(t, r, "svc", 1)
	}
	after := r.Snapshot()
	if len(after) != 1 || after[0].Revision != 1 {
		t.Fatalf("service list changed: %+v", after)
	}
	inst := after[0].Instances[0]
	b := before[0].Instances[0]
	if inst != b {
		t.Fatalf("instance changed: before %+v after %+v", b, inst)
	}
}

func TestRegistrySelectIndependentPerService(t *testing.T) {
	r := NewRegistry()
	for _, svc := range []string{"x", "y"} {
		registerService(t, r, svc, 0, []Instance{{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}})
		markHealth(t, r, svc, "a", 1, 1, true, "")
		markHealth(t, r, svc, "b", 1, 1, true, "")
	}
	// Advance x once (a) but not y; each service rotates independently.
	if out := selected(t, r, "x", 1); out.InstanceID != "a" {
		t.Fatalf("x first: %+v", out)
	}
	if out := selected(t, r, "y", 1); out.InstanceID != "a" {
		t.Fatalf("y first should be independent: %+v", out)
	}
	if out := selected(t, r, "x", 1); out.InstanceID != "b" {
		t.Fatalf("x second: %+v", out)
	}
}

func TestRegistrySelectValidation(t *testing.T) {
	r := NewRegistry()
	if _, err := r.ValidateSelection("   ", 0); err == nil {
		t.Fatalf("empty service should be invalid")
	}
	if _, err := r.ValidateSelection("svc", -1); err == nil {
		t.Fatalf("negative revision should be invalid")
	}
	sel, err := r.ValidateSelection("  svc  ", 0)
	if err != nil || sel.Service != "svc" || sel.Revision != 0 {
		t.Fatalf("trim/valid: %+v err=%v", sel, err)
	}
}

// TestSharedRevisionGateEmptyInstanceService locks the rule shared by health
// and select: a registered service whose instance list is empty still compares
// by its real revision instead of being treated as absent.
func TestSharedRevisionGateEmptyInstanceService(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})
	registerService(t, r, "svc", 1, nil) // empty list bumps to revision 2

	// Both operations, wrong expected revision against the empty service: a
	// conflict carrying the real revision, not not_found at revision 0.
	upd, _ := r.ValidateHealth("svc", "a", 1, 1, true, "")
	if out := r.ApplyHealth(upd); out.OK || out.Kind != OutcomeConflict ||
		out.Expected != 1 || out.Actual != 2 || out.Revision != 2 {
		t.Fatalf("health empty-service conflict: %+v", out)
	}
	sel, _ := r.ValidateSelection("svc", 1)
	if out := r.Select(sel); out.OK || out.Kind != OutcomeConflict ||
		out.Expected != 1 || out.Actual != 2 || out.Revision != 2 {
		t.Fatalf("select empty-service conflict: %+v", out)
	}

	// Matching revision: the gate passes. Health then reports the missing
	// instance (its own not_found), while select reports no_healthy; the two
	// business results must not be mixed.
	upd, _ = r.ValidateHealth("svc", "a", 2, 1, true, "")
	if out := r.ApplyHealth(upd); out.OK || out.Kind != OutcomeNotFound || out.Revision != 2 {
		t.Fatalf("health through gate on empty service: %+v", out)
	}
	sel, _ = r.ValidateSelection("svc", 2)
	if out := r.Select(sel); out.OK || out.Kind != OutcomeNoHealthy ||
		out.Revision != 2 || out.InstanceID != "" || out.Address != "" {
		t.Fatalf("select through gate on empty service: %+v", out)
	}
}

// TestSharedRevisionGateConflictPrecedesHealthBusinessRules locks that a
// revision mismatch on a health request is reported as conflict even when the
// instance is missing or the carried sequence is larger than anything seen.
func TestSharedRevisionGateConflictPrecedesHealthBusinessRules(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})
	markHealth(t, r, "svc", "a", 1, 5, true, "")

	// Missing instance plus a larger sequence, wrong revision: conflict first.
	upd, _ := r.ValidateHealth("svc", "ghost", 9, 100, true, "")
	out := r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeConflict || out.Expected != 9 ||
		out.Actual != 1 || out.Revision != 1 {
		t.Fatalf("missing instance must not mask revision conflict: %+v", out)
	}

	// Existing instance with a larger sequence, wrong revision: still conflict;
	// the observation must not have been written.
	upd, _ = r.ValidateHealth("svc", "a", 2, 100, false, "down")
	out = r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeConflict || out.Expected != 2 || out.Actual != 1 {
		t.Fatalf("larger sequence must not mask revision conflict: %+v", out)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthHealthy || inst.Sequence != 5 {
		t.Fatalf("observation written despite conflict: %+v", inst)
	}

	// Unknown service with a non-zero expected revision conflicts even though
	// the instance cannot exist; expected 0 on the same unknown service is the
	// operation-specific not_found.
	upd, _ = r.ValidateHealth("other", "a", 3, 100, true, "")
	if out := r.ApplyHealth(upd); out.OK || out.Kind != OutcomeConflict ||
		out.Expected != 3 || out.Actual != 0 || out.Revision != 0 {
		t.Fatalf("unknown service wrong revision: %+v", out)
	}
	upd, _ = r.ValidateHealth("other", "a", 0, 1, true, "")
	if out := r.ApplyHealth(upd); out.OK || out.Kind != OutcomeNotFound || out.Revision != 0 {
		t.Fatalf("unknown service expected 0: %+v", out)
	}
}

// TestSharedRevisionGateValidationPrecedesRevision locks the ordering shared by
// both operations: own-field validity is judged before the revision comparison
// even when the service is absent, so the answer is invalid rather than
// conflict or not_found.
func TestSharedRevisionGateValidationPrecedesRevision(t *testing.T) {
	r := NewRegistry()

	if _, err := r.ValidateHealth("   ", "a", 5, 1, true, ""); err == nil {
		t.Fatalf("blank service health should be invalid before the revision check")
	}
	if _, err := r.ValidateHealth("missing", "a", -1, 1, true, ""); err == nil {
		t.Fatalf("negative revision health should be invalid")
	}
	if _, err := r.ValidateSelection("   ", 5); err == nil {
		t.Fatalf("blank service select should be invalid before the revision check")
	}
	if _, err := r.ValidateSelection("missing", -1); err == nil {
		t.Fatalf("negative revision select should be invalid")
	}
}

// assertAcceptedServiceState locks the state established before the rejected
// replacements: revision 1, original addresses, accepted healthy observations.
func assertAcceptedServiceState(t *testing.T, r *Registry) {
	t.Helper()
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("revision should stay 1 after the rejected replacement, got %d", rev)
	}
	views := r.Snapshot()
	if len(views) != 1 || views[0].Service != "svc" || views[0].Revision != 1 {
		t.Fatalf("service view mutated: %+v", views)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Address != "h1:1" ||
		inst.Health != HealthHealthy || inst.Sequence != 11 || inst.Reason != "" {
		t.Fatalf("a must keep its original address and accepted healthy record: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst.Address != "h2:2" ||
		inst.Health != HealthHealthy || inst.Sequence != 22 || inst.Reason != "" {
		t.Fatalf("b must keep its original address and accepted healthy record: %+v", inst)
	}
}

// TestRegistryInvalidReplacementKeepsAcceptedStateAndCursor covers the invalid
// rejection condition: a list whose prefix is valid (and changes an existing
// instance's address) but whose later instance has an invalid address is
// rejected as a whole. Field validation precedes the revision comparison, so a
// simultaneously wrong expectedRevision still reports invalid and Apply is
// never reached; neither the valid prefix nor any reset to unknown may leak
// through, the revision is not consumed and the rotation position survives.
func TestRegistryInvalidReplacementKeepsAcceptedStateAndCursor(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 11, true, "")
	markHealth(t, r, "svc", "b", 1, 22, true, "")
	// One successful selection lands the cursor on a.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" || out.Address != "h1:1" || out.Sequence != 11 {
		t.Fatalf("first select: %+v", out)
	}

	// Valid prefix that would move a to a new address, invalid address later;
	// expectedRevision 9 is also wrong but must not downgrade the answer to a
	// conflict.
	reg, err := r.ValidateRegistration("svc", 9, []Instance{
		{ID: "a", Address: "h9:9"},
		{ID: "b", Address: "bad-address"},
	})
	if err == nil {
		t.Fatalf("expected an invalid-address error, got registration %+v", reg)
	}
	if msg := err.Error(); !strings.Contains(msg, `instance address "bad-address" is not a host:port address`) {
		t.Fatalf("error should name the invalid address, got %q", msg)
	}
	if reg.Service != "" || reg.Revision != 0 || reg.Instances != nil {
		t.Fatalf("failed validation must not return a partial registration: %+v", reg)
	}

	// Nothing was applied: revision, original addresses, health and sequences
	// are all the accepted ones; the rejected new address is absent.
	assertAcceptedServiceState(t, r)

	// The cursor did not move: the rotation continues just after a and returns
	// b's original address and accepted sequence instead of repeating a.
	if out := selected(t, r, "svc", 1); out.InstanceID != "b" || out.Address != "h2:2" || out.Sequence != 22 || out.Revision != 1 {
		t.Fatalf("select after invalid replacement should resume at b with its original state: %+v", out)
	}
	// Past the end the rotation wraps; the rejected new address must never be
	// selectable even though it sat in the valid prefix.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" || out.Address != "h1:1" || out.Sequence != 11 {
		t.Fatalf("wrapped select should use a's original address, got %+v", out)
	}
}

// TestRegistryConflictReplacementKeepsAcceptedStateAndCursor covers the
// conflict rejection condition: a fully valid replacement list that changes an
// existing address but carries an expectedRevision other than the current one
// is reported as conflict with both revisions and changes nothing — no revision
// bump, no address change, no health reset, no cursor movement.
func TestRegistryConflictReplacementKeepsAcceptedStateAndCursor(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 11, true, "")
	markHealth(t, r, "svc", "b", 1, 22, true, "")
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" || out.Address != "h1:1" || out.Sequence != 11 {
		t.Fatalf("first select: %+v", out)
	}

	// Entire list parses; only expectedRevision 5 mismatches the current 1.
	reg, err := r.ValidateRegistration("svc", 5, []Instance{
		{ID: "a", Address: "hx:8080"},
		{ID: "b", Address: "h2:2"},
	})
	if err != nil {
		t.Fatalf("content should be valid, got %v", err)
	}
	out := r.Apply(reg)
	if out.OK || out.Kind != OutcomeConflict || out.Changed ||
		out.Expected != 5 || out.Actual != 1 || out.Revision != 1 {
		t.Fatalf("replacement should conflict without changing state: %+v", out)
	}
	if msg := out.Reason; !strings.Contains(msg, "revision 1, not 5") {
		t.Fatalf("conflict reason should state both revisions, got %q", msg)
	}

	// The valid-but-conflicting replacement left no trace: a is still at its
	// original address, healthy at the accepted sequence 11.
	assertAcceptedServiceState(t, r)

	// The completed selection's position is untouched: the next healthy target
	// in rotation is b, then the wrap returns a's original address — never the
	// rejected hx:8080 and never a repeated a.
	if out := selected(t, r, "svc", 1); out.InstanceID != "b" || out.Address != "h2:2" || out.Sequence != 22 || out.Revision != 1 {
		t.Fatalf("select after conflict should resume at b with its original state: %+v", out)
	}
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" || out.Address != "h1:1" || out.Sequence != 11 || out.Revision != 1 {
		t.Fatalf("wrapped select should use a's original address, got %+v", out)
	}
}

// selectedWithSession runs one successful select carrying a session key.
func selectedWithSession(t *testing.T, r *Registry, service string, revision int64, key string) SelectOutcome {
	t.Helper()
	sel, err := r.ValidateSelectionWithSession(service, revision, &key)
	if err != nil {
		t.Fatalf("validate select %s key %q: %v", service, key, err)
	}
	out := r.Select(sel)
	if !out.OK {
		t.Fatalf("select %s key %q: %+v", service, key, out)
	}
	return out
}

func TestRegistrySelectSessionStickyAndCursor(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	markHealth(t, r, "svc", "c", 1, 3, true, "")

	// First use of the key rotates like a plain selection: smallest id, and
	// the cursor advances to a.
	if out := selectedWithSession(t, r, "svc", 1, "s1"); out.InstanceID != "a" || out.Sequence != 1 {
		t.Fatalf("first session select: %+v", out)
	}
	// Reuse returns the bound instance without moving the cursor.
	if out := selectedWithSession(t, r, "svc", 1, "s1"); out.InstanceID != "a" {
		t.Fatalf("session reuse: %+v", out)
	}
	// A plain selection continues just after a, proving the reuse above did
	// not advance the rotation.
	if out := selected(t, r, "svc", 1); out.InstanceID != "b" {
		t.Fatalf("plain select after reuse: %+v", out)
	}
	// The session is still bound to a.
	if out := selectedWithSession(t, r, "svc", 1, "s1"); out.InstanceID != "a" {
		t.Fatalf("session reuse after plain select: %+v", out)
	}
	// The next plain selection continues after b.
	if out := selected(t, r, "svc", 1); out.InstanceID != "c" {
		t.Fatalf("plain select should continue after b: %+v", out)
	}
}

func TestRegistrySelectSessionReflectsLatestSequence(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")

	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "a" || out.Sequence != 1 {
		t.Fatalf("bind: %+v", out)
	}
	// A newer accepted observation on the bound instance is reflected by the
	// next reuse, address and sequence both current.
	markHealth(t, r, "svc", "a", 1, 7, true, "")
	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "a" || out.Address != "h1:1" || out.Sequence != 7 {
		t.Fatalf("reuse should carry the latest accepted sequence: %+v", out)
	}
}

func TestRegistrySelectSessionFallbackOnUnhealthy(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")

	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "a" {
		t.Fatalf("bind: %+v", out)
	}
	// The bound instance turns unhealthy: the session falls back to the
	// rotation (continuing after a) and rebinds to the newly chosen instance.
	markHealth(t, r, "svc", "a", 1, 2, false, "down")
	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "b" || out.Sequence != 1 {
		t.Fatalf("fallback select: %+v", out)
	}
	// a recovers before the next request, but the binding moved to b.
	markHealth(t, r, "svc", "a", 1, 3, true, "")
	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "b" {
		t.Fatalf("binding should have moved to b: %+v", out)
	}
}

func TestRegistrySelectSessionReuseAfterRecovery(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")

	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "a" {
		t.Fatalf("bind: %+v", out)
	}
	// a flaps unhealthy and recovers before the next session request: the
	// binding is judged at selection time, so it is still reused.
	markHealth(t, r, "svc", "a", 1, 2, false, "down")
	markHealth(t, r, "svc", "a", 1, 3, true, "")
	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "a" || out.Sequence != 3 {
		t.Fatalf("recovered binding should be reused: %+v", out)
	}
}

func TestRegistrySelectSessionFallbackOnRemoval(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")

	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "a" {
		t.Fatalf("bind: %+v", out)
	}
	// The bound instance is removed from the list: fall back to the rotation.
	registerService(t, r, "svc", 1, []Instance{{ID: "b", Address: "h2:2"}})
	if out := selectedWithSession(t, r, "svc", 2, "s"); out.InstanceID != "b" {
		t.Fatalf("fallback after removal: %+v", out)
	}
	// Re-adding a does not steal the binding back.
	registerService(t, r, "svc", 2, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 3, 1, true, "")
	if out := selectedWithSession(t, r, "svc", 3, "s"); out.InstanceID != "b" {
		t.Fatalf("binding should stay on b: %+v", out)
	}
}

func TestRegistrySelectSessionAddressChangeResetsBinding(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 5, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")

	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "a" {
		t.Fatalf("bind: %+v", out)
	}
	// a keeps its id but moves to a new address: its health resets to unknown,
	// so the session must not reuse the old address's health record and falls
	// back to the rotation, rebinding to b.
	registerService(t, r, "svc", 1, []Instance{{ID: "a", Address: "h9:9"}, {ID: "b", Address: "h2:2"}})
	if out := selectedWithSession(t, r, "svc", 2, "s"); out.InstanceID != "b" || out.Address != "h2:2" {
		t.Fatalf("address change must not reuse old health: %+v", out)
	}
	// Only after a fresh observation at the new address is a eligible again;
	// the binding stays on b regardless.
	markHealth(t, r, "svc", "a", 2, 1, true, "")
	if out := selectedWithSession(t, r, "svc", 2, "s"); out.InstanceID != "b" {
		t.Fatalf("binding should stay on b: %+v", out)
	}
}

func TestRegistrySelectSessionPerServiceBindings(t *testing.T) {
	r := NewRegistry()
	for _, svc := range []string{"s1", "s2"} {
		registerService(t, r, svc, 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
		markHealth(t, r, svc, "a", 1, 1, true, "")
		markHealth(t, r, svc, "b", 1, 1, true, "")
	}
	// The same key binds independently per service.
	if out := selectedWithSession(t, r, "s1", 1, "k"); out.InstanceID != "a" {
		t.Fatalf("s1 bind: %+v", out)
	}
	if out := selectedWithSession(t, r, "s2", 1, "k"); out.InstanceID != "a" {
		t.Fatalf("s2 bind: %+v", out)
	}
	// Move s2's rotation with a plain select; s1's binding is unaffected.
	if out := selected(t, r, "s2", 1); out.InstanceID != "b" {
		t.Fatalf("s2 plain select: %+v", out)
	}
	if out := selectedWithSession(t, r, "s1", 1, "k"); out.InstanceID != "a" {
		t.Fatalf("s1 reuse: %+v", out)
	}
	if out := selectedWithSession(t, r, "s2", 1, "k"); out.InstanceID != "a" {
		t.Fatalf("s2 reuse: %+v", out)
	}
}

func TestRegistrySelectSessionFailuresKeepBindingAndCursor(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")

	// A conflicting session select creates no binding and moves no cursor.
	sel, err := r.ValidateSelectionWithSession("svc", 9, strPtr("s"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if out := r.Select(sel); out.OK || out.Kind != OutcomeConflict {
		t.Fatalf("conflict select: %+v", out)
	}
	// The first successful select (plain) still takes the smallest id.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" {
		t.Fatalf("plain select after conflict: %+v", out)
	}
	// The key was never bound: its first success rotates after a to b.
	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "b" {
		t.Fatalf("first session success should rotate to b: %+v", out)
	}

	// no_healthy with a key neither rewrites the binding nor moves the cursor.
	markHealth(t, r, "svc", "a", 1, 2, false, "down")
	markHealth(t, r, "svc", "b", 1, 2, false, "down")
	sel, err = r.ValidateSelectionWithSession("svc", 1, strPtr("s"))
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if out := r.Select(sel); out.OK || out.Kind != OutcomeNoHealthy {
		t.Fatalf("no_healthy select: %+v", out)
	}
	// b recovers: the binding survived the failed request and is reused.
	markHealth(t, r, "svc", "b", 1, 3, true, "")
	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "b" || out.Sequence != 3 {
		t.Fatalf("binding should survive no_healthy: %+v", out)
	}
}

func TestRegistrySelectSessionKeyValidation(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")

	// Keys are trimmed; keys equal after trimming share one session.
	if out := selectedWithSession(t, r, "svc", 1, "  k  "); out.InstanceID != "a" {
		t.Fatalf("bind with padded key: %+v", out)
	}
	if out := selectedWithSession(t, r, "svc", 1, "k"); out.InstanceID != "a" {
		t.Fatalf("trimmed key should reuse the same session: %+v", out)
	}

	// A blank-after-trim key is invalid and must not reach the registry.
	if _, err := r.ValidateSelectionWithSession("svc", 1, strPtr("   ")); err == nil {
		t.Fatalf("blank session key should be invalid")
	}
	if _, err := r.ValidateSelectionWithSession("svc", 1, strPtr("")); err == nil {
		t.Fatalf("empty session key should be invalid")
	}

	// A nil key means no session: plain rotation, continuing after a.
	sel, err := r.ValidateSelectionWithSession("svc", 1, nil)
	if err != nil {
		t.Fatalf("nil key validate: %v", err)
	}
	if sel.SessionKey != "" {
		t.Fatalf("nil key should mean no session: %+v", sel)
	}
	if out := r.Select(sel); !out.OK || out.InstanceID != "b" {
		t.Fatalf("nil key should rotate plainly: %+v", out)
	}
	// The session binding is untouched by the plain rotation.
	if out := selectedWithSession(t, r, "svc", 1, "k"); out.InstanceID != "a" {
		t.Fatalf("session reuse after plain select: %+v", out)
	}
}

// TestRegistryAddressPaddingOnlyReregistrationUnchanged is the regression guard
// for the address-tidying rule at the unchanged boundary: registration removes
// only surrounding whitespace from an address and then keeps the legal text. An
// instance that is already registered and has an accepted health observation,
// resubmitted with whitespace added only around the address, tidies to exactly
// the stored text, so the whole-list replacement succeeds without "changed":
// the revision is not consumed and the accepted health state, sequence and
// reason survive; a later selection returns the tidied original address and
// the original health sequence.
func TestRegistryAddressPaddingOnlyReregistrationUnchanged(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "api.example:8080"}})
	markHealth(t, r, "svc", "a", 1, 7, true, "")

	// Surrounding whitespace only: after trimming, the submitted text equals the
	// stored address. Validation itself tidies by trimming and nothing else.
	reg, err := r.ValidateRegistration("svc", 1, []Instance{{ID: "a", Address: "\t api.example:8080 \n"}})
	if err != nil {
		t.Fatalf("padded legal address should validate: %v", err)
	}
	if reg.Instances[0].Address != "api.example:8080" {
		t.Fatalf("address should be tidied by surrounding trim only: %q", reg.Instances[0].Address)
	}
	out := r.Apply(reg)
	if !out.OK || out.Changed || out.Revision != 1 {
		t.Fatalf("padding-only resubmission must succeed without change: %+v", out)
	}

	// The accepted observation survives exactly: state, sequence and reason, and
	// the stored address is the original (already tidied) spelling.
	if inst := instanceHealth(t, r, "svc", "a"); inst.Address != "api.example:8080" ||
		inst.Health != HealthHealthy || inst.Sequence != 7 || inst.Reason != "" {
		t.Fatalf("accepted health record must survive a padding-only registration: %+v", inst)
	}

	// Selection returns the tidied original address and the original sequence.
	if sel := selected(t, r, "svc", 1); sel.InstanceID != "a" ||
		sel.Address != "api.example:8080" || sel.Sequence != 7 || sel.Revision != 1 {
		t.Fatalf("selection after padding-only registration: %+v", sel)
	}
}

// TestRegistryDistinctLegalAddressSpellingIsChange locks the other side of the
// tidying rule: trimming must not collapse distinct legal spellings. Domain
// letters keep their case and a port keeps its leading zeros, so
// "api.example:8080" and "API.example:08080" both parse as legal host:port
// addresses but remain different texts after trimming. For an unchanged
// instance id that is a content change: the replacement reports changed, bumps
// the revision exactly once and stores the new spelling as submitted, while the
// instance's health record restarts at unknown/0 with no reason; another
// instance whose id and address are unchanged keeps its own record. With no
// other healthy instance the new address is not selectable until an
// observation is accepted under the current revision; that observation may
// restart the sequence at 1, and selection then returns the new spelling with
// the new sequence.
func TestRegistryDistinctLegalAddressSpellingIsChange(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "api.example:8080"},
		{ID: "b", Address: "other.example:9090"},
	})
	markHealth(t, r, "svc", "a", 1, 11, true, "")
	markHealth(t, r, "svc", "b", 1, 12, false, "connection refused")

	// The new spelling parses on its own: uppercase domain labels and a leading
	// zero in the port are accepted forms, never normalized onto the old text.
	reg, err := r.ValidateRegistration("svc", 1, []Instance{
		{ID: "a", Address: "API.example:08080"},
		{ID: "b", Address: "other.example:9090"},
	})
	if err != nil {
		t.Fatalf("distinct legal spelling should validate: %v", err)
	}
	if reg.Instances[0].Address != "API.example:08080" {
		t.Fatalf("new spelling must be kept as submitted, got %q", reg.Instances[0].Address)
	}
	out := r.Apply(reg)
	if !out.OK || !out.Changed || out.Revision != 2 {
		t.Fatalf("distinct legal spelling is a content change: %+v", out)
	}

	// a resets to the fresh observation at the new spelling; b's id and address
	// are unchanged so its unhealthy record, sequence and reason are retained.
	if inst := instanceHealth(t, r, "svc", "a"); inst.Address != "API.example:08080" ||
		inst.Health != HealthUnknown || inst.Sequence != 0 || inst.Reason != "" {
		t.Fatalf("a should reset to unknown/0/no-reason at its new spelling: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst.Address != "other.example:9090" ||
		inst.Health != HealthUnhealthy || inst.Sequence != 12 || inst.Reason != "connection refused" {
		t.Fatalf("b should keep its own record: %+v", inst)
	}

	// No other healthy instance: the new address is unknown, so selection at the
	// current revision is no_healthy with the current revision and no fabricated
	// target.
	sel, _ := r.ValidateSelection("svc", 2)
	sout := r.Select(sel)
	if sout.OK || sout.Kind != OutcomeNoHealthy || sout.Revision != 2 ||
		sout.InstanceID != "" || sout.Address != "" || sout.Sequence != 0 {
		t.Fatalf("new address must not be selectable before a new observation: %+v", sout)
	}

	// An observation under the old revision conflicts even with a sequence far
	// above the old accepted one and heals nothing.
	upd, _ := r.ValidateHealth("svc", "a", 1, 99, true, "")
	if hout := r.ApplyHealth(upd); hout.OK || hout.Kind != OutcomeConflict ||
		hout.Expected != 1 || hout.Actual != 2 {
		t.Fatalf("old-revision observation must conflict: %+v", hout)
	}
	sel, _ = r.ValidateSelection("svc", 2)
	if sout := r.Select(sel); sout.OK || sout.Kind != OutcomeNoHealthy {
		t.Fatalf("conflicting observation must not restore eligibility: %+v", sout)
	}

	// The reset releases the sequence floor: a fresh observation at the current
	// revision may start at 1 even though 11 was accepted for the old spelling.
	markHealth(t, r, "svc", "a", 2, 1, true, "")
	sel, _ = r.ValidateSelection("svc", 2)
	sout = r.Select(sel)
	if !sout.OK || sout.InstanceID != "a" ||
		sout.Address != "API.example:08080" || sout.Sequence != 1 || sout.Revision != 2 {
		t.Fatalf("selection must return the new spelling with its new sequence: %+v", sout)
	}
}

// TestRegistryAddressInternalWhitespaceAndControlRejected locks the illegal
// side of the address acceptance range: whitespace and control characters are
// tolerated only around the ends and removed there; an address containing them
// internally makes the whole item invalid. The failure explains the address
// problem and stamps the service's current revision, and a simultaneously
// wrong expectedRevision must not turn the answer into a conflict. The legal
// changes elsewhere in the same list must not partially apply — revision,
// original list and accepted health records all survive — and later requests
// keep being processed in batch order against the committed state.
func TestRegistryAddressInternalWhitespaceAndControlRejected(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"},
		{ID: "b", Address: "h2:2"},
	})
	markHealth(t, r, "svc", "a", 1, 11, true, "")
	markHealth(t, r, "svc", "b", 1, 22, true, "")

	bad := []struct {
		name    string
		address string
	}{
		{"internal space", "api.example :8080"},
		{"internal tab", "api.ex\tmple:8080"},
		{"control character", "api.ex\u0001ample:8080"},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			// a's legal address change earlier in the same list plus b's illegal
			// address, with expectedRevision 9 also wrong: content validity wins,
			// the item is invalid and the legal prefix must not take effect.
			reg, err := r.ValidateRegistration("svc", 9, []Instance{
				{ID: "a", Address: "h9:9"},
				{ID: "b", Address: tc.address},
			})
			if err == nil {
				t.Fatalf("address with an %s should be invalid, got registration %+v", tc.name, reg)
			}
			msg := err.Error()
			if !strings.Contains(msg, "instance address") ||
				!strings.Contains(msg, "must not contain whitespace or control characters") {
				t.Fatalf("error should explain the address problem, got %q", msg)
			}
			if reg.Service != "" || reg.Revision != 0 || reg.Instances != nil {
				t.Fatalf("failed validation must not return a partial registration: %+v", reg)
			}
		})
	}

	// Nothing leaked through any rejected attempt: revision stays 1, the
	// original addresses and the accepted health records are all intact.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("revision consumed by invalid registrations: %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Address != "h1:1" ||
		inst.Health != HealthHealthy || inst.Sequence != 11 {
		t.Fatalf("a must keep its accepted record: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst.Address != "h2:2" ||
		inst.Health != HealthHealthy || inst.Sequence != 22 {
		t.Fatalf("b must keep its accepted record: %+v", inst)
	}

	// Later legal requests still apply, and selection keeps using the accepted
	// addresses and sequences: the rejected items consumed neither a revision
	// nor a rotation step.
	registerService(t, r, "svc", 1, []Instance{
		{ID: "a", Address: "h1:1"},
		{ID: "b", Address: "h2:2"},
	})
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" ||
		out.Address != "h1:1" || out.Sequence != 11 || out.Revision != 1 {
		t.Fatalf("selection after rejected items should use the accepted state: %+v", out)
	}
}

func strPtr(s string) *string { return &s }
