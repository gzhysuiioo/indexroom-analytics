package indexroom

import (
	"strconv"
	"strings"
	"testing"
)

// serviceAtRevision seeds a service directly at the requested revision with the
// given instances, bypassing the per-replacement +1 increment so behavior at
// maxRevision can be exercised deterministically (the limit is unreachable by
// ordinary increments). The instances start unknown; callers attach health
// observations themselves.
func serviceAtRevision(t *testing.T, r *Registry, service string, revision int, instances []Instance) {
	t.Helper()
	reg, err := r.ValidateRegistration(service, 0, instances)
	if err != nil {
		t.Fatalf("seed registration: %v", err)
	}
	if out := r.Apply(reg); !out.OK {
		t.Fatalf("seed registration rejected: %+v", out)
	}
	r.services[service].revision = revision
}

// assertLimitInvalid checks the ceiling rejection: invalid (never a conflict),
// a reason saying the registration revision reached its limit while naming that
// limit, the limit kept as the current revision, no changed flag and no
// revision-comparison pair.
func assertLimitInvalid(t *testing.T, out Outcome) {
	t.Helper()
	if out.OK || out.Kind != OutcomeInvalid {
		t.Fatalf("want invalid ceiling rejection, got %+v", out)
	}
	if !strings.Contains(out.Reason, "registration revision has reached the limit") {
		t.Fatalf("reason should explain the revision limit, got %q", out.Reason)
	}
	if !strings.Contains(out.Reason, "cannot save") {
		t.Fatalf("reason should say the list change cannot be saved, got %q", out.Reason)
	}
	limit := strconv.Itoa(maxRevision)
	if !strings.Contains(out.Reason, limit) {
		t.Fatalf("reason should include the limit %s, got %q", limit, out.Reason)
	}
	if out.Revision != maxRevision {
		t.Fatalf("revision must stay at the limit %d, got %d", maxRevision, out.Revision)
	}
	if out.Changed {
		t.Fatalf("a rejected replacement must not report changed: %+v", out)
	}
	if out.Expected != 0 || out.Actual != 0 || out.RevisionMismatch {
		t.Fatalf("ceiling rejection must carry no revision-comparison pair: %+v", out)
	}
}

// TestApplyAtRevisionLimitRejectsRealChanges: once the current revision equals
// the architecture limit, every normalized list that actually differs must be
// refused. Add, delete, address change and clearing a non-empty list all fail
// as invalid; the list, health records (state, sequence, reason), rotation
// cursor and session bindings stay exactly as they were.
func TestApplyAtRevisionLimitRejectsRealChanges(t *testing.T) {
	r := NewRegistry()
	serviceAtRevision(t, r, "svc", maxRevision, []Instance{
		{ID: "i1", Address: "h1:1"},
		{ID: "i2", Address: "h2:2"},
	})
	st := r.services["svc"]
	st.instances["i1"].health = HealthHealthy
	st.instances["i1"].sequence = 5
	st.instances["i2"].health = HealthUnhealthy
	st.instances["i2"].sequence = 7
	st.instances["i2"].reason = "connection refused"
	// A live rotation position and a session binding, both of which the
	// rejection must leave alone.
	st.cursor = "i1"
	st.cursorSet = true
	st.sessions = map[string]string{"user-7": "i1"}

	apply := func(instances []Instance) Outcome {
		reg, err := r.ValidateRegistration("svc", int64(maxRevision), instances)
		if err != nil {
			t.Fatalf("registration at the limit should validate: %v", err)
		}
		return r.Apply(reg)
	}

	cases := map[string][]Instance{
		"address change":   {{ID: "i1", Address: "h9:9"}, {ID: "i2", Address: "h2:2"}},
		"added instance":   {{ID: "i1", Address: "h1:1"}, {ID: "i2", Address: "h2:2"}, {ID: "i3", Address: "h3:3"}},
		"removed instance": {{ID: "i1", Address: "h1:1"}},
		"cleared list":     {},
	}
	for name, instances := range cases {
		t.Run(name, func(t *testing.T) {
			assertLimitInvalid(t, apply(instances))
		})
	}

	// Nothing the rejections proposed is visible: revision, list, health,
	// sequence and reason.
	views := r.Snapshot()
	if len(views) != 1 || views[0].Revision != maxRevision {
		t.Fatalf("snapshot after rejections: %+v", views)
	}
	got := views[0].Instances
	if len(got) != 2 {
		t.Fatalf("list must be untouched, got %+v", got)
	}
	if got[0].ID != "i1" || got[0].Address != "h1:1" || got[0].Health != HealthHealthy || got[0].Sequence != 5 || got[0].Reason != "" {
		t.Fatalf("i1 state must be untouched: %+v", got[0])
	}
	if got[1].ID != "i2" || got[1].Address != "h2:2" || got[1].Health != HealthUnhealthy || got[1].Sequence != 7 || got[1].Reason != "connection refused" {
		t.Fatalf("i2 state must be untouched: %+v", got[1])
	}
	// Cursor and session binding survive.
	if st.cursor != "i1" || !st.cursorSet || st.sessions["user-7"] != "i1" {
		t.Fatalf("cursor/session must be untouched: cursor=%q set=%v sessions=%v", st.cursor, st.cursorSet, st.sessions)
	}

	// After the rejections the service remains fully operable at the limit:
	// health reports land, the bound session reuses its instance, a plain
	// select rotates on, and releasing the binding works.
	upd, err := r.ValidateHealth("svc", "i2", int64(maxRevision), 8, true, "")
	if err != nil {
		t.Fatalf("health validation: %v", err)
	}
	if out := r.ApplyHealth(upd); !out.OK || !out.Changed || out.Sequence != 8 || out.Revision != maxRevision {
		t.Fatalf("health report at the limit: %+v", out)
	}
	sel, err := r.ValidateSelectionWithSession("svc", int64(maxRevision), strPtr("user-7"))
	if err != nil {
		t.Fatalf("session select validation: %v", err)
	}
	if out := r.Select(sel); !out.OK || out.InstanceID != "i1" || out.Address != "h1:1" || out.Sequence != 5 || out.Revision != maxRevision {
		t.Fatalf("bound session should still select i1: %+v", out)
	}
	rel, err := r.ValidateSessionRelease("svc", int64(maxRevision), strPtr("user-7"))
	if err != nil {
		t.Fatalf("release validation: %v", err)
	}
	if out := r.ReleaseSession(rel); !out.OK || !out.Changed || out.Revision != maxRevision {
		t.Fatalf("session release at the limit: %+v", out)
	}
}

// TestApplyAtRevisionLimitAcceptsUnchangedList locks the no-change success at
// the ceiling: identical content, a reordering, a resubmission whose fields
// only differ by surrounding whitespace, and an empty list resubmitted as empty
// all succeed without bumping the revision or reporting changed.
func TestApplyAtRevisionLimitAcceptsUnchangedList(t *testing.T) {
	r := NewRegistry()
	serviceAtRevision(t, r, "svc", maxRevision, []Instance{
		{ID: "i1", Address: "h1:1"},
		{ID: "i2", Address: "h2:2"},
	})
	st := r.services["svc"]
	st.instances["i1"].health = HealthHealthy
	st.instances["i1"].sequence = 9

	applyRaw := func(instances []Instance) Outcome {
		reg, err := r.ValidateRegistration("svc", int64(maxRevision), instances)
		if err != nil {
			t.Fatalf("unchanged registration should validate: %v", err)
		}
		return r.Apply(reg)
	}

	same := []Instance{{ID: "i1", Address: "h1:1"}, {ID: "i2", Address: "h2:2"}}
	for name, instances := range map[string][]Instance{
		"identical": same,
		"reordered": {{ID: "i2", Address: "h2:2"}, {ID: "i1", Address: "h1:1"}},
		"trimmed":   {{ID: "  i1  ", Address: "  h1:1  "}, {ID: "i2", Address: "h2:2"}},
	} {
		t.Run(name, func(t *testing.T) {
			out := applyRaw(instances)
			if !out.OK || out.Changed || out.Revision != maxRevision {
				t.Fatalf("unchanged list at the limit should succeed quietly: %+v", out)
			}
		})
	}

	// Health must survive the no-change resubmissions.
	views := r.Snapshot()
	if len(views) != 1 || views[0].Revision != maxRevision || len(views[0].Instances) != 2 {
		t.Fatalf("snapshot: %+v", views)
	}
	if inst := views[0].Instances[0]; inst.ID != "i1" || inst.Health != HealthHealthy || inst.Sequence != 9 {
		t.Fatalf("health record must survive unchanged resubmission: %+v", inst)
	}

	// An already-empty list resubmitted with an empty array is unchanged too.
	re := NewRegistry()
	serviceAtRevision(t, re, "empty", maxRevision, nil)
	reg, err := re.ValidateRegistration("empty", int64(maxRevision), []Instance{})
	if err != nil {
		t.Fatalf("empty-list validation: %v", err)
	}
	if out := re.Apply(reg); !out.OK || out.Changed || out.Revision != maxRevision {
		t.Fatalf("empty list resubmission at the limit: %+v", out)
	}
	if views := re.Snapshot(); len(views) != 1 || views[0].Revision != maxRevision || len(views[0].Instances) != 0 {
		t.Fatalf("empty service must stay empty at the limit: %+v", views)
	}
}

// TestApplyOneBelowLimitIncrementsToLimit: the final real replacement still
// succeeds when the revision is maxRevision-1, bumping exactly to the limit and
// preserving health for id+address survivors while resetting changed and new
// instances. The next real change is then refused.
func TestApplyOneBelowLimitIncrementsToLimit(t *testing.T) {
	r := NewRegistry()
	serviceAtRevision(t, r, "svc", maxRevision-1, []Instance{
		{ID: "i1", Address: "h1:1"},
		{ID: "i2", Address: "h2:2"},
	})
	st := r.services["svc"]
	st.instances["i1"].health = HealthHealthy
	st.instances["i1"].sequence = 42
	st.instances["i2"].health = HealthHealthy
	st.instances["i2"].sequence = 43

	reg, err := r.ValidateRegistration("svc", int64(maxRevision-1), []Instance{
		{ID: "i1", Address: "h1:1"}, // untouched: keeps its observation
		{ID: "i2", Address: "h9:9"}, // address change: resets
		{ID: "i3", Address: "h3:3"}, // new: starts unknown
	})
	if err != nil {
		t.Fatalf("final-step registration should validate: %v", err)
	}
	out := r.Apply(reg)
	if !out.OK || !out.Changed || out.Revision != maxRevision {
		t.Fatalf("replacement at maxRevision-1 should reach the limit, got %+v", out)
	}
	views := r.Snapshot()
	if len(views) != 1 || views[0].Revision != maxRevision || len(views[0].Instances) != 3 {
		t.Fatalf("snapshot after final increment: %+v", views)
	}
	byID := map[string]InstanceView{}
	for _, inst := range views[0].Instances {
		byID[inst.ID] = inst
	}
	if i1 := byID["i1"]; i1.Address != "h1:1" || i1.Health != HealthHealthy || i1.Sequence != 42 {
		t.Fatalf("surviving instance should keep its observation: %+v", i1)
	}
	if i2 := byID["i2"]; i2.Address != "h9:9" || i2.Health != HealthUnknown || i2.Sequence != 0 || i2.Reason != "" {
		t.Fatalf("address-changed instance should reset: %+v", i2)
	}
	if i3 := byID["i3"]; i3.Health != HealthUnknown || i3.Sequence != 0 {
		t.Fatalf("new instance should start unknown/0: %+v", i3)
	}

	// One more real change now hits the ceiling and changes nothing.
	reg, err = r.ValidateRegistration("svc", int64(maxRevision), []Instance{{ID: "i1", Address: "h1:1"}})
	if err != nil {
		t.Fatalf("registration at the limit should validate: %v", err)
	}
	assertLimitInvalid(t, r.Apply(reg))
	if views := r.Snapshot(); len(views[0].Instances) != 3 || views[0].Revision != maxRevision {
		t.Fatalf("rejected change at the limit must leave state intact: %+v", views)
	}

	// An unchanged submission still succeeds at the limit.
	reg, err = r.ValidateRegistration("svc", int64(maxRevision), []Instance{
		{ID: "i1", Address: "h1:1"},
		{ID: "i2", Address: "h9:9"},
		{ID: "i3", Address: "h3:3"},
	})
	if err != nil {
		t.Fatalf("unchanged registration should validate: %v", err)
	}
	if out := r.Apply(reg); !out.OK || out.Changed || out.Revision != maxRevision {
		t.Fatalf("unchanged submission after rejection: %+v", out)
	}
}

// TestApplyAtRevisionLimitConflictPrecedence: a valid list submitted with a
// revision other than the limit is still an ordinary revision-mismatch
// conflict carrying the comparison pair; the ceiling rule must never overwrite
// that conflict even when the list content also differs.
func TestApplyAtRevisionLimitConflictPrecedence(t *testing.T) {
	r := NewRegistry()
	serviceAtRevision(t, r, "svc", maxRevision, []Instance{{ID: "i1", Address: "h1:1"}})

	reg, err := r.ValidateRegistration("svc", int64(maxRevision)-1, []Instance{{ID: "i1", Address: "h9:9"}})
	if err != nil {
		t.Fatalf("mismatching registration should still validate: %v", err)
	}
	out := r.Apply(reg)
	if out.OK || out.Kind != OutcomeConflict || !out.RevisionMismatch {
		t.Fatalf("revision mismatch must stay a conflict at the limit: %+v", out)
	}
	if out.Expected != maxRevision-1 || out.Actual != maxRevision || out.Revision != maxRevision {
		t.Fatalf("conflict must carry the original comparison pair: %+v", out)
	}
	if views := r.Snapshot(); views[0].Revision != maxRevision ||
		views[0].Instances[0].Address != "h1:1" {
		t.Fatalf("conflict must change nothing: %+v", views)
	}

	// Field invalidity still outranks everything, including the ceiling.
	if _, err := r.ValidateRegistration("  ", int64(maxRevision), []Instance{{ID: "i1", Address: "h9:9"}}); err == nil {
		t.Fatalf("blank service name must remain invalid at the limit")
	}
	if _, err := r.ValidateRegistration("svc", int64(maxRevision), []Instance{{ID: "i1", Address: "bad-address"}}); err == nil {
		t.Fatalf("malformed address must remain invalid at the limit")
	}
	if views := r.Snapshot(); views[0].Revision != maxRevision ||
		views[0].Instances[0].Address != "h1:1" {
		t.Fatalf("invalid fields must change nothing at the limit: %+v", views)
	}
}
