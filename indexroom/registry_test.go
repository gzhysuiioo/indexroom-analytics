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
	want := []Instance{
		{ID: "i1", Address: "10.0.0.2:443"},
		{ID: "i3", Address: "[2001:db8::1]:9090"},
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
