package indexroom

import (
	"strings"
	"testing"
)

// This file locks support for a fully qualified domain name's single trailing
// dot (the root label, e.g. "api.example.:8080") in a registered instance
// address. The dot is notation marking a complete name: it is preserved
// verbatim in the saved/selected address text, does not count toward the
// 253-character body limit, and makes the address distinct from the same name
// without it. Everything else (ports, IPv4, bracketed IPv6, whitespace,
// label rules) is unchanged.

func TestFQDNAddressAcceptedVerbatim(t *testing.T) {
	r := NewRegistry()
	reg, err := r.ValidateRegistration("svc", 0, []Instance{
		// Surrounding whitespace is trimmed; case and the trailing dot survive.
		{ID: "i1", Address: "  API.Example.:8080  "},
		{ID: "i2", Address: "api.example.:1"},
		{ID: "i3", Address: "singlelabel.:65535"},
	})
	if err != nil {
		t.Fatalf("fully qualified domains rejected: %v", err)
	}
	want := []Instance{
		{ID: "i1", Address: "API.Example.:8080"},
		{ID: "i2", Address: "api.example.:1"},
		{ID: "i3", Address: "singlelabel.:65535"},
	}
	for i := range want {
		if reg.Instances[i] != want[i] {
			t.Fatalf("instance %d: got %+v want %+v", i, reg.Instances[i], want[i])
		}
	}

	out := r.Apply(reg)
	if !out.OK || out.Revision != 1 {
		t.Fatalf("create: %+v", out)
	}
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Address != "API.Example.:8080" {
		t.Fatalf("snapshot must keep the exact address text: %+v", inst)
	}
}

func TestFQDNBodyLengthExcludesRootDot(t *testing.T) {
	r := NewRegistry()
	// A 253-character body (127 one-character labels) plus the root dot is a
	// 254-character address and must be accepted; the dot is not part of the
	// body length.
	atLimit := strings.Repeat("a.", 126) + "a" // 253 chars
	if _, err := r.ValidateRegistration("ok", 0, []Instance{
		{ID: "x", Address: atLimit + ".:8080"},
	}); err != nil {
		t.Fatalf("253-char body with root dot rejected: %v", err)
	}

	// A 254-character body is over the limit whether or not a root dot follows.
	over := strings.Repeat("a.", 126) + "aa" // 254 chars, no trailing dot
	if _, err := r.ValidateRegistration("bad", 0, []Instance{
		{ID: "x", Address: over + ":8080"},
	}); err == nil {
		t.Fatalf("254-char body must be invalid")
	}
	if _, err := r.ValidateRegistration("bad2", 0, []Instance{
		{ID: "x", Address: over + ".:8080"},
	}); err == nil {
		t.Fatalf("254-char body with root dot must be invalid")
	}
}

func TestFQDNAddressInvalidForms(t *testing.T) {
	r := NewRegistry()
	cases := []struct {
		name    string
		address string
	}{
		{"root dot only", ".:8080"},
		{"empty label inside body", "api..example.:8080"},
		{"empty label before root dot", "api.example..:8080"},
		{"two trailing dots", "api.example...:8080"},
		{"bad character with root dot", "api.ex_ample.:8080"},
		{"leading hyphen label with root dot", "api.-example.:8080"},
		{"trailing hyphen label with root dot", "api.example-.:8080"},
		{"64-char label with root dot", strings.Repeat("a", 64) + ".:8080"},
		{"internal space before root dot", "api.example. :8080"},
		{"port zero", "api.example.:0"},
		{"port too high", "api.example.:65536"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := r.ValidateRegistration("svc", 0, []Instance{
				{ID: "a", Address: tc.address},
			}); err == nil {
				t.Fatalf("%q must be invalid", tc.address)
			}
		})
	}
}

// TestFQDNReplacementIsRealChange drives the exact scenario: the same instance
// moves from "api.example:8080" to "api.example.:8080". The trailing dot makes
// it a genuinely different list entry, so the replacement bumps the revision,
// resets that one instance to unknown/0/no reason and keeps it out of
// selection until a fresh healthy observation at the new revision; an instance
// whose address text did not change keeps its health record.
func TestFQDNReplacementIsRealChange(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "i1", Address: "api.example:8080"},
		{ID: "i2", Address: "h2:2"},
	})
	markHealth(t, r, "svc", "i1", 1, 5, true, "")
	markHealth(t, r, "svc", "i2", 1, 6, true, "")

	reg, err := r.ValidateRegistration("svc", 1, []Instance{
		{ID: "i1", Address: "api.example.:8080"},
		{ID: "i2", Address: "h2:2"},
	})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := r.Apply(reg)
	if !out.OK || !out.Changed || out.Revision != 2 {
		t.Fatalf("adding the root dot must be a real change: %+v", out)
	}
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Health != HealthUnknown ||
		inst.Sequence != 0 || inst.Reason != "" || inst.Address != "api.example.:8080" {
		t.Fatalf("i1 must restart unknown/0/no reason at the new address: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "i2"); inst.Health != HealthHealthy ||
		inst.Sequence != 6 || inst.Address != "h2:2" {
		t.Fatalf("unchanged i2 must keep its observation: %+v", inst)
	}

	// Before i1 is re-observed at the new address it is not selectable; the
	// rotation can only return i2.
	if got := selected(t, r, "svc", 2); got.InstanceID != "i2" ||
		got.Address != "h2:2" || got.Sequence != 6 {
		t.Fatalf("unknown i1 must not be chosen: %+v", got)
	}

	// A healthy observation at the current revision makes i1 eligible again;
	// selection returns the same address text and the latest accepted sequence.
	markHealth(t, r, "svc", "i1", 2, 7, true, "")
	if got := selected(t, r, "svc", 2); got.InstanceID != "i1" ||
		got.Address != "api.example.:8080" || got.Sequence != 7 || got.Revision != 2 {
		t.Fatalf("selection must return the verbatim FQDN and latest sequence: %+v", got)
	}

	// Resubmitting the dotted list is identical content: no revision bump, and
	// i1's fresh observation survives.
	reg, err = r.ValidateRegistration("svc", 2, []Instance{
		{ID: "i2", Address: "h2:2"},
		{ID: "i1", Address: "api.example.:8080"},
	})
	if err != nil {
		t.Fatalf("validate identical: %v", err)
	}
	if out := r.Apply(reg); !out.OK || out.Changed || out.Revision != 2 {
		t.Fatalf("identical dotted list must be unchanged: %+v", out)
	}
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Health != HealthHealthy || inst.Sequence != 7 {
		t.Fatalf("i1 observation lost on identical resubmission: %+v", inst)
	}

	// Moving back to the non-dotted text is a real change in the other
	// direction and resets i1 again.
	reg, _ = r.ValidateRegistration("svc", 2, []Instance{
		{ID: "i1", Address: "api.example:8080"},
		{ID: "i2", Address: "h2:2"},
	})
	if out := r.Apply(reg); !out.OK || !out.Changed || out.Revision != 3 {
		t.Fatalf("removing the root dot must also be a real change: %+v", out)
	}
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Health != HealthUnknown ||
		inst.Sequence != 0 || inst.Address != "api.example:8080" {
		t.Fatalf("i1 must restart unknown/0 at the non-dotted address: %+v", inst)
	}
}

// TestFQDNInvalidReplacementAtomicAndPrecedence locks that an FQDN-related bad
// address fails the whole item: content validation precedes the revision
// comparison (so a simultaneously wrong expectedRevision still reports invalid
// with the address reason and the current revision), no valid prefix leaks in,
// and later requests run against the last accepted state.
func TestFQDNInvalidReplacementAtomicAndPrecedence(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "i1", Address: "api.example:8080"},
	})
	markHealth(t, r, "svc", "i1", 1, 9, true, "")

	for _, bad := range []string{
		"api..example.:8080",
		"api.example..:8080",
		".:8080",
	} {
		// Revision 9 also mismatches the current 1, but the address wins.
		reg, err := r.ValidateRegistration("svc", 9, []Instance{
			{ID: "i1", Address: "api.example.:8080"}, // valid-looking prefix
			{ID: "i2", Address: bad},
		})
		if err == nil {
			t.Fatalf("%q must be invalid, got %+v", bad, reg)
		}
		if msg := err.Error(); !strings.Contains(msg, bad) {
			t.Fatalf("error must name the offending address %q, got %q", bad, msg)
		}
		if reg.Service != "" || reg.Instances != nil {
			t.Fatalf("failed validation must return no partial registration: %+v", reg)
		}

		// Nothing was applied: revision stays 1, i1 keeps its original address
		// and observation, and the dotted prefix never entered the list.
		if rev := r.RevisionOf("svc"); rev != 1 {
			t.Fatalf("%q: revision consumed: %d", bad, rev)
		}
		if inst := instanceHealth(t, r, "svc", "i1"); inst.Address != "api.example:8080" ||
			inst.Health != HealthHealthy || inst.Sequence != 9 {
			t.Fatalf("%q: accepted state mutated: %+v", bad, inst)
		}
	}

	// A later, correct request still works against the accepted state.
	registerService(t, r, "svc", 1, []Instance{
		{ID: "i1", Address: "api.example.:8080"},
	})
	if rev := r.RevisionOf("svc"); rev != 2 {
		t.Fatalf("later valid replacement must proceed, got revision %d", rev)
	}
}
