package main

// This file is the end-to-end regression guard for excludeInstanceIds
// selections that run after the service's instance list has been REPLACED
// within one register batch. It drives JSON batches through runRegister (the
// same path as the CLI; every invocation starts from an empty registry, so
// registration, health reports, selections and the replacement are all
// expressed as items of the one batch) and locks the observable behaviour:
//
//   - with a, b, c, d healthy and the rotation resting on b, replacing the
//     list at the current revision (removing b, keeping the other ids and
//     addresses) leaves the position on the removed b: a select excluding c
//     chooses d, the next plain select wraps to a, and the one after chooses
//     the previously excluded c — the exclusion neither resets the position
//     nor becomes a durable restriction on c;
//   - every successful select reports the post-replacement revision and the
//     address and accepted health sequence of the instance actually chosen;
//   - a select carrying the pre-replacement revision is a conflict naming
//     both revisions, and excluding every healthy instance is no_healthy
//     naming the exclusion; both failures report the current revision and
//     carry no target fields, keep their own result slots, and move no
//     cursor, so the next valid select continues just after the removed b;
//   - a batch containing such failures exits 1 while an all-success batch
//     exits 0; per-item results and the final service list keep the existing
//     output format.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestSelectExcludeAfterReplacementKeepsRotation is the all-success half: the
// whole rotation sequence after the replacement is observable through the
// per-item results, and the final service list proves the exclusion left no
// durable trace. The instance lists are submitted out of id order on purpose:
// the rotation runs by ascending id, never by submission order.
func TestSelectExcludeAfterReplacementKeepsRotation(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"d","address":"h4:4"},
			{"id":"b","address":"h2:2"},
			{"id":"a","address":"h1:1"},
			{"id":"c","address":"h3:3"}
		]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":12,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":13,"healthy":true},
		{"type":"health","service":"svc","instanceId":"d","expectedRevision":1,"sequence":14,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[
			{"id":"d","address":"h4:4"},
			{"id":"a","address":"h1:1"},
			{"id":"c","address":"h3:3"}
		]},
		{"type":"select","service":"svc","expectedRevision":2,"excludeInstanceIds":["c"]},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"select","service":"svc","expectedRevision":2}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("every request succeeds, exit code must be 0, got %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 11 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0: create at revision 1; 1-4: the observations are accepted without
	// bumping the registration revision.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 create: %+v", r)
	}
	for i, seq := range []int64{11, 12, 13, 14} {
		if r := got.Results[1+i]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != seq {
			t.Fatalf("result %d health: %+v", 1+i, r)
		}
	}
	// 5-6: the plain rotation takes a then b; the cursor rests on b.
	if r := got.Results[5]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("result 5 first select: %+v", r)
	}
	if r := got.Results[6]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 12 || r.Revision != 1 {
		t.Fatalf("result 6 second select: %+v", r)
	}
	// 7: the replacement removes b and keeps every other id and address; it
	// succeeds with changed and bumps the revision to 2.
	if r := got.Results[7]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("result 7 replacement: %+v", r)
	}
	// 8: excluding c continues just after the removed b and chooses d with its
	// own address and accepted sequence at the post-replacement revision — the
	// position neither restarts at a nor lands on the excluded c.
	if r := got.Results[8]; !r.OK || r.InstanceID != "d" || r.Address != "h4:4" || r.Sequence != 14 || r.Revision != 2 {
		t.Fatalf("result 8 exclude c should choose d: %+v", r)
	}
	// 9: a plain select wraps past the end to a.
	if r := got.Results[9]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 11 || r.Revision != 2 {
		t.Fatalf("result 9 should wrap to a: %+v", r)
	}
	// 10: the next plain select chooses c — the earlier exclusion was scoped
	// to its one request and never removed c from the rotation.
	if r := got.Results[10]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 13 || r.Revision != 2 {
		t.Fatalf("result 10 should choose the previously excluded c: %+v", r)
	}

	// The final list keeps the existing sorting and field meanings: revision 2,
	// b gone, and the surviving instances healthy with the sequences accepted
	// before the replacement — the exclude request made c neither unhealthy
	// nor unregistered.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 2 {
		t.Fatalf("service view: %+v", svc)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 11},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 13},
		{ID: "d", Address: "h4:4", Health: "healthy", Sequence: 14},
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

// TestSelectExcludeFailuresAfterReplacementKeepPosition is the failure half:
// after the replacement, a stale-revision select conflicts and an
// exclude-everything select is no_healthy. Both failures keep their own
// result slots, report the current revision, carry no target fields in the
// raw JSON, and move no cursor — the valid selects after them continue just
// after the removed b. The batch exits 1 because it contains failures.
func TestSelectExcludeFailuresAfterReplacementKeepPosition(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"a","address":"h1:1"},
			{"id":"b","address":"h2:2"},
			{"id":"c","address":"h3:3"},
			{"id":"d","address":"h4:4"}
		]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":12,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":13,"healthy":true},
		{"type":"health","service":"svc","instanceId":"d","expectedRevision":1,"sequence":14,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[
			{"id":"a","address":"h1:1"},
			{"id":"c","address":"h3:3"},
			{"id":"d","address":"h4:4"}
		]},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":2,"excludeInstanceIds":["a","c","d"]},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"select","service":"svc","expectedRevision":2}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains failures, exit code must be 1, got %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	// One result per request, in input order: the failed items keep their
	// slots and the later valid selects are still processed.
	if len(got.Results) != 12 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 5-6: the plain rotation takes a then b; the cursor rests on b.
	if r := got.Results[5]; !r.OK || r.InstanceID != "a" || r.Sequence != 11 {
		t.Fatalf("result 5 first select: %+v", r)
	}
	if r := got.Results[6]; !r.OK || r.InstanceID != "b" || r.Sequence != 12 {
		t.Fatalf("result 6 second select: %+v", r)
	}
	// 7: the replacement removes b and bumps the revision to 2.
	if r := got.Results[7]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("result 7 replacement: %+v", r)
	}
	// 8: a caller still using the pre-replacement revision gets a conflict
	// stating the expected and actual revisions and reporting the current one.
	if r := got.Results[8]; r.OK || r.Error != "conflict" ||
		r.Reason != `service "svc" is at revision 2, not 1` ||
		r.Revision != 2 || !hasRevisionPair(r, 1, 2) {
		t.Fatalf("result 8 stale-revision select must conflict: %+v", r)
	}
	// 9: the correct revision with every healthy instance excluded is
	// no_healthy; the reason says the exclusion removed them all.
	if r := got.Results[9]; r.OK || r.Error != "no_healthy" ||
		!strings.Contains(r.Reason, "excludeInstanceIds") || r.Revision != 2 {
		t.Fatalf("result 9 exclude-everything must be no_healthy naming the exclusion: %+v", r)
	}
	// Both failures fabricate no target: the raw JSON of their results carries
	// no instanceId, address or sequence field at all.
	for _, i := range []int{8, 9} {
		raw := string(mustResultJSON(t, out, i))
		for _, field := range []string{`"instanceId"`, `"address"`, `"sequence"`} {
			if strings.Contains(raw, field) {
				t.Fatalf("result %d must carry no %s field: %s", i, field, raw)
			}
		}
	}
	// 10: the failures moved no cursor — the rotation continues just after the
	// removed b and picks c, not d (skipping the due instance) and not a
	// (restarting from the smallest id).
	if r := got.Results[10]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 13 || r.Revision != 2 {
		t.Fatalf("result 10 should resume after the removed b at c: %+v", r)
	}
	// 11: only the following select moves on to d.
	if r := got.Results[11]; !r.OK || r.InstanceID != "d" || r.Address != "h4:4" || r.Sequence != 14 || r.Revision != 2 {
		t.Fatalf("result 11 should continue to d: %+v", r)
	}

	// The final list reflects only the committed state: revision 2, b removed,
	// and the surviving instances still healthy with their accepted sequences —
	// the failed selects changed nothing.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 2 {
		t.Fatalf("service view: %+v", svc)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 11},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 13},
		{ID: "d", Address: "h4:4", Health: "healthy", Sequence: 14},
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
