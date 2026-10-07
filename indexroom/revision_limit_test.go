package indexroom

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// This file is the domain-level regression guard for the registration revision
// ceiling. Revisions are carried by int and a real content change increments
// them; at the architecture's maximum (math.MaxInt — 2147483647 on a 32-bit
// build, 9223372036854775807 on a 64-bit build) another increment would
// overflow to a negative revision that no legal expectedRevision could ever
// match again. A content-changing replacement at the cap is therefore refused
// as invalid and changes nothing, while a submission with identical normalized
// content (including a pure reorder or whitespace-only differences) and an
// already-empty list resubmitted as [] still succeeds without bumping; a
// replacement at max-1 still increments to max.
//
// The state is placed directly at the cap: reaching it through billions of
// Apply calls would be impractically slow, and the ceiling semantics are
// independent of how the state got there.

// seedAtRevision builds a service holding insts at exactly revision rev. The
// instances start unknown with sequence 0 and no reason; health records are
// added afterwards with markHealth at the service's revision.
func seedAtRevision(t *testing.T, r *Registry, service string, rev int, insts []Instance) {
	t.Helper()
	st := &serviceState{revision: rev, instances: make(map[string]*instanceState, len(insts))}
	for _, inst := range insts {
		st.instances[inst.ID] = newInstanceState(inst.Address)
	}
	r.services[service] = st
}

// assertInvalidAtLimit checks the refusal shared by every change shape:
// ok:false, error invalid, the reason names the registration-revision limit
// and its value, revision stays the cap, changed is absent, and neither the
// expectedRevision/actualRevision comparison pair nor any target fields appear.
func assertInvalidAtLimit(t *testing.T, out Outcome, rev int) {
	t.Helper()
	if out.OK || out.Kind != OutcomeInvalid {
		t.Fatalf("want invalid at the revision limit, got %+v", out)
	}
	if out.Changed || out.Expected != 0 || out.Actual != 0 || out.RevisionMismatch {
		t.Fatalf("limit rejection must omit changed and the revision pair: %+v", out)
	}
	if out.Revision != rev {
		t.Fatalf("revision must stay at the limit %d, got %d", rev, out.Revision)
	}
	if !strings.Contains(out.Reason, "registration revision has reached its limit") {
		t.Fatalf("reason should state the registration revision limit, got %q", out.Reason)
	}
	if !strings.Contains(out.Reason, strconv.Itoa(rev)) {
		t.Fatalf("reason should name the limit %d, got %q", rev, out.Reason)
	}
}

// TestRegistryRevisionLimitChangeShapesRefused walks the four change shapes the
// task names — adding, removing, changing an address and clearing a non-empty
// list — plus readding a removed instance, and verifies none takes effect.
func TestRegistryRevisionLimitChangeShapesRefused(t *testing.T) {
	const limit = math.MaxInt

	r := NewRegistry()
	seedAtRevision(t, r, "svc", limit, []Instance{
		{ID: "i1", Address: "h1:1"},
		{ID: "i2", Address: "h2:2"},
	})
	markHealth(t, r, "svc", "i1", int64(limit), 11, true, "")
	markHealth(t, r, "svc", "i2", int64(limit), 22, true, "")

	// Establish a session binding and a rotation cursor before the refusals.
	assertPick(t, selectSession(t, r, "svc", int64(limit), "user-42"), "i1", "h1:1", 11)
	assertPick(t, selected(t, r, "svc", int64(limit)), "i2", "h2:2", 22)

	cases := []struct {
		name string
		list []Instance
	}{
		{"add an instance", []Instance{
			{ID: "i1", Address: "h1:1"}, {ID: "i2", Address: "h2:2"}, {ID: "i3", Address: "h3:3"},
		}},
		{"remove an instance", []Instance{{ID: "i1", Address: "h1:1"}}},
		{"change an address", []Instance{
			{ID: "i1", Address: "h1:1"}, {ID: "i2", Address: "h9:9"},
		}},
		{"clear a non-empty list", nil},
		{"remove then readd with a new address", []Instance{
			{ID: "i1", Address: "h1:1"}, {ID: "i2", Address: "h4:4"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reg, err := r.ValidateRegistration("svc", int64(limit), tc.list)
			if err != nil {
				t.Fatalf("content at the cap must validate: %v", err)
			}
			assertInvalidAtLimit(t, r.Apply(reg), limit)

			// No change took effect: revision, list, health, reason, cursor and
			// binding are the ones established beforehand.
			views := r.Snapshot()
			if len(views) != 1 || views[0].Revision != limit {
				t.Fatalf("state changed after refused %s: %+v", tc.name, views)
			}
			if len(views[0].Instances) != 2 {
				t.Fatalf("list changed after refused %s: %+v", tc.name, views[0].Instances)
			}
			if inst := instanceHealth(t, r, "svc", "i1"); inst.Address != "h1:1" ||
				inst.Health != HealthHealthy || inst.Sequence != 11 || inst.Reason != "" {
				t.Fatalf("i1 changed after refused %s: %+v", tc.name, inst)
			}
			if inst := instanceHealth(t, r, "svc", "i2"); inst.Address != "h2:2" ||
				inst.Health != HealthHealthy || inst.Sequence != 22 || inst.Reason != "" {
				t.Fatalf("i2 changed after refused %s: %+v", tc.name, inst)
			}
			// The rejected list rebuilt no map, so the rotation and the bound
			// session consult the same instances with the same records.
			assertPick(t, selectSession(t, r, "svc", int64(limit), "user-42"), "i1", "h1:1", 11)
		})
	}

	// Cursor and bindings are untouched: an ordinary select at the cap continues
	// just after i2 (the last actually rotated id) and wraps to i1 — not i2
	// again, which a rebuilt/reset state would have forced.
	assertPick(t, selected(t, r, "svc", int64(limit)), "i1", "h1:1", 11)
}

// TestRegistryRevisionLimitIdenticalContentSucceeds locks the other half: at
// the cap a matching submission whose normalized content is unchanged is a
// normal success with no changed field and an unchanged revision. This covers
// a plain duplicate, a pure reorder, field trimming, and an already-empty list
// resubmitted as an empty array.
func TestRegistryRevisionLimitIdenticalContentSucceeds(t *testing.T) {
	const limit = math.MaxInt

	r := NewRegistry()
	seedAtRevision(t, r, "svc", limit, []Instance{
		{ID: "i1", Address: "h1:1"},
		{ID: "i2", Address: "h2:2"},
	})
	markHealth(t, r, "svc", "i1", int64(limit), 11, true, "")

	identical := [][]Instance{
		// Plain duplicate.
		{{ID: "i1", Address: "h1:1"}, {ID: "i2", Address: "h2:2"}},
		// Pure reorder: content unchanged by the existing rule.
		{{ID: "i2", Address: "h2:2"}, {ID: "i1", Address: "h1:1"}},
		// Whitespace padding around the service name, ids and addresses is
		// trimmed during validation, so the normalized content is identical.
		{{ID: " i1 ", Address: "  h1:1  "}, {ID: "\ti2\t", Address: " h2:2 "}},
	}
	for i, list := range identical {
		reg, err := r.ValidateRegistration("  svc  ", int64(limit), list)
		if err != nil {
			t.Fatalf("identical case %d must validate: %v", i, err)
		}
		if out := r.Apply(reg); !out.OK || out.Changed || out.Revision != limit {
			t.Fatalf("identical case %d must succeed without a change at the cap: %+v", i, out)
		}
		// Health survives the no-op replacement, and no revision comparison
		// fields appear on the success.
		if inst := instanceHealth(t, r, "svc", "i1"); inst.Health != HealthHealthy || inst.Sequence != 11 {
			t.Fatalf("identical case %d must keep health: %+v", i, inst)
		}
	}

	// An already-empty list resubmitted as [] is content-unchanged too.
	empty := NewRegistry()
	seedAtRevision(t, empty, "ghost", limit, nil)
	reg, err := empty.ValidateRegistration("ghost", int64(limit), nil)
	if err != nil {
		t.Fatalf("empty resubmission must validate: %v", err)
	}
	if out := reg; out.Service != "ghost" || out.Revision != limit {
		t.Fatalf("validated empty resubmission: %+v", out)
	}
	if out := empty.Apply(reg); !out.OK || out.Changed || out.Revision != limit {
		t.Fatalf("empty resubmission at the cap must succeed unchanged: %+v", out)
	}
	views := empty.Snapshot()
	if len(views) != 1 || views[0].Revision != limit || len(views[0].Instances) != 0 {
		t.Fatalf("empty service view: %+v", views)
	}
}

// TestRegistryRevisionLimitMinusOneStillIncrements locks that the cap does not
// clamp ordinary replacements early: a real replacement at max-1 succeeds and
// increments exactly to max, with unchanged-id/address instances keeping their
// health and changed/new instances resetting to unknown/0.
func TestRegistryRevisionLimitMinusOneStillIncrements(t *testing.T) {
	const limit = math.MaxInt

	r := NewRegistry()
	seedAtRevision(t, r, "svc", limit-1, []Instance{
		{ID: "i1", Address: "h1:1"},
		{ID: "i2", Address: "h2:2"},
	})
	markHealth(t, r, "svc", "i1", int64(limit-1), 11, true, "")
	markHealth(t, r, "svc", "i2", int64(limit-1), 22, false, "磁盘故障")

	// i1 keeps id and address; i2 changes address (health resets); i3 is new
	// (unknown/0). The revision climbs exactly from max-1 to max.
	reg, err := r.ValidateRegistration("svc", int64(limit-1), []Instance{
		{ID: "i1", Address: "h1:1"},
		{ID: "i2", Address: "h9:9"},
		{ID: "i3", Address: "h3:3"},
	})
	if err != nil {
		t.Fatalf("replacement at max-1 must validate: %v", err)
	}
	if out := r.Apply(reg); !out.OK || !out.Changed || out.Revision != limit {
		t.Fatalf("replacement at max-1 must increment to the limit: %+v", out)
	}
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Address != "h1:1" ||
		inst.Health != HealthHealthy || inst.Sequence != 11 {
		t.Fatalf("i1 must keep its health record into the limit: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "i2"); inst.Address != "h9:9" ||
		inst.Health != HealthUnknown || inst.Sequence != 0 || inst.Reason != "" {
		t.Fatalf("i2 must reset after the address change: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "i3"); inst.Health != HealthUnknown || inst.Sequence != 0 {
		t.Fatalf("i3 must start unknown/0: %+v", inst)
	}

	// Now at the cap: one further real change (removing i3) is refused, and
	// health at the new revision keeps working on the surviving instances.
	reg, _ = r.ValidateRegistration("svc", int64(limit), []Instance{
		{ID: "i1", Address: "h1:1"},
		{ID: "i2", Address: "h9:9"},
	})
	if out := r.Apply(reg); out.OK || out.Kind != OutcomeInvalid || out.Revision != limit {
		t.Fatalf("further change at the limit must be refused: %+v", out)
	}
	// A health report at revision max is accepted normally (it never bumps the
	// registration revision) and makes the address-changed i2 eligible.
	markHealth(t, r, "svc", "i2", int64(limit), 1, true, "")
	assertPick(t, trySelect(t, r, "svc", int64(limit)), "i1", "h1:1", 11)
	assertPick(t, trySelect(t, r, "svc", int64(limit)), "i2", "h9:9", 1)
}

// TestRegistryRevisionLimitErrorPrecedence locks that the limit rule sits after
// the existing checks: invalid content is still invalid as the field rule, and
// a fully valid list with a mismatched expectedRevision is still a conflict
// carrying the comparison pair — neither is replaced by the limit error.
func TestRegistryRevisionLimitErrorPrecedence(t *testing.T) {
	const limit = math.MaxInt

	r := NewRegistry()
	seedAtRevision(t, r, "svc", limit, []Instance{{ID: "i1", Address: "h1:1"}})

	// Invalid content (bad address) wins as the field-level invalid reason even
	// though accepting the change would also hit the cap.
	reg, err := r.ValidateRegistration("svc", int64(limit), []Instance{{ID: "i1", Address: "bad-address"}})
	if err == nil {
		t.Fatalf("bad address must fail validation")
	}
	if !strings.Contains(err.Error(), "host:port") {
		t.Fatalf("want the address reason, got %q", err.Error())
	}

	// A valid list at the cap with a mismatched expectedRevision is a normal
	// revision conflict with expected/actual and no limit wording; nothing is
	// applied.
	reg, err = r.ValidateRegistration("svc", int64(limit-1), []Instance{{ID: "i2", Address: "h2:2"}})
	if err != nil {
		t.Fatalf("valid list must validate: %v", err)
	}
	out := r.Apply(reg)
	if out.OK || out.Kind != OutcomeConflict || !out.RevisionMismatch ||
		out.Expected != limit-1 || out.Actual != limit || out.Revision != limit {
		t.Fatalf("revision mismatch at the cap must stay a conflict: %+v", out)
	}
	if strings.Contains(out.Reason, "limit") {
		t.Fatalf("conflict reason must not mention the limit: %q", out.Reason)
	}
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Address != "h1:1" {
		t.Fatalf("conflict at the cap must not replace the list: %+v", inst)
	}
}

// TestRegistryRevisionLimitPostRejectionOperations verifies that after a
// refused change the service keeps working under the existing rules at the cap:
// health reports, rotation selection, session reuse and release, and stale
// revisions all behave normally.
func TestRegistryRevisionLimitPostRejectionOperations(t *testing.T) {
	const limit = math.MaxInt

	r := NewRegistry()
	seedAtRevision(t, r, "svc", limit, []Instance{
		{ID: "i1", Address: "h1:1"},
		{ID: "i2", Address: "h2:2"},
	})
	markHealth(t, r, "svc", "i1", int64(limit), 11, true, "")
	markHealth(t, r, "svc", "i2", int64(limit), 22, true, "")

	// Bind a session and advance the shared cursor before the refusal.
	assertPick(t, selectSession(t, r, "svc", int64(limit), "k"), "i1", "h1:1", 11)
	assertPick(t, selected(t, r, "svc", int64(limit)), "i2", "h2:2", 22)

	// Refuse a real change at the cap (address change for i1).
	reg, err := r.ValidateRegistration("svc", int64(limit), []Instance{
		{ID: "i1", Address: "h9:9"}, {ID: "i2", Address: "h2:2"},
	})
	if err != nil {
		t.Fatalf("valid list must validate: %v", err)
	}
	assertInvalidAtLimit(t, r.Apply(reg), limit)

	// A stale revision is still an ordinary conflict carrying the pair.
	upd, _ := r.ValidateHealth("svc", "i1", int64(limit-1), 12, true, "")
	if out := r.ApplyHealth(upd); out.OK || out.Kind != OutcomeConflict ||
		!out.RevisionMismatch || out.Expected != limit-1 || out.Actual != limit || out.Revision != limit {
		t.Fatalf("stale-revision health at the cap: %+v", out)
	}
	// A matching health report is accepted normally.
	markHealth(t, r, "svc", "i1", int64(limit), 12, true, "")

	// The bound session still reuses i1 with the fresh sequence, and the
	// rotation cursor is still just after i2: the next plain select wraps to
	// i1 rather than repeating i2.
	assertPick(t, selectSession(t, r, "svc", int64(limit), "k"), "i1", "h1:1", 12)
	assertPick(t, selected(t, r, "svc", int64(limit)), "i1", "h1:1", 12)

	// Releasing the session succeeds with changed, chooses no target, and moves
	// neither the revision nor the cursor; the key's next selection rotates from
	// the current position (after i1) to i2.
	rel, err := r.ValidateSessionRelease("svc", int64(limit), strPtr("k"))
	if err != nil {
		t.Fatalf("release validation: %v", err)
	}
	relOut := r.ReleaseSession(rel)
	if !relOut.OK || !relOut.Changed || relOut.Revision != limit {
		t.Fatalf("release at the cap: %+v", relOut)
	}
	assertPick(t, selectSession(t, r, "svc", int64(limit), "k"), "i2", "h2:2", 22)

	// Releasing a key that was never bound still succeeds without changed.
	rel, _ = r.ValidateSessionRelease("svc", int64(limit), strPtr("never-bound"))
	if out := r.ReleaseSession(rel); !out.OK || out.Changed || out.Revision != limit {
		t.Fatalf("unbound-key release at the cap: %+v", out)
	}

	// The service snapshot still reports the cap and the original addresses.
	views := r.Snapshot()
	if len(views) != 1 || views[0].Revision != limit || len(views[0].Instances) != 2 {
		t.Fatalf("snapshot after cap operations: %+v", views)
	}
}
