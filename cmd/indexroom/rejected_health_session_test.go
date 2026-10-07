package main

// This file is the batch-level regression guard for a session binding meeting a
// REJECTED health report about its bound instance. A stale or same-sequence
// conflicting "unhealthy" observation is not an accepted state change, so it
// must neither knock the bound instance out of the session's reuse path nor
// touch the rotation the keyless selections share:
//
//   - both rejections answer ok:false with the current revision, the accepted
//     sequence and a reason naming the specific rejection, and never carry
//     changed:true; the same-sequence content conflict omits the
//     expectedRevision/actualRevision pair so it cannot be misread as a
//     registration-revision mismatch;
//   - the session whose binding points at the targeted instance keeps reusing
//     it with its current address and accepted sequence — the rejected
//     "unhealthy" content must never push the request back into the rotation
//     and rebind it elsewhere;
//   - a keyless selection interleaved afterwards continues just after the last
//     actually rotated id rather than skipping, restarting or spending an
//     extra rotation slot, and the plain selection rewrites no session binding;
//   - the final service list keeps only accepted health (the bound instance
//     stays healthy at its accepted sequence with no reason), every other
//     instance keeps its address and health record, and the registration
//     revision is not bumped;
//   - the rejected items keep their own result slots in input order and the
//     batch still exits 1 even though the later selections all succeed.

import (
	"encoding/json"
	"testing"
)

// TestRejectedHealthReportKeepsSessionBindingAndRotation drives the exact
// scenario through the register command in one offline batch: three healthy
// instances a, b, c each accepted at sequence 10, session s bound to a, then a
// keyless selection parked the rotation on b. Against a, two rejected
// observations are submitted — sequence 9 (stale) and sequence 10 flipped to
// unhealthy (same-sequence content conflict) — followed by keyed and keyless
// selections that prove neither rejection moved the binding, the accepted
// record or the rotation.
func TestRejectedHealthReportKeepsSessionBindingAndRotation(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"a","address":"h1:1"},
			{"id":"b","address":"h2:2"},
			{"id":"c","address":"h3:3"}
		]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":10,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":10,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":10,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":9,"healthy":false,"reason":"心跳超时"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":10,"healthy":false,"reason":"心跳超时"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains two rejected health items, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	// One result per request, in input order: the rejected items keep their
	// slots and the later successful selections keep theirs.
	if len(got.Results) != 11 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0: registration accepted at revision 1.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0: %+v", r)
	}
	// 1-3: all three instances accept healthy observations at sequence 10
	// without bumping the registration revision.
	for i := 1; i <= 3; i++ {
		if r := got.Results[i]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 10 {
			t.Fatalf("result %d: %+v", i, r)
		}
	}
	// 4: the key's first success rotates to the smallest healthy id and binds
	// s -> a; the cursor rests on a.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("result 4 session bind: %+v", r)
	}
	// 5: a keyless selection continues just after a and parks the cursor on b.
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("result 5 plain select: %+v", r)
	}

	// 6: sequence 9 is below the accepted sequence 10, so the unhealthy
	// observation is stale. The failure states ok:false, the current revision,
	// the accepted sequence and a specific reason, carries no changed flag and
	// no revision comparison pair, and fabricates neither an instance id nor an
	// address.
	if r := got.Results[6]; r.OK || r.Error != "stale" ||
		r.Reason != "sequence 9 is older than the current sequence 10" ||
		r.Revision != 1 || r.Sequence != 10 || r.Changed ||
		!omitsRevisionPair(r) || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("result 6 stale rejection: %+v", r)
	}
	// 7: reusing the accepted sequence 10 with the opposite health content is a
	// conflict, not a registration mismatch: it still reports ok:false, the
	// current revision and the accepted sequence with a specific reason, has no
	// changed flag, and omits expectedRevision/actualRevision.
	if r := got.Results[7]; r.OK || r.Error != "conflict" ||
		r.Reason != "sequence 10 already used with different health content" ||
		r.Revision != 1 || r.Sequence != 10 || r.Changed ||
		!omitsRevisionPair(r) || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("result 7 same-sequence content conflict: %+v", r)
	}

	// 8: the rejected "unhealthy" content was never accepted, so a is still
	// healthy and s reuses its binding — the existing address h1:1 and the
	// accepted sequence 10 — instead of falling back to the rotation and
	// rebinding to another instance.
	if r := got.Results[8]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("result 8 session must still reuse a with its accepted record: %+v", r)
	}
	// 9: the keyless rotation continues just after the b the earlier plain
	// selection parked on and lands on c. Neither rejection skipped c, restarted
	// at a or spent an extra rotation slot, and the session reuse above moved
	// nothing.
	if r := got.Results[9]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("result 9 plain select must continue after b at c: %+v", r)
	}
	// 10: using s again still answers a, proving the plain rotation through c
	// rewrote the session binding no more than the rejected reports did.
	if r := got.Results[10]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("result 10 session must still be bound to a: %+v", r)
	}

	// The final list reflects only accepted state: the service stays at
	// revision 1 (neither the rejected reports nor the selections bumped it),
	// a stays healthy at sequence 10 with no unhealthy reason leaked from the
	// rejected reports, and b and c keep their addresses and accepted records.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 10},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 10},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 10},
	}
	if len(svc.Instances) != len(wantInsts) {
		t.Fatalf("instances: %+v", svc.Instances)
	}
	for i, want := range wantInsts {
		if svc.Instances[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, svc.Instances[i], want)
		}
	}
}
