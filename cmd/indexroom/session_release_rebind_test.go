package main

// This file is the end-to-end regression guard for releasing a session whose
// binding was REBOUND by an excludeInstanceIds selection earlier in the same
// register batch. Every register invocation starts from an empty registry, so
// registration, health reports, the two sessions, the exclude-driven rebind
// and every release are all items of one JSON batch; the bindings and rotation
// position have no query interface and are observed only through the per-item
// results and later selections.
//
// With a, b, c healthy the locked lifecycle is:
//
//   - sessions x and y first succeed in turn, binding x -> a then y -> b, so
//     the shared rotation rests on b;
//   - x then selects excluding a and c: its bound a is excluded, so the
//     filtered rotation from just after b wraps onto the only candidate b and
//     x rebounds to b — x and y then both answer b, and the cursor stays on b;
//   - release_session on x removes only x's current binding (changed:true at
//     the current revision, with no instance id, address or health sequence),
//     leaves y pinned to the same instance b and moves the cursor nowhere;
//   - the released x with no exclude list rotates just after b to c and
//     establishes a fresh binding, a later x reuses c, and a plain select wraps
//     to a — the rebind and release added no extra rotation step and the
//     earlier exclude list carried to no later request.
//
// The failure half locks the post-rebind conflict: a release carrying a legal
// but mismatched expectedRevision returns conflict with both revisions and a
// reason, fabricates no target, and preserves the rebound binding and the
// rotation (x still answers b, a plain select continues to c); a later release
// at the correct revision still takes effect. Releasing the same key again
// before it is rebound succeeds without changed and with no target fields.
// The all-success batch exits 0; the batch containing the conflict keeps
// processing later items and exits 1. Neither batch changes the instance
// list, addresses or accepted health records in the final services list.

import (
	"encoding/json"
	"strings"
	"testing"
)

// assertNoReleaseTargetRaw fails unless the raw per-item JSON at index carries
// none of the target fields a release must never emit (instanceId, address,
// sequence).
func assertNoReleaseTargetRaw(t *testing.T, out string, index int) {
	t.Helper()
	raw := string(mustResultJSON(t, out, index))
	for _, field := range []string{`"instanceId"`, `"address"`, `"sequence"`} {
		if strings.Contains(raw, field) {
			t.Fatalf("result %d must carry no %s field: %s", index, field, raw)
		}
	}
}

// assertFinalABCState locks the tail invariant shared by both batches: the
// service stays at revision 1 with a, b, c at their original addresses and
// accepted healthy records (sequences 1, 2, 3, no reasons) — sessions,
// exclusions and releases change none of it.
func assertFinalABCState(t *testing.T, got registerOutput) {
	t.Helper()
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

// TestReleaseSessionAfterExcludeRebind is the all-success half: x is rebound
// to b by an exclude list while y is already bound there, the release removes
// only x's current binding, and the subsequent selections prove y's binding,
// the rotation position and the one-request exclusion scope all behave. The
// batch exits 0 because every item succeeds and the result is written.
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
	if len(got.Results) != 13 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0: created at revision 1; 1-3: the observations are accepted without
	// bumping the registration revision.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 create: %+v", r)
	}
	for i, seq := range []int64{1, 2, 3} {
		if r := got.Results[1+i]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != seq {
			t.Fatalf("result %d health: %+v", 1+i, r)
		}
	}
	// 4: x's first success rotates to a and binds x -> a.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 4 x binds a: %+v", r)
	}
	// 5: y's first success continues the same rotation just after a to b and
	// binds y -> b; the cursor rests on b.
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 || r.Revision != 1 {
		t.Fatalf("result 5 y binds b: %+v", r)
	}
	// 6: x excludes a and c for this one request: its bound a cannot be reused,
	// so the filtered rotation from just after b wraps onto the only candidate
	// b. x rebounds to b (the instance y is already bound to) and the cursor
	// stays on b.
	if r := got.Results[6]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 || r.Revision != 1 {
		t.Fatalf("result 6 x must rebind to b through the filtered rotation: %+v", r)
	}
	// 7: releasing the rebound binding reports changed:true at the current
	// revision and carries no target id, address or health sequence.
	if r := got.Results[7]; !r.OK || !r.Changed || r.Revision != 1 ||
		r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
		t.Fatalf("result 7 release of the rebound binding: %+v", r)
	}
	assertNoReleaseTargetRaw(t, out, 7)
	// 8: releasing again before x is rebound still succeeds, but changed must
	// be absent and no target fields may appear.
	if r := got.Results[8]; !r.OK || r.Changed || r.Revision != 1 ||
		r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
		t.Fatalf("result 8 repeat release while unbound: %+v", r)
	}
	assertNoReleaseTargetRaw(t, out, 8)
	if strings.Contains(string(mustResultJSON(t, out, 8)), `"changed"`) {
		t.Fatalf("result 8 raw JSON must not carry a changed field: %s", mustResultJSON(t, out, 8))
	}
	// 9: y keeps its own binding to b even though x's binding to that same
	// instance was just removed.
	if r := got.Results[9]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("result 9 y must stay bound to b: %+v", r)
	}
	// 10: the released x, with no exclude list, joins the rotation just after
	// the last real position b and lands on c, establishing a new binding. It
	// is not b (the cursor did not move) and not a (the earlier exclusion of a
	// did not persist to this request).
	if r := got.Results[10]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 3 || r.Revision != 1 {
		t.Fatalf("result 10 released x should rotate to c: %+v", r)
	}
	// 11: x reuses the fresh binding c without rotating.
	if r := got.Results[11]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 3 {
		t.Fatalf("result 11 x should reuse c: %+v", r)
	}
	// 12: a plain select wraps from the last real rotation (c) back to a:
	// neither the exclude-driven rebind, the releases nor the session reuses
	// added an extra rotation step.
	if r := got.Results[12]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("result 12 plain select should wrap to a: %+v", r)
	}

	// The whole sequence changed neither the instance list, an address nor an
	// accepted health record.
	assertFinalABCState(t, got)
}

// TestReleaseSessionAfterExcludeRebindConflict is the failure half: after the
// exclude-driven rebind, a release with a legal but mismatched
// expectedRevision conflicts with the current revision and a clear reason,
// removes nothing, and the rebound binding plus the rotation position survive
// for every later item. The batch keeps processing and exits 1, and the
// correct-revision release afterwards still takes effect.
func TestReleaseSessionAfterExcludeRebindConflict(t *testing.T) {
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
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains a conflict, exit code must be 1, got %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	// One result per request, in input order: the failed item keeps its slot
	// and every later item is still processed.
	if len(got.Results) != 16 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 4-5: x -> a then y -> b; the cursor rests on b.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("result 4 x binds a: %+v", r)
	}
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("result 5 y binds b: %+v", r)
	}
	// 6: the exclude-a,c selection rebounds x to b and leaves the cursor on b.
	if r := got.Results[6]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("result 6 x rebinds to b: %+v", r)
	}
	// 7: a legal-but-mismatched expectedRevision conflicts, stating the
	// submitted and current revisions and the current revision, with a reason
	// that names both. It fabricates no target fields.
	if r := got.Results[7]; r.OK || r.Error != "conflict" ||
		r.Reason != `service "svc" is at revision 1, not 9` ||
		r.Revision != 1 || r.ExpectedRevision != 9 || r.ActualRevision != 1 ||
		r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
		t.Fatalf("result 7 mismatched release must conflict: %+v", r)
	}
	assertNoReleaseTargetRaw(t, out, 7)
	// 8: x still reuses the rebound b — the failed release preserved that
	// binding.
	if r := got.Results[8]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("result 8 x must still reuse the rebound b: %+v", r)
	}
	// 9: y and its binding are untouched.
	if r := got.Results[9]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("result 9 y must stay bound to b: %+v", r)
	}
	// 10: a plain select continues just after the last real rotation (b, set
	// by result 6) to c — the failed release moved neither cursor nor binding.
	if r := got.Results[10]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 3 || r.Revision != 1 {
		t.Fatalf("result 10 plain select must continue after b to c: %+v", r)
	}
	// 11: that plain rotation rewrote no session binding: x still answers b.
	if r := got.Results[11]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 {
		t.Fatalf("result 11 x must still be bound to b: %+v", r)
	}
	// 12: releasing at the correct revision now takes effect (changed:true, no
	// target fields); it removes x's rebound binding only.
	if r := got.Results[12]; !r.OK || !r.Changed || r.Revision != 1 ||
		r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
		t.Fatalf("result 12 correct-revision release should change: %+v", r)
	}
	assertNoReleaseTargetRaw(t, out, 12)
	// 13: releasing again before x is rebound succeeds without changed and
	// returns no target fields.
	if r := got.Results[13]; !r.OK || r.Changed || r.Revision != 1 ||
		r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
		t.Fatalf("result 13 repeat release while unbound: %+v", r)
	}
	assertNoReleaseTargetRaw(t, out, 13)
	if strings.Contains(string(mustResultJSON(t, out, 13)), `"changed"`) {
		t.Fatalf("result 13 raw JSON must not carry a changed field: %s", mustResultJSON(t, out, 13))
	}
	// 14: the released x joins the rotation just after the last real position
	// (c from result 10) and wraps to a — it does not reuse the removed binding
	// to b, and a is choosable because the earlier exclude list never
	// persisted. It establishes a fresh binding.
	if r := got.Results[14]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 14 released x should rotate from c to a: %+v", r)
	}
	// 15: x reuses the fresh binding a without rotating.
	if r := got.Results[15]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("result 15 x should reuse a: %+v", r)
	}

	// Even with the conflict in the middle, the final list reflects only the
	// original registration and accepted health observations.
	assertFinalABCState(t, got)
}
