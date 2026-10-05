package indexroom

import (
	"strings"
	"testing"
)

// This file is the regression guard for the release_session request: an
// explicit operation that drops ONE session binding in ONE service, so the
// key's next selection joins the normal rotation again.
//
// The lifecycle locked here, observed entirely through public results:
//
//   - releasing a bound key reports changed and deletes only that binding; the
//     key's next successful selection rotates from just after the last
//     actually rotated position (not from the smallest id and not from the
//     released binding) and records a fresh binding on success;
//   - releasing a key that has no binding still succeeds, without changed;
//   - the bound instance being removed, unknown, unhealthy, or the service
//     holding an empty instance list never blocks removing the binding — a
//     binding is only a remembered id;
//   - other sessions keep their bindings even when bound to the same
//     instance, and the same key in another service is a different binding;
//   - the release moves neither the rotation cursor nor any health record,
//     and never bumps the registration revision;
//   - revision/conflict/not_found failures state the current revision and
//     preserve the binding, and invalid fields (in particular a missing,
//     null-like or blank sessionKey) are reported before the revision check.

// releaseSession validates and applies one release_session request, failing
// the test when validation itself rejects it (validation cases have their own
// test below).
func releaseSession(t *testing.T, r *Registry, service string, revision int64, key string) ReleaseOutcome {
	t.Helper()
	rel, err := r.ValidateSessionRelease(service, revision, &key)
	if err != nil {
		t.Fatalf("validate release %s key %q: %v", service, key, err)
	}
	return r.ReleaseSession(rel)
}

// TestRegistryReleaseSessionRejoinsRotationAfterCursor locks the headline
// contract: after binding s -> a and a plain rotation to b, releasing s does
// not move the cursor, so the key's next selection continues just after b and
// lands on c rather than restarting at a. The success establishes a new
// binding, which later requests reuse.
func TestRegistryReleaseSessionRejoinsRotationAfterCursor(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	markHealth(t, r, "svc", "c", 1, 3, true, "")

	// First session success rotates to the smallest id and binds s -> a; the
	// shared cursor rests on a.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 1)

	// A plain selection advances the shared cursor to b.
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 2)

	// Releasing the binding reports changed, with the current revision and no
	// target fields.
	rel := releaseSession(t, r, "svc", 1, "s")
	if !rel.OK || !rel.Changed || rel.Revision != 1 {
		t.Fatalf("release of the bound key: %+v", rel)
	}
	if rel.Kind != "" || rel.Reason != "" || rel.Expected != 0 || rel.Actual != 0 {
		t.Fatalf("successful release must carry no failure fields: %+v", rel)
	}

	// The key's next selection behaves like a first binding: it rotates from
	// the last real position (just after b) and lands on c.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "c", "h3:3", 3)

	// The fresh selection rebound the key: the next request reuses c without
	// rotating, and a plain select continues just after c (wrapping to a).
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "c", "h3:3", 3)
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 1)
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "c", "h3:3", 3)
}

// TestRegistryReleaseSessionWithoutBindingSucceeds locks that releasing a key
// that was never bound (or one already released) is still a success with no
// changed field and no revision bump.
func TestRegistryReleaseSessionWithoutBindingSucceeds(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")

	rel := releaseSession(t, r, "  svc  ", 1, "  ghost  ")
	if !rel.OK || rel.Changed || rel.Revision != 1 || rel.Service != "svc" {
		t.Fatalf("releasing an unbound key: %+v", rel)
	}

	// The no-op release moved neither cursor nor state: a keyed selection with
	// the released name rotates from scratch (smallest id) and binds, exactly
	// as any first use would.
	assertPick(t, selectSession(t, r, "svc", 1, "ghost"), "a", "h1:1", 1)

	// Releasing again now that the key is bound reports changed, and a third
	// release is back to unchanged — idempotent from the second call on.
	if rel := releaseSession(t, r, "svc", 1, "ghost"); !rel.OK || !rel.Changed {
		t.Fatalf("second release should remove the fresh binding: %+v", rel)
	}
	if rel := releaseSession(t, r, "svc", 1, "ghost"); !rel.OK || rel.Changed {
		t.Fatalf("third release must succeed unchanged: %+v", rel)
	}
}

// TestRegistryReleaseSessionIgnoresBoundInstanceState locks every
// cannot-block condition: the bound instance removed by a replacement,
// currently unhealthy, or the instance list emptied entirely — the binding
// itself still comes off and reports changed in all of them.
func TestRegistryReleaseSessionIgnoresBoundInstanceState(t *testing.T) {
	t.Run("bound instance unhealthy", func(t *testing.T) {
		r := NewRegistry()
		registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
		markHealth(t, r, "svc", "a", 1, 1, true, "")
		markHealth(t, r, "svc", "b", 1, 1, true, "")
		assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 1)

		// a is unhealthy: selection could not reuse the binding, but release
		// must still remove it.
		markHealth(t, r, "svc", "a", 1, 2, false, "down")
		if rel := releaseSession(t, r, "svc", 1, "s"); !rel.OK || !rel.Changed || rel.Revision != 1 {
			t.Fatalf("release against an unhealthy bound instance: %+v", rel)
		}
		// Nothing healthy but b: a fresh rotation from the cursor (still on a,
		// the only real rotation) continues to b, proving the binding is gone.
		assertPick(t, selectSession(t, r, "svc", 1, "s"), "b", "h2:2", 1)
	})

	t.Run("bound instance removed", func(t *testing.T) {
		r := NewRegistry()
		registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
		markHealth(t, r, "svc", "a", 1, 1, true, "")
		markHealth(t, r, "svc", "b", 1, 1, true, "")
		assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 1)

		// Replace a away (content change bumps to revision 2).
		registerService(t, r, "svc", 1, []Instance{{ID: "b", Address: "h2:2"}})
		if rel := releaseSession(t, r, "svc", 2, "s"); !rel.OK || !rel.Changed || rel.Revision != 2 {
			t.Fatalf("release pointing at a removed instance: %+v", rel)
		}
	})

	t.Run("never confirmed healthy", func(t *testing.T) {
		r := NewRegistry()
		// Build the binding to a, then replace the list with the same ids at a
		// NEW address only for a: a resets to unknown/0, while the binding
		// still names a. Releasing must succeed despite the unknown target.
		registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}})
		markHealth(t, r, "svc", "a", 1, 1, true, "")
		assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 1)
		registerService(t, r, "svc", 1, []Instance{{ID: "a", Address: "h9:9"}})
		if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthUnknown || inst.Sequence != 0 {
			t.Fatalf("a should be unknown/0 after the address change: %+v", inst)
		}
		if rel := releaseSession(t, r, "svc", 2, "s"); !rel.OK || !rel.Changed || rel.Revision != 2 {
			t.Fatalf("release pointing at an unknown rebound instance: %+v", rel)
		}
	})

	t.Run("empty instance list", func(t *testing.T) {
		r := NewRegistry()
		registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}})
		markHealth(t, r, "svc", "a", 1, 1, true, "")
		assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 1)

		// Empty the list: the service survives at revision 2, and releasing on
		// it must still remove the dangling binding.
		registerService(t, r, "svc", 1, nil)
		if rel := releaseSession(t, r, "svc", 2, "s"); !rel.OK || !rel.Changed || rel.Revision != 2 {
			t.Fatalf("release on an empty-list service: %+v", rel)
		}
		// A repeat release on the emptied service is an unchanged success, not
		// not_found: the service still exists at its real revision.
		if rel := releaseSession(t, r, "svc", 2, "s"); !rel.OK || rel.Changed {
			t.Fatalf("repeat release on the emptied service: %+v", rel)
		}
	})
}

// TestRegistryReleaseSessionIsolatesSessionsAndServices locks scope: only the
// named key in the named service loses its binding. Another session bound to
// the same instance is untouched, and the identical key under another service
// keeps its own binding.
func TestRegistryReleaseSessionIsolatesSessionsAndServices(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	markHealth(t, r, "svc", "c", 1, 3, true, "")
	registerService(t, r, "other", 0, []Instance{{ID: "a", Address: "o1:1"}, {ID: "b", Address: "o2:2"}})
	markHealth(t, r, "other", "a", 1, 1, true, "")
	markHealth(t, r, "other", "b", 1, 2, true, "")
	// svc: x -> a then y -> b (the shared cursor rests on b). other: k -> a.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "other", 1, "x"), "a", "o1:1", 1)

	if rel := releaseSession(t, r, "svc", 1, "x"); !rel.OK || !rel.Changed {
		t.Fatalf("release x: %+v", rel)
	}

	// y keeps its own binding even though it is a different key, and x's
	// removal did not move the shared cursor.
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// The same key under another service is unaffected: it reuses its own a.
	assertPick(t, selectSession(t, r, "other", 1, "x"), "a", "o1:1", 1)

	// Releasing x in "other" is a separate binding and reports changed on its
	// own; svc's released x stays released.
	if rel := releaseSession(t, r, "other", 1, "x"); !rel.OK || !rel.Changed {
		t.Fatalf("release x in other service: %+v", rel)
	}
	if rel := releaseSession(t, r, "svc", 1, "x"); !rel.OK || rel.Changed {
		t.Fatalf("svc x was already released: %+v", rel)
	}

	// svc's released x now joins the rotation at the cursor (just after y's
	// b) and binds to c; y is still pinned to b.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "c", "h3:3", 3)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "c", "h3:3", 3)
}

// TestRegistryReleaseSessionKeepsCursorRevisionAndHealth locks that a release
// changes nothing beyond the single binding: the plain rotation continues
// from wherever the last real selection left it, revision is untouched, and
// every health record is exactly as the observations set it.
func TestRegistryReleaseSessionKeepsCursorRevisionAndHealth(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 10, true, "")
	markHealth(t, r, "svc", "b", 1, 20, true, "")
	markHealth(t, r, "svc", "c", 1, 30, false, "down")

	// s -> a, then a plain rotation -> b; the cursor rests on b.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 10)
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 20)

	if rel := releaseSession(t, r, "svc", 1, "s"); !rel.OK || !rel.Changed || rel.Revision != 1 {
		t.Fatalf("release: %+v", rel)
	}

	// A plain selection continues just after the last real rotation (b); c is
	// unhealthy, so the rotation wraps to a. The release neither advanced nor
	// rewound the cursor.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 10)

	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("release changed the registration revision: %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst != (InstanceView{ID: "a", Address: "h1:1", Health: HealthHealthy, Sequence: 10}) {
		t.Fatalf("a record changed by release: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst != (InstanceView{ID: "b", Address: "h2:2", Health: HealthHealthy, Sequence: 20}) {
		t.Fatalf("b record changed by release: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "c"); inst != (InstanceView{ID: "c", Address: "h3:3", Health: HealthUnhealthy, Sequence: 30, Reason: "down"}) {
		t.Fatalf("c record changed by release: %+v", inst)
	}
}

// TestRegistryReleaseSessionFailuresPreserveBinding locks the revision gate
// shared with health and select: a mismatch conflicts carrying both revisions,
// an unknown service at expectedRevision 0 is not_found, and either failure
// leaves the existing binding and cursor exactly as they were for the next
// request in the batch.
func TestRegistryReleaseSessionFailuresPreserveBinding(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 1)

	// Wrong revision on an existing service: conflict carrying current 1 and
	// the submitted revision; the binding survives.
	if rel := releaseSession(t, r, "svc", 9, "s"); rel.OK || rel.Kind != OutcomeConflict ||
		rel.Expected != 9 || rel.Actual != 1 || rel.Revision != 1 || rel.Changed || rel.Reason == "" {
		t.Fatalf("wrong revision release: %+v", rel)
	}
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 1)

	// Unknown service, non-zero revision: conflict against actual revision 0.
	if rel := releaseSession(t, r, "ghost", 3, "s"); rel.OK || rel.Kind != OutcomeConflict ||
		rel.Expected != 3 || rel.Actual != 0 || rel.Revision != 0 || rel.Reason == "" {
		t.Fatalf("unknown service wrong revision: %+v", rel)
	}

	// Unknown service, expectedRevision 0: not_found at revision 0.
	if rel := releaseSession(t, r, "ghost", 0, "s"); rel.OK || rel.Kind != OutcomeNotFound ||
		rel.Revision != 0 || rel.Reason == "" {
		t.Fatalf("unknown service expected 0: %+v", rel)
	}

	// Later requests keep processing against the preserved state: the correct
	// revision now releases the binding, and the key afterwards rotates.
	if rel := releaseSession(t, r, "svc", 1, "s"); !rel.OK || !rel.Changed {
		t.Fatalf("release after failures should succeed: %+v", rel)
	}
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "b", "h2:2", 2)
}

// TestRegistryReleaseSessionValidation locks content validation and its
// precedence over the revision comparison.
func TestRegistryReleaseSessionValidation(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "a", Address: "h1:1"}})
	markHealth(t, r, "svc", "a", 1, 1, true, "")

	if _, err := r.ValidateSessionRelease("   ", 1, strPtr("s")); err == nil {
		t.Fatalf("blank service should be invalid")
	}
	// sessionKey is mandatory: a nil (absent) key or a blank/empty key is
	// invalid with a reason naming sessionKey. A blank key with a
	// wrong-but-in-range revision still reports the key problem: field
	// validity precedes the revision comparison (99 only conflicts later, in
	// Apply — see below).
	for name, key := range map[string]*string{
		"absent":               nil,
		"blank":                strPtr("   "),
		"empty":                strPtr(""),
		"blank wrong revision": strPtr("  "),
	} {
		revision := int64(1)
		if name == "blank wrong revision" {
			revision = 99
		}
		_, err := r.ValidateSessionRelease("svc", revision, key)
		if err == nil {
			t.Fatalf("%s key should be invalid", name)
		}
		if got := err.Error(); !strings.Contains(got, "sessionKey") {
			t.Fatalf("%s reason should name sessionKey, got %q", name, got)
		}
	}
	// The revision range is checked before the key (the order shared with
	// select): an out-of-range revision wins over a simultaneously blank key.
	if _, err := r.ValidateSessionRelease("svc", -1, strPtr("   ")); err == nil ||
		!strings.Contains(err.Error(), "expectedRevision") {
		t.Fatalf("out-of-range revision should precede the blank key, got %v", err)
	}
	// A valid key with a wrong-but-in-range revision passes validation; the
	// mismatch is a business conflict applied later, not an invalid error.
	rel, err := r.ValidateSessionRelease("  svc  ", 99, strPtr("  s  "))
	if err != nil {
		t.Fatalf("valid fields should validate despite the revision mismatch: %v", err)
	}
	if rel.Service != "svc" || rel.SessionKey != "s" || rel.Revision != 99 {
		t.Fatalf("trim/valid mismatch: %+v", rel)
	}
	if out := r.ReleaseSession(rel); out.OK || out.Kind != OutcomeConflict {
		t.Fatalf("validated wrong revision should conflict at apply time: %+v", out)
	}
}
