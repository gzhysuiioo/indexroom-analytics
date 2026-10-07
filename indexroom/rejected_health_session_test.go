package indexroom

import "testing"

// This file is the domain-level regression guard for a rejected health report
// aimed at an instance a session is already bound to. The existing suite covers
// session stickiness and rejected health reports separately, but it never
// combines them: these tests lock that a rejection is not a state change as far
// as session reuse and the shared rotation are concerned.
//
//   - a stale unhealthy observation and a same-sequence/different-content
//     conflict about the bound instance answer ok:false with the current
//     revision and accepted sequence, report Changed false and leave the
//     revision-mismatch flag clear (the content conflict must not gain an
//     expectedRevision/actualRevision pair);
//   - the bound session keeps reusing the instance's current address and
//     accepted sequence instead of falling back to the rotation and rebinding,
//     because the rejected unhealthy content never altered eligibility;
//   - an interleaved keyless selection continues just after the last actually
//     rotated id — it neither skips, restarts nor spends an extra slot — and
//     that plain selection rewrites the session binding no more than the
//     rejections did;
//   - the snapshot keeps the instance healthy at its accepted sequence with no
//     reason, leaves every other instance's record untouched and does not bump
//     the registration revision.

// TestRegistryRejectedHealthKeepsSessionBindingAndRotation drives the session
// scenario through the public registry API: three healthy instances a, b, c at
// sequence 10, session s bound to a, and one keyless selection parking the
// rotation on b; then a stale and a same-sequence conflicting unhealthy report
// about a are both rejected, and keyed and keyless selections prove neither
// rejection moved the binding, the accepted record or the rotation.
func TestRegistryRejectedHealthKeepsSessionBindingAndRotation(t *testing.T) {
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 10, true, "")
	markHealth(t, r, "svc", "b", 1, 10, true, "")
	markHealth(t, r, "svc", "c", 1, 10, true, "")

	// The key's first success rotates to the smallest id and binds s -> a; the
	// following keyless selection continues to b and parks the cursor there.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 10)
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 10)

	// Sequence 9 is older than the accepted 10: stale. The failure reports the
	// current revision and accepted sequence, marks no change, and is not a
	// registration-revision conflict.
	stale := reportHealth(t, r, "svc", "a", 1, 9, false, "心跳超时")
	if stale.OK || stale.Kind != OutcomeStale ||
		stale.Revision != 1 || stale.Sequence != 10 || stale.Changed ||
		stale.RevisionMismatch || stale.Reason == "" {
		t.Fatalf("stale unhealthy report: %+v", stale)
	}

	// Reusing the accepted sequence 10 with the opposite health content is the
	// other conflict: it still reports the current revision and accepted
	// sequence with no change, and the revision-mismatch flag stays clear so it
	// serializes without an expected/actual revision pair.
	conflict := reportHealth(t, r, "svc", "a", 1, 10, false, "心跳超时")
	if conflict.OK || conflict.Kind != OutcomeConflict ||
		conflict.Revision != 1 || conflict.Sequence != 10 || conflict.Changed ||
		conflict.RevisionMismatch || conflict.Reason == "" {
		t.Fatalf("same-sequence content conflict: %+v", conflict)
	}

	// a was never marked unhealthy: s reuses its binding — the existing address
	// and accepted sequence 10 — rather than rebinding through the rotation.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 10)

	// The keyless rotation is still parked on b by the earlier plain selection;
	// neither the rejections nor the session reuses moved it, so it continues to
	// c instead of skipping c, restarting at a or repeating b.
	assertPick(t, selected(t, r, "svc", 1), "c", "h3:3", 10)

	// The plain rotation rewrote no binding: s still answers a.
	assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 10)

	// The snapshot reflects only accepted observations: revision 1, all three
	// instances healthy at sequence 10 with no reason and their original
	// addresses; nothing from the rejected reports leaked in.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("rejected reports or selections changed the revision: %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Address != "h1:1" ||
		inst.Health != HealthHealthy || inst.Sequence != 10 || inst.Reason != "" {
		t.Fatalf("a must stay healthy at sequence 10 with no reason: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "b"); inst.Address != "h2:2" ||
		inst.Health != HealthHealthy || inst.Sequence != 10 || inst.Reason != "" {
		t.Fatalf("b must keep its accepted record: %+v", inst)
	}
	if inst := instanceHealth(t, r, "svc", "c"); inst.Address != "h3:3" ||
		inst.Health != HealthHealthy || inst.Sequence != 10 || inst.Reason != "" {
		t.Fatalf("c must keep its accepted record: %+v", inst)
	}
}
