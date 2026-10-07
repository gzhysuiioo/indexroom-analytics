package indexroom

import (
	"strings"
	"testing"
)

// This file is the regression guard for session stickiness across a very
// specific failure shape: the session's bound instance is currently unhealthy
// AND this one request's excludeInstanceIds list removes every remaining
// healthy instance. The filtered no_healthy failure must preserve both the
// session binding and the rotation position; whether a later request reuses
// the recovered binding or re-rotates depends only on the health records and
// the exclusion scope of THAT request, never on the earlier failure.

// assertExcludedNoHealthy locks the failure shape of a select whose
// excludeInstanceIds list removed every healthy instance: no_healthy at the
// current revision, a reason saying the exclusion did it, and no fabricated
// target (no instance id, address or health sequence).
func assertExcludedNoHealthy(t *testing.T, out SelectOutcome, revision int) {
	t.Helper()
	if out.OK || out.Kind != OutcomeNoHealthy {
		t.Fatalf("should be no_healthy, got %+v", out)
	}
	if out.Revision != revision {
		t.Fatalf("no_healthy must report the current revision %d, got %+v", revision, out)
	}
	if !strings.Contains(out.Reason, "excludeInstanceIds") {
		t.Fatalf("reason must say the exclusion removed the healthy instances, got %q", out.Reason)
	}
	if out.InstanceID != "" || out.Address != "" || out.Sequence != 0 {
		t.Fatalf("no_healthy must fabricate no target, got id=%q addr=%q seq=%d", out.InstanceID, out.Address, out.Sequence)
	}
}

// setupSessionRecoveryScenario builds the shared prelude: service svc with
// healthy instances a, b, c (sequence 1 each), session x bound to a through
// its first selection, the plain rotation advanced to b, and a then knocked
// unhealthy by a larger-sequence observation carrying a non-empty reason.
// The cursor rests on b and x's binding still points at the now-unhealthy a.
func setupSessionRecoveryScenario(t *testing.T, r *Registry) {
	t.Helper()
	registerService(t, r, "svc", 0, []Instance{
		{ID: "a", Address: "h1:1"}, {ID: "b", Address: "h2:2"}, {ID: "c", Address: "h3:3"},
	})
	markHealth(t, r, "svc", "a", 1, 1, true, "")
	markHealth(t, r, "svc", "b", 1, 1, true, "")
	markHealth(t, r, "svc", "c", 1, 1, true, "")

	// x's first selection rotates to the smallest id and binds x -> a; the
	// following plain selection advances the shared cursor to b.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "a", "h1:1", 1)
	assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 1)

	// A larger-sequence unhealthy observation with a non-empty reason knocks
	// the bound instance out of the eligible set; b and c stay healthy.
	markHealth(t, r, "svc", "a", 1, 2, false, "心跳超时")
}

// TestRegistrySessionExcludedFailureKeepsBindingForRecoveredReuse locks the
// recovery path: the filtered failure clears neither the binding nor the
// cursor, so once a accepts a larger-sequence healthy observation, a request
// that excludes only b reuses the recovered binding a — even though c is also
// selectable and a fresh rotation would have landed there. The plain rotation
// then continues from b, proving neither the failure nor the reuse moved it.
func TestRegistrySessionExcludedFailureKeepsBindingForRecoveredReuse(t *testing.T) {
	r := NewRegistry()
	setupSessionRecoveryScenario(t, r)
	keyX := "x"

	// x selects while its bound instance a is unhealthy and this request
	// excludes the only remaining healthy instances b and c: no_healthy, and
	// the failure must not clear the x -> a binding or move the cursor off b.
	assertExcludedNoHealthy(t, selectExcluding(t, r, "svc", 1, &keyX, "b", "c"), 1)

	// a accepts a larger-sequence healthy observation and recovers.
	markHealth(t, r, "svc", "a", 1, 3, true, "")

	// Excluding only b this time: the preserved binding is reusable again, so
	// x gets a back with its current address and latest health sequence 3 —
	// the request does NOT fall back to the rotation, which from the cursor on
	// b would have chosen c.
	assertPick(t, selectExcluding(t, r, "svc", 1, &keyX, "b"), "a", "h1:1", 3)

	// The plain rotation still rests on b: the next ordinary selection
	// continues just after b to c, proving the failed request and the session
	// reuse above both left the position untouched.
	assertPick(t, selected(t, r, "svc", 1), "c", "h3:3", 1)

	// Nothing durable moved: the revision never bumped and every instance
	// keeps exactly its accepted observations.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("selection flow changed the registration revision: %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthHealthy || inst.Sequence != 3 {
		t.Fatalf("a should be healthy at sequence 3: %+v", inst)
	}
	for _, id := range []string{"b", "c"} {
		if inst := instanceHealth(t, r, "svc", id); inst.Health != HealthHealthy || inst.Sequence != 1 {
			t.Fatalf("%s should stay healthy at sequence 1: %+v", id, inst)
		}
	}
}

// TestRegistrySessionRecoveryRespectsCurrentExclusion locks the other half of
// the contract: recovery does not make the binding immune to THIS request's
// exclusion list. After the same filtered failure, a recovers, but x's next
// request excludes a and b, so the recovered binding cannot be reused — the
// request falls back to the filtered rotation, chooses c and rebinds x -> c.
// The earlier exclusion stays scoped to its own request: it removed no
// instance and rewrote no health record.
func TestRegistrySessionRecoveryRespectsCurrentExclusion(t *testing.T) {
	r := NewRegistry()
	setupSessionRecoveryScenario(t, r)
	keyX := "x"

	// The same filtered failure as above: bound a unhealthy, b and c excluded
	// for this one request; binding and cursor survive.
	assertExcludedNoHealthy(t, selectExcluding(t, r, "svc", 1, &keyX, "b", "c"), 1)

	// a recovers with a larger-sequence healthy observation.
	markHealth(t, r, "svc", "a", 1, 3, true, "")

	// This request excludes a and b: the recovered binding a is excluded, so
	// the request falls back to the filtered rotation. From the cursor on b
	// the only candidate is c; success rebinds x -> c and advances the cursor.
	assertPick(t, selectExcluding(t, r, "svc", 1, &keyX, "a", "b"), "c", "h3:3", 1)

	// x without a list now reuses the new binding c — the earlier exclusion of
	// c was scoped to its own request and did not bar c from later bindings.
	assertPick(t, selectSession(t, r, "svc", 1, "x"), "c", "h3:3", 1)

	// The plain rotation continues just after the last real rotation (c) and
	// wraps to a: the exclusions removed nothing from the registry, and the
	// recovered a is fully eligible again with its latest sequence.
	assertPick(t, selected(t, r, "svc", 1), "a", "h1:1", 3)

	// The exclusions were per-request filters only: revision unchanged, all
	// three instances still registered at their original addresses, and the
	// health records reflect exactly the accepted observations.
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("exclusions changed the registration revision: %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthHealthy || inst.Sequence != 3 {
		t.Fatalf("a should be healthy at sequence 3: %+v", inst)
	}
	for _, id := range []string{"b", "c"} {
		if inst := instanceHealth(t, r, "svc", id); inst.Health != HealthHealthy || inst.Sequence != 1 {
			t.Fatalf("%s should stay healthy at sequence 1: %+v", id, inst)
		}
	}
}
