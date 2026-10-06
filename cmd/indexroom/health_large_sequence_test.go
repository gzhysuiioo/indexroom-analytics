package main

// This file is the end-to-end regression guard for integer precision in the
// register command's health JSON. Sequences are positive signed 64-bit
// integers; JSON numbers are floating point in many toolchains and lose the
// distinction between 2^53 and 2^53+1, so the batches below assert the exact
// submitted digits both numerically (decoded into int64) and as raw output
// text. They also lock the update rule the sequence drives and the batch
// semantics:
//
//   - the adjacent sequences 9007199254740992 and 9007199254740993 are two
//     distinct observations: the second changes the record and clears the
//     unhealthy reason; replaying the first is stale and reports the current
//     sequence; replaying the current content succeeds without a change;
//     reusing the current sequence with different health content conflicts
//     and does not overwrite the record — stale and same-sequence conflict
//     carry distinguishable reasons;
//   - the legal upper bound 9223372036854775807 is accepted healthy and
//     survives unchanged in the health result, the selection result and the
//     final instance record;
//   - 9223372036854775808 (one past the bound) is that one item's invalid
//     result with a reason naming the sequence value — not a legal sequence,
//     not a batch-wide decode failure — and a later selection still runs and
//     returns the previously accepted healthy instance with its address and
//     the bound sequence;
//   - health operations never bump the registration revision; a batch with
//     rejected items keeps every per-item result in input order and exits 1,
//     while the all-success counterpart exits 0.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRegisterHealthAdjacentLargeSequences drives one registered instance
// through the adjacent 2^53 / 2^53+1 observations. The batch contains the
// stale and conflict rejections, so its exit status is 1 while every later
// item and the final service list are still emitted.
func TestRegisterHealthAdjacentLargeSequences(t *testing.T) {
	const (
		older = "9007199254740992" // 2^53
		newer = "9007199254740993" // 2^53 + 1, indistinguishable from 2^53 in float64
	)
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + older + `,"healthy":false,"reason":" 心跳超时 "},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + newer + `,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + older + `,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + newer + `,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + newer + `,"healthy":false,"reason":"再次故障"},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains stale/conflict rejections, exit code: %d, output: %s", code, out)
	}

	// The raw document must carry both full integers exactly — no scientific
	// notation, decimal point or rounding, both values present as distinct
	// digit runs.
	if !strings.Contains(out, older) || !strings.Contains(out, newer) {
		t.Fatalf("output must contain both exact sequences %s and %s:\n%s", older, newer, out)
	}
	if strings.Contains(out, "9.007") || strings.Contains(out, "e+") || strings.Contains(out, newer+".0") {
		t.Fatalf("output must not round the integers:\n%s", out)
	}

	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 7 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0: registration creates the service at revision 1.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 register: %+v", r)
	}
	// 1: the 2^53 unhealthy observation is accepted with its trimmed reason
	// recorded (the reason is only observable in the final record); the
	// returned sequence equals the submitted value exactly.
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 ||
		r.Sequence != 9007199254740992 {
		t.Fatalf("result 1 unhealthy at 2^53: %+v", r)
	}
	// 2: 2^53+1 is only one greater but must count as newer: it changes the
	// record to healthy and clears the reason. The exact +1 value comes back.
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 1 ||
		r.Sequence != 9007199254740993 {
		t.Fatalf("result 2 healthy at 2^53+1: %+v", r)
	}
	// 3: replaying the older observation is stale; the failure reports the
	// currently accepted sequence (exactly 2^53+1) and a stale-specific
	// reason, and the instance is not knocked unhealthy.
	if r := got.Results[3]; r.OK || r.Error != "stale" ||
		r.Reason != "sequence "+older+" is older than the current sequence "+newer ||
		r.Sequence != 9007199254740993 || r.Revision != 1 {
		t.Fatalf("result 3 stale replay: %+v", r)
	}
	// 4: replaying the current sequence with identical healthy content
	// succeeds but produces no change.
	if r := got.Results[4]; !r.OK || r.Changed || r.Revision != 1 ||
		r.Sequence != 9007199254740993 {
		t.Fatalf("result 4 identical replay without change: %+v", r)
	}
	// 5: reusing the same sequence with different content (unhealthy) is a
	// conflict — distinguishable from the stale failure by both error and
	// reason — and must not overwrite the healthy record.
	if r := got.Results[5]; r.OK || r.Error != "conflict" ||
		r.Reason != "sequence "+newer+" already used with different health content" ||
		r.Sequence != 9007199254740993 || r.Revision != 1 {
		t.Fatalf("result 5 same-sequence conflict: %+v", r)
	}
	// 6: selection observes the record that actually took effect: i1 is still
	// healthy at its address with the exact accepted sequence 2^53+1 — the
	// rejected unhealthy replay never landed.
	if r := got.Results[6]; !r.OK || r.Revision != 1 ||
		r.InstanceID != "i1" || r.Address != "h1:8080" ||
		r.Sequence != 9007199254740993 {
		t.Fatalf("result 6 select after rejections: %+v", r)
	}

	// The final record: healthy, exact sequence, reason cleared, revision
	// untouched by any health operation.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 || len(svc.Instances) != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	wantInst := registerInstance{ID: "i1", Address: "h1:8080", Health: "healthy", Sequence: 9007199254740993}
	if svc.Instances[0] != wantInst {
		t.Fatalf("final instance: got %+v want %+v", svc.Instances[0], wantInst)
	}
}

// TestRegisterHealthSequenceUpperBoundAndOverflow locks the legal int64
// boundary and the first value past it. The maximum sequence is accepted
// healthy and the full integer survives the health result, both selection
// results and the final record. The overflow value is only that item's
// invalid result: later requests keep their own results and the exit status
// is 1 solely because of the one rejection.
func TestRegisterHealthSequenceUpperBoundAndOverflow(t *testing.T) {
	const maxSeq = "9223372036854775807"
	const overflow = "9223372036854775808" // one past the signed 64-bit maximum
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + maxSeq + `,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + overflow + `,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains the overflow rejection, exit code: %d, output: %s", code, out)
	}

	// Raw text: the full 19-digit maximum appears verbatim; the overflow
	// digits appear only inside the invalid reason, and nothing is rendered
	// as a float.
	if !strings.Contains(out, maxSeq) {
		t.Fatalf("output must contain the exact maximum sequence %s:\n%s", maxSeq, out)
	}
	if strings.Contains(out, "9.223") || strings.Contains(out, "e+") {
		t.Fatalf("output must not round the boundary integer:\n%s", out)
	}

	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 5 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 1: the maximum legal sequence updates normally.
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 ||
		r.Sequence != 9223372036854775807 {
		t.Fatalf("result 1 healthy at max int64: %+v", r)
	}
	// 2: selection before the rejected item already returns the full integer.
	if r := got.Results[2]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" ||
		r.Sequence != 9223372036854775807 || r.Revision != 1 {
		t.Fatalf("result 2 select at max sequence: %+v", r)
	}
	// 3: one past the bound is this item's own invalid result, with a reason
	// naming the sequence range and the submitted value. It carries no
	// accepted sequence and the batch did not lose its per-item structure.
	if r := got.Results[3]; r.OK || r.Error != "invalid" ||
		!strings.Contains(r.Reason, "sequence must be an integer between 1 and "+maxSeq) ||
		!strings.Contains(r.Reason, "got "+overflow) ||
		r.Revision != 1 || r.Sequence != 0 {
		t.Fatalf("result 3 overflow invalid: %+v", r)
	}
	// 4: selection after the rejection still runs against the previously
	// accepted record: the same instance, address and maximum sequence.
	if r := got.Results[4]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" ||
		r.Sequence != 9223372036854775807 || r.Revision != 1 {
		t.Fatalf("result 4 select after overflow rejection: %+v", r)
	}

	// The final record keeps the full maximum integer; the rejected value
	// neither became the sequence nor changed the revision.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 || len(svc.Instances) != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	wantInst := registerInstance{ID: "i1", Address: "h1:8080", Health: "healthy", Sequence: 9223372036854775807}
	if svc.Instances[0] != wantInst {
		t.Fatalf("final instance: got %+v want %+v", svc.Instances[0], wantInst)
	}
}

// TestRegisterHealthLargeSequencesAllSuccess is the all-success counterpart
// of the rejecting batches: the same large integers accepted without a single
// failure give exit status 0.
func TestRegisterHealthLargeSequencesAllSuccess(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":9007199254740992,"healthy":false,"reason":"心跳超时"},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":9007199254740993,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":9223372036854775807,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("all-success batch must exit 0, got %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 5 {
		t.Fatalf("results: %+v", got.Results)
	}
	// Every item succeeded; the three observations each report their exact
	// sequence, and the final selection returns the accepted maximum.
	wantSeq := []int64{0, 9007199254740992, 9007199254740993, 9223372036854775807, 9223372036854775807}
	for i, r := range got.Results {
		if !r.OK || r.Revision != 1 || r.Sequence != wantSeq[i] {
			t.Fatalf("result %d: %+v want ok sequence %d", i, r, wantSeq[i])
		}
	}
	last := got.Results[4]
	if last.InstanceID != "i1" || last.Address != "h1:8080" {
		t.Fatalf("final select target: %+v", last)
	}
	if svc := got.Services[0]; svc.Revision != 1 ||
		len(svc.Instances) != 1 || svc.Instances[0].Sequence != 9223372036854775807 {
		t.Fatalf("final record: %+v", svc)
	}
}
