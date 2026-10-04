package indexroom

import "testing"

// This file is the regression guard for several session keys interleaved
// against ONE service. The per-service session map already binds each trimmed
// key independently, but the earlier suite only exercised a single key per
// service (or the same key in different services). These tests lock the
// cross-session guarantees purely through the public registration, health and
// selection results:
//
//   - two distinct keys first succeed by consuming the same rotation in turn,
//     each remembering its own instance; an existing binding never chooses the
//     target for a new key;
//   - interleaved keyed reuses return each session's own current binding
//     (id, address and latest accepted health sequence) without moving the
//     shared rotation, and plain selects continue from the last real rotation;
//   - when one key's bound instance turns unhealthy, only that key's binding is
//     rewritten by a fallback rotation, and two keys may legitimately bind to
//     the same instance without stealing each other's binding;
//   - an all-unhealthy no_healthy failure fabricates no target and preserves
//     both sessions' bindings and the rotation position.
//
// No session query, release or expiry operation exists or is introduced: the
// bindings are observed only through what later selections return.

// selectSession validates and runs one select carrying a session key,
// returning the outcome (success or failure) so failure cases can be asserted
// directly instead of failing the helper.
func selectSession(t *testing.T, r *Registry, service string, revision int, key string) SelectOutcome {
	t.Helper()
	sel, err := r.ValidateSelectionWithSession(service, revision, &key)
	if err != nil {
		t.Fatalf("validate select %s key %q: %v", service, key, err)
	}
	return r.Select(sel)
}

// assertPick locks one successful selection's full target triple: instance id,
// current address and the instance's currently accepted health sequence must
// all belong to the same instance chosen for this request.
func assertPick(t *testing.T, out SelectOutcome, id, addr string, seq int64) {
	t.Helper()
	if !out.OK {
		t.Fatalf("select should succeed, got %+v", out)
	}
	if out.InstanceID != id || out.Address != addr || out.Sequence != seq {
		t.Fatalf("select target = %q/%s/seq %d, want %q/%s/seq %d (outcome %+v)",
			out.InstanceID, out.Address, out.Sequence, id, addr, seq, out)
	}
}

// TestRegistryTwoSessionsShareOneRotationAndStayIndependent fixes the
// interleaving contract for two keys in one service. Both keys' first
// successes consume the same rotation in input order; later reuses never move
// it; plain selections continue just after the last instance an actual
// rotation returned; and every result carries the selected instance's own
// address and sequence rather than the other session's.
func TestRegistryTwoSessionsShareOneRotationAndStayIndependent(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	markHealth(t, r, "svc", "c", 1, 3, true, "")

	// The first key rotates from the unset position to the smallest id and
	// binds x -> a; the shared cursor rests on a.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)

	// The second key is a different session: the existing x -> a binding must
	// not pick its target. Its first success continues the SAME rotation from
	// the current position, landing on b, and binds y -> b.
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// Interleaved reuses return each session's own binding and move nothing.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// A plain select continues just after the last rotated id (b), proving the
	// reuses above neither advanced nor rewound the rotation.
	assertPick(t, selected(t, r, "svc", 1), "c", "h3:3", 3)

	// Reusing the earlier-bound instance (a) must not drag a later plain select
	// backwards, and the other session's reuse must not skip a healthy
	// instance: both reuses still answer their own bindings while the cursor
	// stays on c.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// Plain rotation wraps past c to a, then continues to b exactly once each:
	// no reuse inserted an extra skip.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 1)
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 2)

	// Newer accepted observations on the bound instances are reflected per
	// session: x reports a's current record and y reports b's, never a mix.
	markHealth(t, r, "svc", "a", 1, 10, true, "")
	markHealth(t, r, "svc", "b", 1, 20, true, "")
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 10)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 20)

	// The plain rotation is still where the last real rotation left it (b), so
	// it continues to c and then wraps to a carrying a's latest sequence.
	assertPick(t, selected(t, r, "svc", 1), "c", "h3:3", 3)
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 10)

	// Selections changed neither the revision nor any accepted health record.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("selection changed the registration revision: %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthHealthy || inst.Sequence != 10 || inst.Reason != "" {
		t.Fatalf("a accepted record changed by selection: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst.Health != HealthHealthy || inst.Sequence != 20 {
		t.Fatalf("b accepted record changed by selection: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "c"); inst.Health != HealthHealthy || inst.Sequence != 3 {
		t.Fatalf("c accepted record changed by selection: %+v", inst)
	}
}

// TestRegistrySessionUnhealthyRebindIsolatesAndMayShare drives one session's
// bound instance unhealthy while another session's instance stays healthy. The
// affected session alone reselects from the current rotation and rewrites only
// its own binding; the two sessions are then allowed to converge on the same
// instance without it being a conflict or stealing the other binding. The
// originally bound instance recovering afterwards does not move the rebound
// session, and a plain select continues from the successful reselection's
// position.
func TestRegistrySessionUnhealthyRebindIsolatesAndMayShare(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	markHealth(t, r, "svc", "c", 1, 3, true, "")

	// x -> a, then y -> b; the shared cursor rests on b.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// Only a (x's binding) receives a newer unhealthy observation; b and c
	// remain healthy.
	markHealth(t, r, "svc", "a", 1, 4, false, "down")

	// x's next selection cannot reuse a and falls back to the rotation at its
	// current position (just after b), choosing c. Only x's binding moves; the
	// returned triple is c's, never stale data from a.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "c", "h3:3", 3)

	// y is unaffected: its own binding to b is still returned.
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "c", "h3:3", 3)

	// c (now x's binding) turns unhealthy too, leaving b as the sole healthy
	// instance. x's fallback continues just after c, wraps and rebinds to b.
	markHealth(t, r, "svc", "c", 1, 5, false, "down")
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "b", "h2:2", 2)

	// Two different sessions bound to the same instance is allowed: it is not
	// a conflict and neither request displaces the other session's binding.
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "b", "h2:2", 2)

	// The original instance a accepts a newer healthy observation and
	// recovers. The session that moved (x) keeps reusing its new binding b;
	// recovery never steals it back. The other session keeps its own binding.
	markHealth(t, r, "svc", "a", 1, 6, true, "")
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// The plain rotation continues from the position set by x's successful
	// reselection (cursor on b): with a and b healthy it wraps to a, carrying
	// a's recovered sequence rather than restarting at a's old record.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 6)

	// The plain rotation rewrote no session binding: both sessions still
	// answer b even though the cursor moved through a.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "b", "h2:2", 2)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// The selections and binding changes left the revision and the accepted
	// health records exactly as the observations set them.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("selection changed the registration revision: %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthHealthy || inst.Sequence != 6 || inst.Reason != "" {
		t.Fatalf("a final record: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst.Health != HealthHealthy || inst.Sequence != 2 {
		t.Fatalf("b final record: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "c"); inst.Health != HealthUnhealthy || inst.Sequence != 5 || inst.Reason != "down" {
		t.Fatalf("c final record: %+v", inst)
	}
}

// TestRegistryTwoSessionsNoHealthyKeepsBothBindingsAndCursor isolates the
// failure path shared by two sessions: once every instance is unhealthy, keyed
// and plain selections all return no_healthy with the current revision and a
// reason but no fabricated instance, address or sequence. The failures
// preserve both sessions' prior bindings and the shared rotation position.
//
// Recovery is ordered so a surviving binding is distinguishable from a fresh
// rotation: an instance no session is bound to (c) recovers first, then x's
// bound a recovers while the cursor still rests on b. A request that reused
// the preserved binding returns a; one that had lost it would rotate just
// after b to c instead. After y's bound b recovers both sessions reuse their
// own targets with the latest sequences, and a plain select resumes from the
// last real rotation at c rather than anywhere the failures or reuses moved.
func TestRegistryTwoSessionsNoHealthyKeepsBothBindingsAndCursor(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 2, true, "")
	markHealth(t, r, "svc", "c", 1, 3, true, "")

	// x -> a then y -> b; the last real rotation parked the shared cursor on b.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 2)

	// Every instance receives a newer unhealthy observation.
	markHealth(t, r, "svc", "a", 1, 4, false, "down")
	markHealth(t, r, "svc", "b", 1, 5, false, "down")
	markHealth(t, r, "svc", "c", 1, 6, false, "down")

	// Keyed and plain requests all fail the same way: no_healthy at the
	// current revision with a stated reason, inventing neither an instance id
	// nor an address nor a sequence.
	assertNoHealthy := func(key *string) {
		t.Helper()
		var out SelectOutcome
		if key == nil {
			sel, err := r.ValidateSelection("svc", 1)
			if err != nil {
				t.Fatalf("validate plain select: %v", err)
			}
			out = r.Select(sel)
		} else {
			out = selectSession(t, r, "svc", 1, *key)
		}
		if out.OK || out.Kind != OutcomeNoHealthy {
			t.Fatalf("should be no_healthy, got %+v", out)
		}
		if out.Revision != 1 || out.Reason == "" {
			t.Fatalf("no_healthy must state revision and reason, got %+v", out)
		}
		if out.InstanceID != "" || out.Address != "" || out.Sequence != 0 {
			t.Fatalf("no_healthy must fabricate no target, got id=%q addr=%q seq=%d", out.InstanceID, out.Address, out.Sequence)
		}
	}
	keyX, keyY := "x", "y"
	assertNoHealthy(&keyX)
	assertNoHealthy(&keyY)
	assertNoHealthy(nil)

	// An instance no session is bound to recovers first. No keyed request is
	// made against it: it only establishes a healthy rotation candidate past
	// the cursor so a later fresh rotation would not coincide with x's binding.
	markHealth(t, r, "svc", "c", 1, 7, true, "")

	// x's own bound instance a recovers while b is still down. x reuses a
	// directly with its latest sequence: had the failure erased the binding,
	// this would be a first-time rotation from cursor b, which skips past b to
	// c — returning a therefore proves the preserved binding was reused and
	// the rotation never moved.
	markHealth(t, r, "svc", "a", 1, 8, true, "")
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 8)

	// y's bound instance b recovers; y likewise reuses b directly instead of
	// rotating to c, showing its binding survived too.
	markHealth(t, r, "svc", "b", 1, 9, true, "")
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 9)
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 8)

	// The last real rotation is still y's original success on b; all the
	// no_healthy failures and direct reuses left the cursor there. A plain
	// select therefore continues just after b to c, then wraps through a and b
	// using each instance's recovered sequence.
	assertPick(t, selected(t, r, "svc", 1), "c", "h3:3", 7)
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 8)
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 9)

	// The plain rotation rewrote no session binding: both sessions still
	// answer their own targets.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 8)
	assertPick(t, selectSession(t, r, "svc", 1, "y"), "b", "h2:2", 9)

	// Neither the failed selections nor the successful reuses changed a
	// revision or an accepted health record.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("failures changed the registration revision: %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthHealthy || inst.Sequence != 8 || inst.Reason != "" {
		t.Fatalf("a final record: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst.Health != HealthHealthy || inst.Sequence != 9 || inst.Reason != "" {
		t.Fatalf("b final record: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "c"); inst.Health != HealthHealthy || inst.Sequence != 7 || inst.Reason != "" {
		t.Fatalf("c final record: %+v", inst)
	}
}
