package indexroom

import (
	"math"
	"strings"
	"testing"
)

// This file is the domain-level regression guard for health observations that
// carry large sequence numbers. Sequences are positive signed 64-bit integers
// and the update rule (newer updates, older is stale, equal content is a
// no-op, equal sequence with different content conflicts) must stay exact all
// the way to the int64 boundary — in particular for the two adjacent integers
// 2^53 and 2^53+1, which IEEE-754 double precision cannot distinguish. JSON
// numbers decode that way in many runtimes, so these tests lock both the
// comparison semantics and the exact accepted value via the public
// Validate/Apply/Select and Snapshot APIs.

// TestParseSequence locks the raw-token parser used for a health
// observation's sequence. It establishes integer type and int64
// representability only; the positive rule belongs to ValidateHealth, so zero
// and negatives still parse here and are rejected at validation.
func TestParseSequence(t *testing.T) {
	cases := []struct {
		text string
		want int64
		err  string // "" means no error expected
	}{
		{"1", 1, ""},
		{"42", 42, ""},
		{"9007199254740992", 9007199254740992, ""},
		{"9007199254740993", 9007199254740993, ""},
		{"9223372036854775807", math.MaxInt64, ""},
		// Zero and negatives parse as integers; the positive rule belongs to
		// ValidateHealth so its reason stays "sequence must be a positive
		// integer" rather than a parse-time range message.
		{"0", 0, ""},
		{"-1", -1, ""},
		// Non-integer tokens get the integer-type error, quoting as submitted.
		{"1.5", 0, "sequence must be an integer, got 1.5"},
		{"1e3", 0, "sequence must be an integer, got 1e3"},
		{`"1"`, 0, "sequence must be an integer"},
		{"true", 0, "sequence must be an integer"},
		// 9223372036854775808 is one past the signed 64-bit maximum and cannot
		// be carried numerically; the raw text is reported as an
		// out-of-range sequence rather than an overflowed value that could
		// narrow to a legal sequence.
		{"9223372036854775808", 0, "sequence must be an integer between 1 and"},
		{"99999999999999999999999999", 0, "sequence must be an integer between 1 and"},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			got, err := ParseSequence(tc.text)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.want {
					t.Fatalf("got %d want %d", got, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("got error %v, want substring %q", err, tc.err)
			}
		})
	}
}

// TestRegistryHealthAdjacentLargeSequencesDistinguished is the central
// precision guard: the adjacent sequences 2^53 and 2^53+1 must be treated as
// two different observations. The first marks the instance unhealthy with a
// reason, the second (one greater) must still count as newer, change the
// record and clear the reason. Replaying the older one is stale and reports
// the currently accepted sequence; replaying the current one with identical
// content succeeds without a change; reusing the current sequence to flip the
// health conflicts and must not overwrite the record. Selection and the
// snapshot observe the accepted record, and no health observation bumps the
// registration revision.
func TestRegistryHealthAdjacentLargeSequencesDistinguished(t *testing.T) {
	const (
		older int64 = 9007199254740992 // 2^53
		newer int64 = 9007199254740993 // 2^53 + 1, indistinguishable above in float64
	)

	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "i1", Address: "h1:8080"}})

	// The 2^53 observation is accepted unhealthy with its trimmed reason; the
	// output sequence must equal the submitted value exactly.
	assertHealth(t, reportHealth(t, r, "svc", "i1", 1, older, false, " 心跳超时 "), 1, older)
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Health != HealthUnhealthy ||
		inst.Sequence != older || inst.Reason != "心跳超时" {
		t.Fatalf("after %d unhealthy: %+v", older, inst)
	}

	// 2^53+1 is only one greater but must count as newer: changed, healthy,
	// and the unhealthy reason is cleared.
	assertHealth(t, reportHealth(t, r, "svc", "i1", 1, newer, true, ""), 1, newer)
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Health != HealthHealthy ||
		inst.Sequence != newer || inst.Reason != "" {
		t.Fatalf("after %d healthy: %+v", newer, inst)
	}

	// Replaying the older sequence is stale and reports the currently
	// accepted sequence (exactly 2^53+1); the instance stays healthy.
	if out := reportHealth(t, r, "svc", "i1", 1, older, true, ""); out.OK ||
		out.Kind != OutcomeStale || out.Sequence != newer || out.Revision != 1 {
		t.Fatalf("replayed %d should be stale against %d: %+v", older, newer, out)
	}
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Health != HealthHealthy ||
		inst.Sequence != newer || inst.Reason != "" {
		t.Fatalf("stale replay must not overwrite the record: %+v", inst)
	}

	// Replaying the current sequence with identical healthy content succeeds
	// but reports no change.
	if out := reportHealth(t, r, "svc", "i1", 1, newer, true, ""); !out.OK ||
		out.Changed || out.Sequence != newer || out.Revision != 1 {
		t.Fatalf("identical replay of %d should succeed without change: %+v", newer, out)
	}

	// Reusing the current sequence with different content (unhealthy) is a
	// conflict and must leave the healthy record untouched.
	if out := reportHealth(t, r, "svc", "i1", 1, newer, false, "再次故障"); out.OK ||
		out.Kind != OutcomeConflict || out.Sequence != newer || out.Revision != 1 {
		t.Fatalf("same-sequence conflict at %d: %+v", newer, out)
	}
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Health != HealthHealthy ||
		inst.Sequence != newer || inst.Reason != "" {
		t.Fatalf("conflicting replay must not overwrite the record: %+v", inst)
	}

	// Selection is the external observation that the record really took
	// effect: the healthy instance is chosen with its address and the exact
	// accepted sequence 2^53+1.
	assertPick(t, trySelect(t, r, "svc", 1), "i1", "h1:8080", newer)

	// Health observations never bump the registration revision, and the
	// snapshot keeps the full integer.
	views := r.Snapshot()
	if len(views) != 1 || views[0].Revision != 1 {
		t.Fatalf("health reports must not bump the revision: %+v", views)
	}
	want := InstanceView{ID: "i1", Address: "h1:8080", Health: HealthHealthy, Sequence: newer}
	if len(views[0].Instances) != 1 || views[0].Instances[0] != want {
		t.Fatalf("snapshot: got %+v want %+v", views[0].Instances, want)
	}
}

// TestRegistryHealthSequenceUpperBound locks the legal upper boundary: a
// healthy observation carrying math.MaxInt64 updates normally, and the exact
// value survives selection and the snapshot with no rounding or truncation.
// From that boundary a smaller sequence is stale (reporting the full max),
// while reusing the max sequence with conflicting content is rejected without
// touching the record.
func TestRegistryHealthSequenceUpperBound(t *testing.T) {
	const maxSeq int64 = math.MaxInt64

	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "i1", Address: "h1:8080"}})

	assertHealth(t, reportHealth(t, r, "svc", "i1", 1, maxSeq, true, ""), 1, maxSeq)

	// The full 19-digit integer is the current sequence everywhere it is
	// observable: selection result and the final snapshot.
	assertPick(t, trySelect(t, r, "svc", 1), "i1", "h1:8080", maxSeq)
	want := InstanceView{ID: "i1", Address: "h1:8080", Health: HealthHealthy, Sequence: maxSeq}
	if inst := instanceHealth(t, r, "svc", "i1"); inst != want {
		t.Fatalf("boundary record: got %+v want %+v", inst, want)
	}

	// A smaller sequence is stale against the max and reports it in full.
	if out := reportHealth(t, r, "svc", "i1", 1, maxSeq-1, false, "迟到的观察"); out.OK ||
		out.Kind != OutcomeStale || out.Sequence != maxSeq {
		t.Fatalf("sequence %d should be stale against %d: %+v", maxSeq-1, maxSeq, out)
	}
	// Reusing the max sequence to report different content conflicts without
	// overwriting the healthy record.
	if out := reportHealth(t, r, "svc", "i1", 1, maxSeq, false, "边界故障"); out.OK ||
		out.Kind != OutcomeConflict || out.Sequence != maxSeq {
		t.Fatalf("same-sequence conflict at the boundary: %+v", out)
	}
	if inst := instanceHealth(t, r, "svc", "i1"); inst != want {
		t.Fatalf("rejections must not overwrite the boundary record: got %+v want %+v", inst, want)
	}
	if view := r.Snapshot()[0]; view.Revision != 1 {
		t.Fatalf("health reports must not bump the revision: %+v", view)
	}
}
