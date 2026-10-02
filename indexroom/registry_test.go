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
