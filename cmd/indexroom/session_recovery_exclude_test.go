package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// This file is the end-to-end guard for session stickiness across the failure
// shape where the bound instance is unhealthy and the same request's
// excludeInstanceIds list removes every remaining healthy instance. The
// filtered no_healthy failure must preserve the session binding and the
// rotation position, and the public JSON output must show it: the batch
// containing a failure exits 1, later health observations and selections
// still produce their own results, and the final service list keeps the
// original instances, addresses and registration revision, with health
// reflecting only accepted observations.

// TestSelectSessionExcludedFailureKeepsBindingForRecoveredReuse runs the
// recovery path through the JSON surface: session x is bound to a, a turns
// unhealthy, and x's request excluding b and c fails with no_healthy. After a
// recovers, a request excluding only b must reuse the preserved binding a —
// not re-rotate to c — and the following plain selection must continue from
// the cursor's untouched position on b.
func TestSelectSessionExcludedFailureKeepsBindingForRecoveredReuse(t *testing.T) {
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
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":2,"healthy":false,"reason":"心跳超时"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x","excludeInstanceIds":["b","c"]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x","excludeInstanceIds":["b"]},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code == 0 {
		t.Fatalf("batch contains a failure, exit code must be 1, output: %s", out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 11 {
		t.Fatalf("results: %+v", got.Results)
	}

	// Items 1-4 commit the service and three healthy instances at sequence 1.
	for i := 0; i < 4; i++ {
		if !got.Results[i].OK {
			t.Fatalf("setup item %d should succeed: %+v", i+1, got.Results[i])
		}
	}
	// Item 5: x's first selection rotates to the smallest id and binds x -> a.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("item 5 should bind a, got %+v", r)
	}
	// Item 6: the plain rotation advances to b; the cursor rests on b.
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 1 {
		t.Fatalf("item 6 should choose b, got %+v", r)
	}
	// Item 7: a accepts a larger-sequence unhealthy observation with a
	// non-empty reason; the registration revision stays 1.
	if r := got.Results[6]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 2 {
		t.Fatalf("item 7 should record a's unhealthy observation, got %+v", r)
	}
	// Item 8: the bound instance a is unhealthy and this request excludes the
	// only remaining healthy instances b and c — no_healthy, the reason says
	// the exclusion removed the healthy instances, and the failure fabricates
	// no instanceId, address or sequence.
	if r := got.Results[7]; r.OK || r.Error != "no_healthy" || r.Revision != 1 ||
		!strings.Contains(r.Reason, "excludeInstanceIds") ||
		r.InstanceID != "" || r.Address != "" || r.Sequence != 0 || !omitsRevisionPair(r) {
		t.Fatalf("item 8 should be no_healthy naming the exclusion, got %+v", r)
	}
	// Item 9: a accepts a larger-sequence healthy observation and recovers.
	if r := got.Results[8]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 3 {
		t.Fatalf("item 9 should record a's recovery, got %+v", r)
	}
	// Item 10: excluding only b, x reuses the recovered binding a — the failed
	// item 8 did not clear it, so the request does not re-rotate to the also
	// selectable c. The result carries a's current address, its latest health
	// sequence 3 and the unchanged registration revision.
	if r := got.Results[9]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 3 || r.Revision != 1 {
		t.Fatalf("item 10 should reuse the recovered binding a, got %+v", r)
	}
	// Item 11: the plain rotation still rests on b — neither the failure nor
	// the session reuse moved it — so this selection continues to c.
	if r := got.Results[10]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("item 11 should continue the rotation at c, got %+v", r)
	}

	// The final list keeps the original instances and addresses at revision 1;
	// health reflects only the accepted observations: a healthy at sequence 3
	// with its reason cleared, b and c untouched at sequence 1.
	if len(got.Services) != 1 || got.Services[0].Service != "svc" || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 3},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 1},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 1},
	}
	insts := got.Services[0].Instances
	if len(insts) != len(wantInsts) {
		t.Fatalf("instances: %+v", insts)
	}
	for i, want := range wantInsts {
		if insts[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, insts[i], want)
		}
	}
}

// TestSelectSessionRecoveryRespectsCurrentExclusion runs the complementary
// path: after the same filtered failure, a recovers, but x's next request
// excludes a and b, so the recovered binding cannot be reused — the request
// falls back to the filtered rotation, chooses c and rebinds x -> c. The
// earlier exclusion stays scoped to its own request: it removed no instance
// from the registry and rewrote no health record.
func TestSelectSessionRecoveryRespectsCurrentExclusion(t *testing.T) {
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
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":2,"healthy":false,"reason":"连接失败"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x","excludeInstanceIds":["b","c"]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x","excludeInstanceIds":["a","b"]},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"x"},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code == 0 {
		t.Fatalf("batch contains a failure, exit code must be 1, output: %s", out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 12 {
		t.Fatalf("results: %+v", got.Results)
	}

	// Items 1-7 reproduce the shared prelude: x bound to a, cursor on b, then
	// a knocked unhealthy by a larger-sequence observation with a reason.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 {
		t.Fatalf("item 5 should bind a, got %+v", r)
	}
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 1 {
		t.Fatalf("item 6 should choose b, got %+v", r)
	}
	if r := got.Results[6]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 2 {
		t.Fatalf("item 7 should record a's unhealthy observation, got %+v", r)
	}
	// Item 8: bound a unhealthy, b and c excluded for this one request —
	// no_healthy naming the exclusion, with no target fields.
	if r := got.Results[7]; r.OK || r.Error != "no_healthy" || r.Revision != 1 ||
		!strings.Contains(r.Reason, "excludeInstanceIds") ||
		r.InstanceID != "" || r.Address != "" || r.Sequence != 0 || !omitsRevisionPair(r) {
		t.Fatalf("item 8 should be no_healthy naming the exclusion, got %+v", r)
	}
	// Item 9: a recovers with a larger-sequence healthy observation.
	if r := got.Results[8]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 3 {
		t.Fatalf("item 9 should record a's recovery, got %+v", r)
	}
	// Item 10: recovery does not ignore THIS request's exclusion — a and b are
	// excluded, so the recovered binding a cannot be reused. The filtered
	// rotation from the cursor on b chooses c and rebinds x -> c.
	if r := got.Results[9]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("item 10 should fall back to c and rebind, got %+v", r)
	}
	// Item 11: without a list, x reuses the new binding c — item 8's exclusion
	// of c was scoped to that one request and did not bar c from binding.
	if r := got.Results[10]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 1 {
		t.Fatalf("item 11 should reuse the new binding c, got %+v", r)
	}
	// Item 12: the plain rotation continues just after the last real rotation
	// (c) and wraps to a, now healthy again at sequence 3.
	if r := got.Results[11]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 3 || r.Revision != 1 {
		t.Fatalf("item 12 should wrap the rotation to a, got %+v", r)
	}

	// The exclusions were per-request filters: all three instances are still
	// registered at their original addresses, the revision never moved, and
	// the health records show only the accepted observations.
	if len(got.Services) != 1 || got.Services[0].Service != "svc" || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 3},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 1},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 1},
	}
	insts := got.Services[0].Instances
	if len(insts) != len(wantInsts) {
		t.Fatalf("instances: %+v", insts)
	}
	for i, want := range wantInsts {
		if insts[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, insts[i], want)
		}
	}
}
