package main

// This file is the end-to-end regression guard for session stickiness across
// a bound instance's unhealthy gap when the SAME select request also excludes
// the remaining healthy instances. It drives JSON batches through runRegister
// (the same path as the CLI; every invocation starts from an empty registry,
// so registration, the offline health observations and every selection are
// items of the one batch, processed in input order) and locks the observable
// behaviour:
//
//   - with a, b, c healthy, session x binds to a through the normal rotation
//     and a following plain select moves the cursor to b; once a accepts a
//     higher-sequence unhealthy observation with a non-empty reason, a select
//     for x that excludes b and c is no_healthy — the reason says the healthy
//     instances were all excluded by excludeInstanceIds, and the failure
//     carries no instanceId, address or sequence, keeps the x -> a binding
//     and leaves the rotation resting on b;
//   - after a accepts a higher-sequence healthy observation, a select for x
//     that excludes only b reuses the recovered a with its current address,
//     latest health sequence and the current registration revision — the
//     failure never cleared the binding, and the reuse moves no cursor, so
//     the next plain select continues just after b and chooses c;
//   - recovery does not suspend the exclusion: after the same failure, a
//     select for x that excludes the recovered a (and b) falls back to the
//     filtered rotation and rebinds x -> c; x then reuses c without a list,
//     and the plain rotation continues from c back to a — the earlier
//     exclusions were scoped to their own requests and removed nothing;
//   - a batch containing the no_healthy item exits 1 while later health
//     reports and selections still produce their per-item results; the final
//     service list keeps the original instances and addresses at the
//     unchanged registration revision, with health state, sequence and reason
//     reflecting only the accepted observations.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSelectSessionRecoveryAfterExcludedFailureKeepsRotation is the reuse
// half: the failed select neither clears the x -> a binding nor moves the
// rotation off b, so once a is healthy again the session reuses it (even
// though c would also be eligible) and the plain rotation resumes at c.
func TestSelectSessionRecoveryAfterExcludedFailureKeepsRotation(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"a","address":"h1:1"},
			{"id":"b","address":"h2:2"},
			{"id":"c","address":"h3:3"}
		]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":2,"healthy":false,"reason":"连接超时"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x","excludeInstanceIds":["b","c"]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x","excludeInstanceIds":["b"]},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains a failure, exit code must be 1, got %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	// One result per request, in input order: the failed item keeps its slot
	// and the later health report and selections are still processed.
	if len(got.Results) != 11 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0: create at revision 1; 1-3: the observations are accepted without
	// bumping the registration revision.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 create: %+v", r)
	}
	for i := 1; i <= 3; i++ {
		if r := got.Results[i]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 1 {
			t.Fatalf("result %d health: %+v", i, r)
		}
	}
	// 4: the session's first select rotates to the smallest id and binds
	// x -> a; the cursor rests on a.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 4 session bind: %+v", r)
	}
	// 5: the plain select continues just after a and takes b; the cursor
	// rests on b.
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 5 plain select: %+v", r)
	}
	// 6: a accepts a higher-sequence unhealthy observation with a non-empty
	// reason; the registration revision stays 1.
	if r := got.Results[6]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 2 {
		t.Fatalf("result 6 unhealthy observation: %+v", r)
	}
	// 7: the bound a is unhealthy and this request excludes the only healthy
	// instances b and c — no_healthy, and the reason says the exclusion
	// removed them all. The failure reports the current revision, no revision
	// pair, and fabricates no target.
	if r := got.Results[7]; r.OK || r.Error != "no_healthy" ||
		r.Reason != `service "svc" has no healthy instance available: all healthy instances are excluded by excludeInstanceIds` ||
		r.Revision != 1 || !omitsRevisionPair(r) {
		t.Fatalf("result 7 must be no_healthy naming the exclusion: %+v", r)
	}
	raw := string(mustResultJSON(t, out, 7))
	for _, field := range []string{`"instanceId"`, `"address"`, `"sequence"`} {
		if strings.Contains(raw, field) {
			t.Fatalf("result 7 must carry no %s field: %s", field, raw)
		}
	}
	// 8: a accepts a higher-sequence healthy observation and recovers; the
	// reason is cleared and the revision still does not move.
	if r := got.Results[8]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 3 {
		t.Fatalf("result 8 recovery observation: %+v", r)
	}
	// 9: excluding only b, the session reuses the recovered a — the failed
	// select never cleared the binding, so the request does not fall back to
	// the rotation (which would have chosen c). The result carries a's
	// current address, its latest accepted sequence and the current
	// registration revision, and the reuse advances no cursor.
	if r := got.Results[9]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 3 || r.Revision != 1 {
		t.Fatalf("result 9 should reuse the recovered a: %+v", r)
	}
	// 10: the plain rotation still rests on b — neither the failed select nor
	// the session reuse moved it — so it continues just after b and takes c.
	if r := got.Results[10]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 10 should resume after b at c: %+v", r)
	}

	// The final list reflects only the committed state: revision unchanged at
	// 1, the original instances and addresses, and health records holding only
	// the accepted observations — a healthy at sequence 3 with its unhealthy
	// reason cleared, b and c untouched at sequence 1.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 3},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 1},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 1},
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

// TestSelectSessionRecoveryStillHonoursExclusion is the rebind half: after the
// same no_healthy failure, a recovers but the session's next select excludes a
// itself, so the request falls back to the filtered rotation and rebinds
// x -> c. The exclusion scoped only that one request — x reuses c without a
// list, and the plain rotation continues from c back to the recovered a.
func TestSelectSessionRecoveryStillHonoursExclusion(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"a","address":"h1:1"},
			{"id":"b","address":"h2:2"},
			{"id":"c","address":"h3:3"}
		]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":2,"healthy":false,"reason":"连接超时"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x","excludeInstanceIds":["b","c"]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x","excludeInstanceIds":["a","b"]},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains a failure, exit code must be 1, got %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 12 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 4: the session binds x -> a through the normal rotation; 5: the plain
	// select moves the cursor to b.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("result 4 session bind: %+v", r)
	}
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 1 {
		t.Fatalf("result 5 plain select: %+v", r)
	}
	// 6: a turns unhealthy at the higher sequence 2 with a non-empty reason.
	if r := got.Results[6]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 2 {
		t.Fatalf("result 6 unhealthy observation: %+v", r)
	}
	// 7: with a unhealthy and b, c excluded this request, the select is
	// no_healthy naming the exclusion and carrying no target fields; the
	// failure keeps the x -> a binding and the cursor on b.
	if r := got.Results[7]; r.OK || r.Error != "no_healthy" ||
		r.Reason != `service "svc" has no healthy instance available: all healthy instances are excluded by excludeInstanceIds` ||
		r.Revision != 1 || !omitsRevisionPair(r) {
		t.Fatalf("result 7 must be no_healthy naming the exclusion: %+v", r)
	}
	raw := string(mustResultJSON(t, out, 7))
	for _, field := range []string{`"instanceId"`, `"address"`, `"sequence"`} {
		if strings.Contains(raw, field) {
			t.Fatalf("result 7 must carry no %s field: %s", field, raw)
		}
	}
	// 8: a recovers at the higher sequence 3.
	if r := got.Results[8]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 3 {
		t.Fatalf("result 8 recovery observation: %+v", r)
	}
	// 9: recovery does not suspend this request's exclusion — the recovered
	// binding a is excluded along with b, so the session falls back to the
	// filtered rotation. The only candidate is c: the request chooses it,
	// rebinds x -> c and advances the cursor to c.
	if r := got.Results[9]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 9 should fall back to c and rebind: %+v", r)
	}
	// 10: the earlier exclusion was scoped to its own request — without a
	// list the session reuses the new binding c, moving no cursor.
	if r := got.Results[10]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 10 should reuse the new binding c: %+v", r)
	}
	// 11: the plain rotation continues just after c and wraps to a, which is
	// healthy again and reports its latest accepted sequence 3 — the
	// exclusions never removed a from the registry nor rewrote its record.
	if r := got.Results[11]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 3 || r.Revision != 1 {
		t.Fatalf("result 11 should wrap from c to the recovered a: %+v", r)
	}

	// The final list keeps the original instances and addresses at the
	// unchanged registration revision; health state, sequence and reason
	// reflect only the accepted observations.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 3},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 1},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 1},
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
