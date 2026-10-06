package indexroom

// This file is the domain-level regression guard for large health sequences.
// A sequence is a positive signed 64-bit integer per instance, and the
// existing small-sequence cases never reach the magnitudes where the ordering
// rules are actually at risk: 9007199254740992 (2^53) and 9007199254740993
// (2^53+1) are adjacent integers, but 2^53+1 is the first positive integer a
// float64 cannot represent, so any comparison or storage path that narrows
// through a float collapses the newer observation into the older one. These
// tests drive the public Validate/Apply/Select and Snapshot APIs at those
// magnitudes and at the legal upper bound math.MaxInt64 to lock:
//
//   - adjacent large sequences stay ordered: 2^53+1 is newer than 2^53, so
//     the recovery is a change that clears the reason, and resubmitting 2^53
//     is stale against it;
//   - the same large sequence distinguishes an identical duplicate (success
//     without changed) from a content conflict, and neither rejection
//     overwrites the accepted record;
//   - math.MaxInt64 is accepted, selected and snapshotted with the full
//     integer, and health reports never bump the registration revision.

import (
	"math"
	"testing"
)

// TestRegistryHealthAdjacentLargeSequences walks one instance through an
// unhealthy observation at 2^53 and a recovery at 2^53+1, then the stale,
// duplicate and conflict replays at those magnitudes.
func TestRegistryHealthAdjacentLargeSequences(t *testing.T) {
	const (
		seq2to53      = int64(9007199254740992)
		seq2to53Plus1 = int64(9007199254740993)
	)
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "i1", Address: "h1:8080"}})

	// The unhealthy observation at 2^53 is accepted with its trimmed reason.
	assertHealth(t, reportHealth(t, r, "svc", "i1", 1, seq2to53, false, " 心跳超时 "), 1, seq2to53)

	// 2^53+1 is newer than 2^53: the recovery is a change and clears the
	// reason. A float64-narrowed comparison would see the two as equal and
	// report a conflict here instead.
	assertHealth(t, reportHealth(t, r, "svc", "i1", 1, seq2to53Plus1, true, ""), 1, seq2to53Plus1)
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Health != HealthHealthy || inst.Sequence != seq2to53Plus1 || inst.Reason != "" {
		t.Fatalf("i1 should be healthy at 2^53+1 with the reason cleared, got %+v", inst)
	}

	// Resubmitting 2^53 is stale against the accepted 2^53+1 and reports the
	// current sequence exactly.
	out := reportHealth(t, r, "svc", "i1", 1, seq2to53, false, "误报重发")
	if out.OK || out.Kind != OutcomeStale || out.Sequence != seq2to53Plus1 || out.Revision != 1 {
		t.Fatalf("stale at 2^53: %+v", out)
	}

	// The same 2^53+1 with identical content succeeds without changed.
	out = reportHealth(t, r, "svc", "i1", 1, seq2to53Plus1, true, "")
	if !out.OK || out.Changed || out.Sequence != seq2to53Plus1 || out.Revision != 1 {
		t.Fatalf("duplicate at 2^53+1: %+v", out)
	}

	// The same 2^53+1 flipping to unhealthy conflicts and overwrites nothing.
	out = reportHealth(t, r, "svc", "i1", 1, seq2to53Plus1, false, "重复序号改报")
	if out.OK || out.Kind != OutcomeConflict || out.Sequence != seq2to53Plus1 || out.Revision != 1 {
		t.Fatalf("conflict at 2^53+1: %+v", out)
	}

	// The rejections changed nothing: the instance is still healthy at
	// 2^53+1, selection returns it with its address and exact sequence, and
	// the registration revision never moved.
	sel := trySelect(t, r, "svc", 1)
	if !sel.OK || sel.InstanceID != "i1" || sel.Address != "h1:8080" || sel.Sequence != seq2to53Plus1 || sel.Revision != 1 {
		t.Fatalf("select after rejections: %+v", sel)
	}
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("health reports must not bump the registration revision, got %d", rev)
	}
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Health != HealthHealthy || inst.Sequence != seq2to53Plus1 || inst.Reason != "" {
		t.Fatalf("final record: %+v", inst)
	}
}

// TestRegistryHealthMaxInt64Sequence locks the legal upper bound: a sequence
// of math.MaxInt64 validates, updates, selects and snapshots with the full
// integer preserved.
func TestRegistryHealthMaxInt64Sequence(t *testing.T) {
	const maxSeq = int64(math.MaxInt64)
	r := NewRegistry()
	registerService(t, r, "svc", 0, []Instance{{ID: "i1", Address: "h1:8080"}})

	assertHealth(t, reportHealth(t, r, "svc", "i1", 1, maxSeq, true, ""), 1, maxSeq)

	sel := trySelect(t, r, "svc", 1)
	if !sel.OK || sel.InstanceID != "i1" || sel.Address != "h1:8080" || sel.Sequence != maxSeq || sel.Revision != 1 {
		t.Fatalf("select at max int64: %+v", sel)
	}
	if inst := instanceHealth(t, r, "svc", "i1"); inst.Health != HealthHealthy || inst.Sequence != maxSeq || inst.Reason != "" {
		t.Fatalf("snapshot at max int64: %+v", inst)
	}
	if rev := r.RevisionOf("svc"); rev != 1 {
		t.Fatalf("health reports must not bump the registration revision, got %d", rev)
	}
}
