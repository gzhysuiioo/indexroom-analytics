package main

// This file is the end-to-end regression guard for large health sequences at
// the register command's JSON boundary. A sequence is a positive signed
// 64-bit integer per instance, and the existing small-sequence cases never
// exercise the magnitudes where integer precision is actually at risk:
//
//   - 9007199254740992 (2^53) and 9007199254740993 (2^53+1) are adjacent
//     integers, but 2^53+1 is the first positive integer a float64 cannot
//     represent — it rounds back down to 2^53. Any decode or encode path that
//     passes through a float collapses the two into one value, turning the
//     newer observation into a same-sequence conflict. Both observations must
//     be accepted in order, and every emitted sequence (health results, stale
//     and conflict reasons, the select result and the final instance record)
//     must carry the submitted digits exactly;
//   - 9223372036854775807 (math.MaxInt64) is the legal upper bound and must
//     update and select normally with the full integer preserved;
//   - 9223372036854775808 overflows int64 and must be that one item's invalid
//     rejection — the reason naming the sequence's numeric problem — never a
//     legal sequence, and never a top-level failure that strips the batch of
//     its per-item results. Later requests still run against the committed
//     state.
//
// The batches also lock the shared invariants at these magnitudes: health
// reports never bump the registration revision, a stale report and a
// same-sequence conflict are distinguished by their reasons and overwrite
// nothing, a batch containing failures exits 1 with every result slot kept in
// input order, and the all-success counterpart exits 0.

import (
	"strconv"
	"strings"
	"testing"
)

// Large sequence values as decimal text, so the submitted JSON carries the
// exact digits and the assertions compare against the same source.
const (
	seq2to53JSON      = "9007199254740992" // 2^53, largest exactly-representable float64 integer boundary
	seq2to53Plus1JSON = "9007199254740993" // 2^53+1, rounds to 2^53 in float64
	seqMaxInt64JSON   = "9223372036854775807"
	seqOverMaxJSON    = "9223372036854775808"
)

// seq2to53 is 2^53 as an int64 for comparisons against decoded output.
const seq2to53 = int64(9007199254740992)

// seq2to53Plus1 is 2^53+1 as an int64 for comparisons against decoded output.
const seq2to53Plus1 = int64(9007199254740993)

// TestHealthAdjacentLargeSequencesPrecision drives one registered instance
// through an unhealthy observation at 2^53 and a recovery at 2^53+1, then the
// stale/duplicate/conflict replays at those same magnitudes. If the command
// decoded or emitted sequences through a float64, the two adjacent values
// would collapse into one and the recovery would surface as a conflict
// instead of a change.
func TestHealthAdjacentLargeSequencesPrecision(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + seq2to53JSON + `,"healthy":false,"reason":"心跳超时"},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + seq2to53Plus1JSON + `,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + seq2to53JSON + `,"healthy":false,"reason":"误报重发"},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + seq2to53Plus1JSON + `,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + seq2to53Plus1JSON + `,"healthy":false,"reason":"重复序号改报"},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains failures, exit code: %d, output: %s", code, out)
	}
	// The emitted document must carry the exact digits of 2^53+1; a float64
	// round trip would print 9007199254740992 instead.
	if !strings.Contains(out, seq2to53Plus1JSON) {
		t.Fatalf("output lost the exact sequence %s:\n%s", seq2to53Plus1JSON, out)
	}
	got := decodeRegisterOutput(t, out)
	if len(got.Results) != 7 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0: registration accepted at revision 1.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 create: %+v", r)
	}
	// 1: the unhealthy observation at 2^53 is accepted; the reported sequence
	// echoes the submitted value exactly and the revision stays 1.
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != seq2to53 {
		t.Fatalf("result 1 unhealthy at 2^53: %+v", r)
	}
	// 2: 2^53+1 is NEWER than 2^53 — adjacent large integers must not collapse
	// into one. The recovery is a change, clears the reason, and reports the
	// exact submitted sequence.
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != seq2to53Plus1 {
		t.Fatalf("result 2 recovery at 2^53+1: %+v", r)
	}
	// 3: resubmitting 2^53 is stale against the accepted 2^53+1; the failure
	// reports the current sequence exactly and overwrites nothing.
	if r := got.Results[3]; r.OK || r.Error != "stale" ||
		r.Reason != "sequence "+seq2to53JSON+" is older than the current sequence "+seq2to53Plus1JSON ||
		r.Sequence != seq2to53Plus1 || r.Revision != 1 {
		t.Fatalf("result 3 stale at 2^53: %+v", r)
	}
	// 4: the same sequence 2^53+1 with identical content succeeds WITHOUT
	// changed — a duplicate, not an update.
	if r := got.Results[4]; !r.OK || r.Changed || r.Revision != 1 || r.Sequence != seq2to53Plus1 {
		t.Fatalf("result 4 duplicate at 2^53+1: %+v", r)
	}
	// 5: the same sequence 2^53+1 flipping to unhealthy conflicts; the reason
	// is distinct from the stale one and the accepted record is not overwritten.
	if r := got.Results[5]; r.OK || r.Error != "conflict" ||
		r.Reason != "sequence "+seq2to53Plus1JSON+" already used with different health content" ||
		r.Sequence != seq2to53Plus1 || r.Revision != 1 {
		t.Fatalf("result 5 conflict at 2^53+1: %+v", r)
	}
	// 6: the instance is still healthy after the rejections and is selected
	// with its original address and the exact accepted sequence 2^53+1.
	if r := got.Results[6]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" ||
		r.Sequence != seq2to53Plus1 || r.Revision != 1 {
		t.Fatalf("result 6 select after rejections: %+v", r)
	}

	// The final record keeps the recovery: healthy at exactly 2^53+1 with the
	// unhealthy reason cleared, and the health reports never bumped the
	// registration revision.
	if len(got.Services) != 1 || got.Services[0].Service != "svc" || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 1 || insts[0].ID != "i1" || insts[0].Address != "h1:8080" ||
		insts[0].Health != "healthy" || insts[0].Sequence != seq2to53Plus1 || insts[0].Reason != "" {
		t.Fatalf("final instance record: %+v", insts)
	}
}

// TestHealthMaxInt64SequenceAcceptedAndOverflowRejected locks the legal upper
// bound and the first illegal value: 9223372036854775807 updates, selects and
// persists with the full integer intact, while 9223372036854775808 is that
// one item's invalid rejection — the batch keeps its per-item results and the
// following select still returns the previously accepted record.
func TestHealthMaxInt64SequenceAcceptedAndOverflowRejected(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + seqMaxInt64JSON + `,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + seqOverMaxJSON + `,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains a failure, exit code: %d, output: %s", code, out)
	}
	// The legal upper bound must survive the round trip untruncated.
	if !strings.Contains(out, seqMaxInt64JSON) {
		t.Fatalf("output lost the exact sequence %s:\n%s", seqMaxInt64JSON, out)
	}
	got := decodeRegisterOutput(t, out)
	if len(got.Results) != 5 {
		t.Fatalf("results: %+v", got.Results)
	}

	maxSeq, err := strconv.ParseInt(seqMaxInt64JSON, 10, 64)
	if err != nil {
		t.Fatalf("parse max sequence: %v", err)
	}

	// 0: registration accepted at revision 1.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 create: %+v", r)
	}
	// 1: the upper bound itself is a legal sequence: accepted, changed, echoed
	// exactly, revision unchanged.
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != maxSeq {
		t.Fatalf("result 1 healthy at max int64: %+v", r)
	}
	// 2: the selection returns the instance with its address and the full
	// max-int64 sequence — no rounding or truncation.
	if r := got.Results[2]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" ||
		r.Sequence != maxSeq || r.Revision != 1 {
		t.Fatalf("result 2 select at max int64: %+v", r)
	}
	// 3: one past the bound overflows int64 and is this item's invalid
	// rejection — never a legal sequence. The reason names the sequence's
	// numeric problem and the submitted value, the service's current revision
	// is reported, and no target or comparison fields appear.
	if r := got.Results[3]; r.OK || r.Error != "invalid" || r.Revision != 1 ||
		r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
		t.Fatalf("result 3 overflow rejection: %+v", r)
	}
	if reason := got.Results[3].Reason; !strings.Contains(reason, "sequence") ||
		!strings.Contains(reason, seqOverMaxJSON) {
		t.Fatalf("result 3 reason should name the sequence's numeric problem and the submitted value, got %q", reason)
	}
	// 4: the rejected overflow wrote nothing — the following select still
	// returns the previously accepted healthy instance, its original address
	// and the legal upper-bound sequence.
	if r := got.Results[4]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" ||
		r.Sequence != maxSeq || r.Revision != 1 {
		t.Fatalf("result 4 select after overflow rejection: %+v", r)
	}

	// The batch kept its per-item results in input order and the final record
	// holds the full legal-bound integer at the unchanged revision 1.
	if len(got.Services) != 1 || got.Services[0].Service != "svc" || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 1 || insts[0].ID != "i1" || insts[0].Address != "h1:8080" ||
		insts[0].Health != "healthy" || insts[0].Sequence != maxSeq || insts[0].Reason != "" {
		t.Fatalf("final instance record: %+v", insts)
	}
}

// TestHealthLargeSequenceAllSuccessBatch is the all-success counterpart: the
// same adjacent large sequences with no rejected item, so the exit status is
// 0 and the duplicate report still omits changed.
func TestHealthLargeSequenceAllSuccessBatch(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + seq2to53JSON + `,"healthy":false,"reason":"心跳超时"},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + seq2to53Plus1JSON + `,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + seq2to53Plus1JSON + `,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("all-success batch, exit code: %d, output: %s", code, out)
	}
	if !strings.Contains(out, seq2to53Plus1JSON) {
		t.Fatalf("output lost the exact sequence %s:\n%s", seq2to53Plus1JSON, out)
	}
	got := decodeRegisterOutput(t, out)
	if len(got.Results) != 5 {
		t.Fatalf("results: %+v", got.Results)
	}
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != seq2to53 {
		t.Fatalf("result 1 unhealthy at 2^53: %+v", r)
	}
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != seq2to53Plus1 {
		t.Fatalf("result 2 recovery at 2^53+1: %+v", r)
	}
	// The duplicate at 2^53+1 succeeds without changed.
	if r := got.Results[3]; !r.OK || r.Changed || r.Revision != 1 || r.Sequence != seq2to53Plus1 {
		t.Fatalf("result 3 duplicate at 2^53+1: %+v", r)
	}
	if r := got.Results[4]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" ||
		r.Sequence != seq2to53Plus1 || r.Revision != 1 {
		t.Fatalf("result 4 select: %+v", r)
	}
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 1 || insts[0].Health != "healthy" || insts[0].Sequence != seq2to53Plus1 || insts[0].Reason != "" {
		t.Fatalf("final instance record: %+v", insts)
	}
}
