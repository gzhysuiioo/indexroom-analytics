package main

// This file is the end-to-end regression guard for release_session applied to
// a session whose binding was REWRITTEN by an excludeInstanceIds selection
// earlier in the same register batch. It drives JSON batches through
// runRegister (the same path as the CLI; every invocation starts from an
// empty registry, so registration, health reports, the exclude-driven rebind,
// the release and every confirming selection are all items of the one batch)
// and locks the observable behaviour, using only public request/response
// content — bindings and the rotation position have no query interface and
// are observed exclusively through later selections:
//
//   - with a, b, c healthy, sessions x and y bind a and b; x then selects
//     with excludeInstanceIds [a, c], falls back to the filtered rotation and
//     rebinds to b, so x and y both point at b and the last actually rotated
//     position rests on b;
//   - releasing x now removes only that current binding: changed:true with
//     the current registration revision and no instanceId, address or health
//     sequence anywhere in the result; y still reuses b, and the released x
//     rotates from just after b to c and binds there;
//   - x's reuse of c and the following plain select wrapping to a prove the
//     release and the reuses moved no cursor, and that the earlier exclusion
//     scoped only its own request (a and c are both eligible again);
//   - a release carrying a valid but mismatching expectedRevision conflicts
//     with the current revision and a reason, preserves the rebound b binding
//     and every other session and the cursor, and a later release at the
//     correct revision still takes effect; releasing the just-released key
//     again succeeds without changed and without target fields;
//   - the whole sequence never alters the instance list, addresses or
//     accepted health records: the final service list reflects the original
//     state. A batch containing the conflict exits 1 while the all-success
//     batch exits 0; per-item results keep the existing output format.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestReleaseSessionAfterExcludeRebind is the all-success half: x rebinds
// from a to b through a one-request exclusion, the release drops exactly that
// current binding, and every later selection confirms the surviving session,
// the untouched cursor and the expired exclusion scope.
func TestReleaseSessionAfterExcludeRebind(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"a","address":"h1:1"},
			{"id":"b","address":"h2:2"},
			{"id":"c","address":"h3:3"}
		]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x","excludeInstanceIds":["a","c"]},
		{"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("every request succeeds, exit code must be 0, got %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 12 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0: create at revision 1; 1-3: the observations are accepted without
	// bumping the registration revision.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 create: %+v", r)
	}
	for i, seq := range []int64{1, 2, 3} {
		if r := got.Results[1+i]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != seq {
			t.Fatalf("result %d health: %+v", 1+i, r)
		}
	}
	// 4-5: the first use of each key rotates and binds x -> a then y -> b; the
	// shared cursor rests on b.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 4 x binds a: %+v", r)
	}
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 || r.Revision != 1 {
		t.Fatalf("result 5 y binds b: %+v", r)
	}
	// 6: x's bound a is excluded for this one request, so x falls back to the
	// filtered rotation continuing just after b; the only surviving candidate
	// is b itself, and the success rebinds x -> b. Now both sessions point at
	// b and the last actually rotated position is b.
	if r := got.Results[6]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 || r.Revision != 1 {
		t.Fatalf("result 6 x should rebind to b: %+v", r)
	}
	// 7: releasing x drops exactly that current binding: changed:true at the
	// current revision, and the raw result carries no instanceId, address or
	// health sequence — a release chooses no target.
	if r := got.Results[7]; !r.OK || !r.Changed || r.Revision != 1 ||
		r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
		t.Fatalf("result 7 release x: %+v", r)
	}
	raw := string(mustResultJSON(t, out, 7))
	for _, field := range []string{`"instanceId"`, `"address"`, `"sequence"`} {
		if strings.Contains(raw, field) {
			t.Fatalf("result 7 release must carry no %s field: %s", field, raw)
		}
	}
	// 8: y's binding to the same instance b is untouched by x's release.
	if r := got.Results[8]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("result 8 y must keep b: %+v", r)
	}
	// 9: the released x submits no exclusion and behaves like a first binding:
	// it rotates from just after the last real position (b) and lands on c —
	// not on a, which would mean the release reset the cursor, and not on b,
	// which would mean the release left the binding in place.
	if r := got.Results[9]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 3 || r.Revision != 1 {
		t.Fatalf("result 9 released x should rotate to c: %+v", r)
	}
	// 10: the success rebound x -> c, so the next request reuses c without
	// rotating.
	if r := got.Results[10]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 3 {
		t.Fatalf("result 10 x should reuse c: %+v", r)
	}
	// 11: a plain select continues just after c and wraps to a. Neither the
	// release nor the reuses advanced the rotation, and the earlier exclusion
	// of a and c scoped only its own request — both are eligible again.
	if r := got.Results[11]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 11 plain select should wrap to a: %+v", r)
	}

	// The final list reflects the original state: revision 1, the same ids
	// and addresses, and the accepted health records — the exclusion, the
	// release and every selection changed none of them.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 1},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 2},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 3},
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

// TestReleaseSessionConflictAfterExcludeRebind is the failure half: after x
// rebinds to b through the exclusion, a release with a valid but mismatching
// expectedRevision conflicts and preserves everything — the rebound binding,
// the other session and the cursor — the correct-revision release then takes
// effect, and a repeat release of the already-released key succeeds without
// changed. The batch contains the conflict, so it exits 1 while every later
// request is still processed.
func TestReleaseSessionConflictAfterExcludeRebind(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"a","address":"h1:1"},
			{"id":"b","address":"h2:2"},
			{"id":"c","address":"h3:3"}
		]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x","excludeInstanceIds":["a","c"]},
		{"type":"release_session","service":"svc","expectedRevision":9,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"y"},
		{"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains a conflict, exit code must be 1, got %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	// One result per request, in input order: the failed release keeps its
	// slot and the later requests are still processed.
	if len(got.Results) != 14 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 4-5: x binds a, y binds b; the cursor rests on b.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Sequence != 1 {
		t.Fatalf("result 4 x binds a: %+v", r)
	}
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Sequence != 2 {
		t.Fatalf("result 5 y binds b: %+v", r)
	}
	// 6: the one-request exclusion of a and c rebinds x -> b.
	if r := got.Results[6]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("result 6 x should rebind to b: %+v", r)
	}
	// 7: the fields are valid but expectedRevision 9 does not match the
	// current revision 1: conflict carrying both revisions, reporting the
	// current one and stating a reason — and choosing no target.
	if r := got.Results[7]; r.OK || r.Error != "conflict" ||
		r.Reason == "" || r.Revision != 1 ||
		!hasRevisionPair(r, 9, 1) ||
		r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
		t.Fatalf("result 7 wrong-revision release must conflict: %+v", r)
	}
	raw := string(mustResultJSON(t, out, 7))
	for _, field := range []string{`"instanceId"`, `"address"`, `"sequence"`, `"changed"`} {
		if strings.Contains(raw, field) {
			t.Fatalf("result 7 conflict must carry no %s field: %s", field, raw)
		}
	}
	// 8: the conflict preserved the rebound binding — x still reuses b rather
	// than rotating to c.
	if r := got.Results[8]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("result 8 x must keep the rebound b: %+v", r)
	}
	// 9: the other session pointing at the same instance is equally untouched.
	if r := got.Results[9]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("result 9 y must keep b: %+v", r)
	}
	// 10: the correct revision releases the binding now: changed:true at the
	// current revision, with no target fields.
	if r := got.Results[10]; !r.OK || !r.Changed || r.Revision != 1 ||
		r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
		t.Fatalf("result 10 release at the correct revision: %+v", r)
	}
	// 11: the binding is already gone, so the repeat release still succeeds
	// but changed must be absent from the raw JSON, and no target fields
	// appear either.
	if r := got.Results[11]; !r.OK || r.Changed || r.Revision != 1 ||
		r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
		t.Fatalf("result 11 repeat release should succeed unchanged: %+v", r)
	}
	raw = string(mustResultJSON(t, out, 11))
	for _, field := range []string{`"changed"`, `"instanceId"`, `"address"`, `"sequence"`} {
		if strings.Contains(raw, field) {
			t.Fatalf("result 11 unchanged release must carry no %s field: %s", field, raw)
		}
	}
	// 12: the released x rejoins the rotation just after the last real
	// position (b) and binds to c — the conflict and the repeat release moved
	// no cursor.
	if r := got.Results[12]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 3 || r.Revision != 1 {
		t.Fatalf("result 12 released x should rotate to c: %+v", r)
	}
	// 13: a plain select wraps past the end to a, proving the whole
	// release/failure sequence left the shared rotation exactly where the
	// real selections put it.
	if r := got.Results[13]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 13 plain select should wrap to a: %+v", r)
	}

	// The final list reflects only the original committed state: revision 1,
	// the same ids, addresses and accepted health records — neither the
	// failed nor the successful releases altered them.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 1},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 2},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 3},
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
