package indexroom

import "testing"

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
		revision  int
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
func registerService(t *testing.T, r *Registry, service string, rev int, instances []Instance) {
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
		revision   int
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
func markHealth(t *testing.T, r *Registry, service, id string, revision int, sequence int64, healthy bool, reason string) {
	t.Helper()
	upd, err := r.ValidateHealth(service, id, revision, sequence, healthy, reason)
	if err != nil {
		t.Fatalf("validate health %s/%s: %v", service, id, err)
	}
	if out := r.ApplyHealth(upd); !out.OK {
		t.Fatalf("apply health %s/%s: %+v", service, id, out)
	}
}

func selected(t *testing.T, r *Registry, service string, revision int) SelectOutcome {
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

	selectFail := func(rev int, want OutcomeKind) {
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

// TestRegistryEmptyListTakesPartInRevisionGate pins the shared revision rule:
// a registered service whose instance list is empty still participates at its
// real revision. It must not be treated as an unknown (revision 0) service, and
// the gate's verdict must stay distinct from each operation's own result.
func TestRegistryEmptyListTakesPartInRevisionGate(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})
	registerService(t, r, "svc", 1, nil) // replaces with an empty list, revision 2

	// Health at an outdated revision conflicts at the real revision, even though
	// the service currently has no instance the request could match.
	upd, _ := r.ValidateHealth("svc", "a", 1, 9, true, "")
	out := r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeConflict || out.Expected != 1 || out.Actual != 2 || out.Revision != 2 {
		t.Fatalf("health outdated revision on empty service: %+v", out)
	}

	// Matching revision reaches health's own rule: the instance is not found.
	upd, _ = r.ValidateHealth("svc", "a", 2, 9, true, "")
	out = r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeNotFound || out.Revision != 2 {
		t.Fatalf("health matching revision on empty service: %+v", out)
	}

	// Select at an outdated revision conflicts before the no_healthy rule.
	sel, _ := r.ValidateSelection("svc", 1)
	sout := r.Select(sel)
	if sout.OK || sout.Kind != OutcomeConflict || sout.Expected != 1 || sout.Actual != 2 || sout.Revision != 2 {
		t.Fatalf("select outdated revision on empty service: %+v", sout)
	}

	// Matching revision reaches select's own rule: the present-but-empty service
	// has no healthy instance.
	sel, _ = r.ValidateSelection("svc", 2)
	sout = r.Select(sel)
	if sout.OK || sout.Kind != OutcomeNoHealthy || sout.Revision != 2 {
		t.Fatalf("select matching revision on empty service: %+v", sout)
	}
}

// TestRegistryHealthRevisionConflictPrecedesBusinessRules pins gate precedence:
// a revision mismatch is reported as conflict regardless of a missing instance
// or a newer/larger sequence, and neither observation nor rotation moves.
func TestRegistryHealthRevisionConflictPrecedesBusinessRules(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})

	upd, _ := r.ValidateHealth("svc", "ghost", 2, 999, true, "")
	out := r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeConflict || out.Expected != 2 || out.Actual != 1 || out.Revision != 1 {
		t.Fatalf("missing instance must not mask the revision conflict: %+v", out)
	}
	if out.Sequence != 0 || out.InstanceID != "" {
		t.Fatalf("conflict must not borrow business fields: %+v", out)
	}

	// Unknown service, non-zero expected revision: conflict at revision 0 even
	// though the instance cannot exist either.
	upd, _ = r.ValidateHealth("other", "ghost", 4, 999, true, "")
	out = r.ApplyHealth(upd)
	if out.OK || out.Kind != OutcomeConflict || out.Expected != 4 || out.Actual != 0 || out.Revision != 0 {
		t.Fatalf("unknown service wrong revision: %+v", out)
	}

	// No observation landed for either request.
	if views := r.Snapshot(); len(views) != 1 || views[0].Revision != 1 || len(views[0].Instances) != 1 {
		t.Fatalf("state changed after revision conflicts: %+v", views)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthUnknown || inst.Sequence != 0 {
		t.Fatalf("existing instance touched by rejected report: %+v", inst)
	}
}

// TestRegistrySharedValidationOrderAndPrecedence pins the shared field checks
// extracted for health and select: per-field order is preserved and invalid
// fields win over any revision or service-presence judgment.
func TestRegistrySharedValidationOrderAndPrecedence(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}})

	// Health keeps checking the instance id before expectedRevision.
	if _, err := r.ValidateHealth("svc", "  ", -1, 1, true, ""); err == nil || err.Error() != "instance id must not be empty" {
		t.Fatalf("health field order: %v", err)
	}
	// Invalid fields report invalid even against an existing service whose
	// current revision differs, and even when the service is unknown.
	if _, err := r.ValidateHealth("svc", "a", -7, 1, true, ""); err == nil || err.Error() != "expectedRevision must be a non-negative integer" {
		t.Fatalf("health negative revision against existing service: %v", err)
	}
	if _, err := r.ValidateHealth("   ", "a", 5, 1, true, ""); err == nil || err.Error() != "service name must not be empty" {
		t.Fatalf("health empty service: %v", err)
	}
	if _, err := r.ValidateSelection("svc", -7); err == nil || err.Error() != "expectedRevision must be a non-negative integer" {
		t.Fatalf("select negative revision against existing service: %v", err)
	}
	if _, err := r.ValidateSelection("   ", 5); err == nil || err.Error() != "service name must not be empty" {
		t.Fatalf("select empty service: %v", err)
	}
}
