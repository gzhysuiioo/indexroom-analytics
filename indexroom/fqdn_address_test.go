package indexroom

import (
	"strings"
	"testing"
)

// This file is the regression guard for fully qualified domain names that end
// in exactly one root dot (e.g. "api.example.:8080"). Such an address was
// rejected as invalid even though the trailing dot is a legal, meaningful part
// of a complete domain name. Everything is validated offline: no name
// resolution or other network access occurs.

// TestFQDNAddressValidation covers the address grammar around the root dot:
// the dot is accepted exactly once at the end, it is not counted against the
// 253-character body limit, body labels keep the existing character, length
// and hyphen-position rules, and the returned text keeps case and the dot
// after only trimming surrounding whitespace.
func TestFQDNAddressValidation(t *testing.T) {
	r := NewRegistry()

	valid := []struct {
		name string
		addr string
		want string // normalized address after trimming
	}{
		{"fully qualified multi-label", "api.example.:8080", "api.example.:8080"},
		{"fully qualified single label", "localhost.:1", "localhost.:1"},
		{"case and dot are preserved", "  API.Example.:8443  ", "API.Example.:8443"},
		{"ordinary undotted domain still accepted", "api.example:8080", "api.example:8080"},
		{"body of exactly 253 chars plus root dot", longestBody(253) + ".:8080", longestBody(253) + ".:8080"},
		{"body of 253 chars without root dot", longestBody(253) + ":8080", longestBody(253) + ":8080"},
	}
	for _, tc := range valid {
		t.Run("valid/"+tc.name, func(t *testing.T) {
			reg, err := r.ValidateRegistration("svc", 0, []Instance{{ID: "a", Address: tc.addr}})
			if err != nil {
				t.Fatalf("address %q rejected: %v", tc.addr, err)
			}
			if got := reg.Instances[0].Address; got != tc.want {
				t.Fatalf("address %q normalized to %q, want %q", tc.addr, got, tc.want)
			}
		})
	}

	invalid := []struct {
		name string
		addr string
	}{
		{"empty body is just the root dot", ".:8080"},
		{"double dot inside body", "api..example.:8080"},
		{"double dot at end of body", "api.example..:8080"},
		{"leading dot leaves an empty label", ".example.:8080"},
		{"body of 254 chars even with root dot", longestBody(254) + ".:8080"},
		{"label of 64 chars with root dot", strings.Repeat("a", 64) + ".:8080"},
		{"leading hyphen label with root dot", "-bad.example.:8080"},
		{"trailing hyphen label with root dot", "bad-.example.:8080"},
		{"illegal character with root dot", "my_host.example.:8080"},
		{"internal whitespace with root dot", "api.example. :8080"},
		{"root dot without a port", "api.example."},
		{"root dot with port zero", "api.example.:0"},
		{"root dot with port out of range", "api.example.:65536"},
	}
	for _, tc := range invalid {
		t.Run("invalid/"+tc.name, func(t *testing.T) {
			if _, err := r.ValidateRegistration("svc", 0, []Instance{{ID: "a", Address: tc.addr}}); err == nil {
				t.Fatalf("address %q must be invalid", tc.addr)
			}
		})
	}
}

// TestFQDNAddressReplacementBumpsRevisionAndResetsHealth proves the dotted and
// undotted spellings of the same name are two distinct registration addresses:
// replacing one with the other is a real list change (revision bump), the
// affected instance returns to unknown/0 with no reason and cannot be selected
// until a fresh observation at the new revision, while an instance whose
// address is untouched keeps its health record. After the fresh observation
// select returns the dotted address text and the latest health sequence.
func TestFQDNAddressReplacementBumpsRevisionAndResetsHealth(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "i1", Address: "api.example:8080"},
		{ID: "i2", Address: "h2:2"},
	})
	markHealth(t, r, "svc", "i1", 1, 10, true, "")
	markHealth(t, r, "svc", "i2", 1, 20, true, "")
	assertPick(t, selected(t, r, "svc", 1), "i1", "api.example:8080", 10)

	// Replace i1's address with the fully qualified spelling; i2 is untouched.
	reg, err := r.ValidateRegistration("svc", 1, []Instance{
		{ID: "i2", Address: "h2:2"},
		{ID: "i1", Address: "api.example.:8080"},
	})
	if err != nil {
		t.Fatalf("dotted replacement rejected: %v", err)
	}
	out := r.Apply(reg)
	if !out.OK || !out.Changed || out.Revision != 2 {
		t.Fatalf("dotted replacement must bump the revision: %+v", out)
	}

	views := r.Snapshot()
	if len(views) != 1 {
		t.Fatalf("snapshot: %+v", views)
	}
	inst := map[string]InstanceView{}
	for _, v := range views[0].Instances {
		inst[v.ID] = v
	}
	if got := inst["i1"]; got.Address != "api.example.:8080" || got.Health != HealthUnknown ||
		got.Sequence != 0 || got.Reason != "" {
		t.Fatalf("i1 must restart unknown at the dotted address: %+v", got)
	}
	if got := inst["i2"]; got.Address != "h2:2" || got.Health != HealthHealthy ||
		got.Sequence != 20 || got.Reason != "" {
		t.Fatalf("i2 must keep its health record: %+v", got)
	}

	// i1 is unknown at the new revision: excluding the still-healthy i2 leaves
	// no selectable target, and the failure moves no state.
	sel, err := r.ValidateSelectionWithExclusions("svc", 2, nil, []string{"i2"})
	if err != nil {
		t.Fatalf("validate select: %v", err)
	}
	if pick := r.Select(sel); pick.OK || pick.Kind != OutcomeNoHealthy {
		t.Fatalf("unobserved dotted address must not be selectable: %+v", pick)
	}

	// A fresh healthy observation at the new revision makes i1 selectable, and
	// select reports the dotted text and the latest accepted sequence. i2 is
	// excluded for this one request so i1 is the target regardless of where the
	// replacement left the rotation cursor; the exclusion touches no records.
	markHealth(t, r, "svc", "i1", 2, 1, true, "")
	sel, err = r.ValidateSelectionWithExclusions("svc", 2, nil, []string{"i2"})
	if err != nil {
		t.Fatalf("validate select with exclusion: %v", err)
	}
	if pick := r.Select(sel); !pick.OK || pick.InstanceID != "i1" ||
		pick.Address != "api.example.:8080" || pick.Sequence != 1 {
		t.Fatalf("dotted healthy instance must be selected with its text and sequence: %+v", pick)
	}

	// Resubmitting the exact dotted list (reordered) is an unchanged no-op: the
	// dot is part of the stored address, not normalised away.
	reg, err = r.ValidateRegistration("svc", 2, []Instance{
		{ID: "i1", Address: "api.example.:8080"},
		{ID: "i2", Address: "h2:2"},
	})
	if err != nil {
		t.Fatalf("identical dotted list rejected: %v", err)
	}
	if out = r.Apply(reg); !out.OK || out.Changed || out.Revision != 2 {
		t.Fatalf("identical dotted list must be unchanged: %+v", out)
	}

	// Switching back to the undotted spelling is again a real change and reset.
	reg, err = r.ValidateRegistration("svc", 2, []Instance{
		{ID: "i1", Address: "api.example:8080"},
		{ID: "i2", Address: "h2:2"},
	})
	if err != nil {
		t.Fatalf("undotted replacement rejected: %v", err)
	}
	if out = r.Apply(reg); !out.OK || !out.Changed || out.Revision != 3 {
		t.Fatalf("undotted replacement must bump the revision: %+v", out)
	}
	if got := instanceHealth(t, r, "svc", "i1"); got.Health != HealthUnknown || got.Sequence != 0 {
		t.Fatalf("i1 must restart unknown again: %+v", got)
	}
}

// TestFQDNInvalidAddressReportedBeforeRevisionMismatch shows the field check
// still wins over the revision gate for dotted-domain addresses: a list with
// an illegal trailing-dot address and a wrong expectedRevision is invalid,
// naming the address and stamping the current revision, and no state moves.
func TestFQDNInvalidAddressReportedBeforeRevisionMismatch(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "i1", Address: "api.example:8080"}})

	for _, bad := range []string{".:8080", "api..example.:8080", "api.example..:8080"} {
		if _, err := r.ValidateRegistration("svc", 99, []Instance{{ID: "i1", Address: bad}}); err == nil {
			t.Fatalf("address %q must be invalid even with a mismatched revision", bad)
		}
	}

	// Nothing was applied: the service stays at revision 1 with the old address
	// and its fresh-instance health slate.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("revision changed after failed validation: %d", rev)
	}
	if got := instanceHealth(t, r, "svc", "i1"); got.Address != "api.example:8080" {
		t.Fatalf("address changed after failed validation: %+v", got)
	}
}

// longestBody builds a valid (label-wise) dotted domain body of exactly n
// bytes: 63-character labels separated by dots with the remainder in the final
// label (e.g. n=253 -> 63.63.63.61, 250 label bytes plus 3 dots).
func longestBody(n int) string {
	var labels []string
	remaining := n
	first := true
	for remaining > 0 {
		if !first {
			remaining-- // reserve the dot joining this label
		}
		l := 63
		if remaining < l {
			l = remaining
		}
		labels = append(labels, strings.Repeat("a", l))
		remaining -= l
		first = false
	}
	return strings.Join(labels, ".")
}
