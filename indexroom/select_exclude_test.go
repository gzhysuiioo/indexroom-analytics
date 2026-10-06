package indexroom

import (
	"strings"
	"testing"
)

// selectedExcluding runs one select request carrying the given exclusion list.
func selectedExcluding(t *testing.T, r *Registry, service string, revision int64, excludeIDs []string) SelectOutcome {
	t.Helper()
	sel, err := r.ValidateSelectionWithExclusions(service, revision, nil, excludeIDs)
	if err != nil {
		t.Fatalf("validate select %s: %v", service, err)
	}
	out := r.Select(sel)
	if !out.OK {
		t.Fatalf("select %s: %+v", service, out)
	}
	return out
}

func TestSelectExcludeRotatesPastExcluded(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}, {ID: "c", Address: "h:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	markHealth(t, r, "svc", "c", 1, 3, true, "")

	// First success lands on a; the cursor is a.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" {
		t.Fatalf("first select: %+v", out)
	}
	// Excluding b skips it for this request only: c is next after a.
	out := selectedExcluding(t, r, "svc", 1, []string{"b"})
	if out.InstanceID != "c" || out.Address != "h:3" || out.Sequence != 3 {
		t.Fatalf("exclude b: %+v", out)
	}
	if out.Revision != 1 {
		t.Fatalf("exclusion must not change the revision: %+v", out)
	}
	// A plain request sees b again: after c the rotation wraps to a, then b.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" {
		t.Fatalf("wrap after excluded select: %+v", out)
	}
	if out := selected(t, r, "svc", 1); out.InstanceID != "b" {
		t.Fatalf("b back in rotation: %+v", out)
	}
}

func TestSelectExcludeCursorIdDoesNotReset(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}, {ID: "c", Address: "h:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")
	markHealth(t, r, "svc", "c", 1, 1, true, "")

	// Cursor is a. Excluding the cursor's own id must not reset the rotation:
	// the candidates are b and c, and the one just after a is b.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" {
		t.Fatalf("first select: %+v", out)
	}
	if out := selectedExcluding(t, r, "svc", 1, []string{"a"}); out.InstanceID != "b" {
		t.Fatalf("exclude cursor id: %+v", out)
	}
	// The cursor moved to b with that success, so the next plain select is c.
	if out := selected(t, r, "svc", 1); out.InstanceID != "c" {
		t.Fatalf("continue after excluded select: %+v", out)
	}
}

func TestSelectExcludeAllHealthyIsNoHealthy(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}, {ID: "c", Address: "h:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	// c stays unknown: it is not a candidate and excluding it changes nothing.

	// One success puts the cursor on a.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" {
		t.Fatalf("first select: %+v", out)
	}

	// Every healthy instance excluded: no_healthy, and the reason says the
	// exclusion list removed them all. Unknown ids in the list are ignored.
	sel, err := r.ValidateSelectionWithExclusions("svc", 1, nil, []string{"a", "b", "ghost"})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	out := r.Select(sel)
	if out.OK || out.Kind != OutcomeNoHealthy {
		t.Fatalf("want no_healthy, got %+v", out)
	}
	if !strings.Contains(out.Reason, "excludeInstanceIds") {
		t.Fatalf("reason should name the exclusion list: %q", out.Reason)
	}
	if out.InstanceID != "" || out.Address != "" || out.Sequence != 0 {
		t.Fatalf("failure must not carry a target: %+v", out)
	}
	if out.Revision != 1 {
		t.Fatalf("revision: %+v", out)
	}

	// The failure moved nothing: the excluded instances keep their health
	// records and the cursor still sits on a, so the next plain select is b.
	for _, id := range []string{"a", "b"} {
		if view := instanceHealth(t, r, "svc", id); view.Health != HealthHealthy {
			t.Fatalf("health of %s changed: %+v", id, view)
		}
	}
	if out := selected(t, r, "svc", 1); out.InstanceID != "b" {
		t.Fatalf("cursor preserved after failed exclude: %+v", out)
	}
}

func TestSelectExcludeUnknownIDsIgnored(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")

	// Ids that name no registered instance match nothing: the rotation is the
	// ordinary one. An empty list behaves exactly like an absent field.
	if out := selectedExcluding(t, r, "svc", 1, []string{"ghost", "other"}); out.InstanceID != "a" {
		t.Fatalf("unknown ids ignored: %+v", out)
	}
	if out := selectedExcluding(t, r, "svc", 1, []string{}); out.InstanceID != "b" {
		t.Fatalf("empty list is an ordinary select: %+v", out)
	}
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" {
		t.Fatalf("rotation continues: %+v", out)
	}
}

func TestSelectExcludeValidation(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")

	// A blank-after-trim id rejects the whole list; the reason names the
	// excludeInstanceIds problem and the valid ids in the list do not apply.
	for _, list := range [][]string{{""}, {"a", "  "}, {"\t"}} {
		if _, err := r.ValidateSelectionWithExclusions("svc", 1, nil, list); err == nil {
			t.Fatalf("list %v: want invalid", list)
		} else if !strings.Contains(err.Error(), "excludeInstanceIds") {
			t.Fatalf("list %v: reason should name excludeInstanceIds: %v", list, err)
		}
	}

	// Ids are trimmed and duplicates collapse: " a " and "a" are one id, so
	// only b remains a candidate for this request.
	sel, err := r.ValidateSelectionWithExclusions("svc", 1, nil, []string{" a ", "a"})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if len(sel.ExcludeIDs) != 1 || sel.ExcludeIDs[0] != "a" {
		t.Fatalf("normalized exclusions: %+v", sel.ExcludeIDs)
	}
	if out := r.Select(sel); !out.OK || out.InstanceID != "b" {
		t.Fatalf("trimmed exclusion: %+v", out)
	}
	// The exclusion was per-request: a is chosen again right after.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" {
		t.Fatalf("exclusion scoped to one request: %+v", out)
	}
}

func TestSelectExcludeSessionBoundExcluded(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}, {ID: "c", Address: "h:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")
	markHealth(t, r, "svc", "c", 1, 1, true, "")

	// Bind s to a and t to b; the cursor is b.
	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "a" {
		t.Fatalf("bind s: %+v", out)
	}
	if out := selectedWithSession(t, r, "svc", 1, "t"); out.InstanceID != "b" {
		t.Fatalf("bind t: %+v", out)
	}

	// s's bound instance is excluded this once: the request rotates among the
	// remaining healthy instances (c is next after b) and rebinds s on success.
	sel, err := r.ValidateSelectionWithExclusions("svc", 1, strPtr("s"), []string{"a"})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if out := r.Select(sel); !out.OK || out.InstanceID != "c" {
		t.Fatalf("excluded binding falls back to rotation: %+v", out)
	}
	// The rebound key sticks; t's binding is untouched.
	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "c" {
		t.Fatalf("s rebound to c: %+v", out)
	}
	if out := selectedWithSession(t, r, "svc", 1, "t"); out.InstanceID != "b" {
		t.Fatalf("t keeps its binding: %+v", out)
	}
	// Session reuses never moved the cursor: it is still on c from the
	// fallback rotation, so the next plain select wraps to a.
	if out := selected(t, r, "svc", 1); out.InstanceID != "a" {
		t.Fatalf("cursor after rebound: %+v", out)
	}
}

func TestSelectExcludeSessionFailureKeepsBinding(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h:1"}, {ID: "b", Address: "h:2"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")

	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "a" {
		t.Fatalf("bind s: %+v", out)
	}

	// Excluding every healthy instance fails; the binding and the cursor
	// survive, so the key reuses a again afterwards.
	sel, err := r.ValidateSelectionWithExclusions("svc", 1, strPtr("s"), []string{"a", "b"})
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if out := r.Select(sel); out.OK || out.Kind != OutcomeNoHealthy {
		t.Fatalf("want no_healthy, got %+v", out)
	}
	if out := selectedWithSession(t, r, "svc", 1, "s"); out.InstanceID != "a" {
		t.Fatalf("binding survives failed exclusion: %+v", out)
	}
}
