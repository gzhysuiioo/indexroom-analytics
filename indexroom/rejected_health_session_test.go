package indexroom

import "testing"

// This file is the registry-level regression guard for session stickiness when
// an offline health observation against the bound instance is rejected rather
// than accepted. It complements the end-to-end batch test in cmd/indexroom by
// locking the state machine directly:
//
//   - a stale observation (sequence below the accepted one) is "stale" and a
//     same-sequence/different-content observation is a health-content
//     "conflict"; both report the current revision and the accepted sequence
//     with no expected/actual revision pair, are not OK and report no change;
//   - neither rejection mutates the bound instance's accepted healthy record,
//     so the session keeps reusing that instance instead of falling back to the
//     rotation and rebinding;
//   - the rotation continues just after the previously chosen plain id, and a
//     plain selection rewrites the session binding neither on the rejection nor
//     afterwards;
//   - the registration revision is never bumped by a rejected health item.
//
// The state is observed only through the public apply/select/snapshot results,
// like the rest of this package's suite.

// TestRegistryRejectedHealthKeepsSessionBindingAndRotation runs both rejection
// kinds from the identical starting state: a, b, c healthy, session s bound to
// a at the accepted sequence 10, and the plain rotation resting on b.
func TestRegistryRejectedHealthKeepsSessionBindingAndRotation(t *testing.T) {
	cases := []struct {
		name       string
		sequence   int64
		healthy    bool
		wantKind   OutcomeKind
		wantReason string
	}{
		{
			name:       "stale sequence",
			sequence:   9,
			healthy:    false,
			wantKind:   OutcomeStale,
			wantReason: "sequence 9 is older than the current sequence 10",
		},
		{
			name:       "same sequence different content",
			sequence:   10,
			healthy:    false,
			wantKind:   OutcomeConflict,
			wantReason: "sequence 10 already used with different health content",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := NewRegistry()
			registerService(t, r, "svc", 0, []Instance{
				{ID: "a", Address: "h1:1"},
				{ID: "b", Address: "h2:2"},
				{ID: "c", Address: "h3:3"},
			})
			// a's accepted observation is healthy at sequence 10; b and c are
			// healthy at sequence 1. The valid unhealthy reason makes the
			// rejected content well-formed, so the rejection is purely the
			// sequence rule rather than a validation error.
			markHealth(t, r, "svc", "a", 1, 10, true, "")
			markHealth(t, r, "svc", "b", 1, 1, true, "")
			markHealth(t, r, "svc", "c", 1, 1, true, "")

			// s first rotates to the smallest healthy id a and binds s -> a;
			// the rotation rests on a.
			assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 10)
			// The following plain select continues just after a to b, leaving
			// the rotation resting on b.
			assertPick(t, selected(t, r, "svc", 1), "b", "h2:2", 1)

			// Submit the rejected unhealthy observation against a at the
			// matching current revision.
			upd, err := r.ValidateHealth("svc", "a", 1, tc.sequence, tc.healthy, "心跳超时")
			if err != nil {
				t.Fatalf("rejected observation should be well-formed: %v", err)
			}
			out := r.ApplyHealth(upd)

			// The rejection reports the specific kind and reason, the current
			// registration revision and the accepted health sequence 10. It is
			// not OK, reports no change, and a health-content conflict carries
			// no expected/actual revision pair (it must not look like a
			// registration mismatch).
			if out.OK || out.Kind != tc.wantKind || out.Reason != tc.wantReason ||
				out.Revision != 1 || out.Sequence != 10 || out.Changed {
				t.Fatalf("rejection outcome: %+v want kind %q reason %q", out, tc.wantKind, tc.wantReason)
			}
			if out.RevisionMismatch || out.Expected != 0 || out.Actual != 0 {
				t.Fatalf("rejection must not report a revision pair: %+v", out)
			}

			// a's accepted record is untouched: still healthy at sequence 10
			// with no reason, and the registration revision is still 1.
			if inst := instanceHealth(t, r, "svc", "a"); inst.Health != HealthHealthy ||
				inst.Sequence != 10 || inst.Reason != "" {
				t.Fatalf("a accepted record must survive the rejection: %+v", inst)
			}
			if rev := r.RevisionOf("svc"); rev != 1 {
				t.Fatalf("rejection bumped the registration revision to %d", rev)
			}

			// Because the unhealthy content was not accepted, a stays healthy
			// and s keeps its existing binding: it returns a's existing address
			// and sequence 10 rather than falling back to the rotation (which
			// would have chosen c). The reuse moves no cursor.
			assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 10)

			// The rotation still rests on b, so the next plain select continues
			// just after b to c: the rejection and the session reuse neither
			// restarted at a, skipped c nor consumed a rotation slot.
			assertPick(t, selected(t, r, "svc", 1), "c", "h3:3", 1)

			// Reusing s still answers a: the plain selection through c rewrote
			// the session binding to nothing else.
			assertPick(t, selectSession(t, r, "svc", 1, "s"), "a", "h1:1", 10)

			// b and c kept their own addresses and accepted records.
			if inst := instanceHealth(t, r, "svc", "b"); inst.Address != "h2:2" ||
				inst.Health != HealthHealthy || inst.Sequence != 1 {
				t.Fatalf("b record must be untouched: %+v", inst)
			}
			if inst := instanceHealth(t, r, "svc", "c"); inst.Address != "h3:3" ||
				inst.Health != HealthHealthy || inst.Sequence != 1 {
				t.Fatalf("c record must be untouched: %+v", inst)
			}
		})
	}
}
