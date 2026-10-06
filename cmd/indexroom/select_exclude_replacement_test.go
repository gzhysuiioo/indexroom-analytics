package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// This file is the end-to-end regression guard for the rotation position once a
// service's instance list has been replaced while the rotation is in use and a
// later select carries a one-request excludeInstanceIds list. It exercises the
// exact public contract through the register command's JSON: each invocation
// starts from an empty registry, so the registration, health observations,
// selections and the replacement all ride in one requests batch, and the
// protection lands on the actual per-item results and the final service list
// rather than on a mere success status.

// replacementAddr is the address each scenario instance keeps across the
// replacement, mirroring the package-level scenario.
var replacementAddr = map[string]string{
	"a": "h1:1",
	"b": "h2:2",
	"c": "h3:3",
	"d": "h4:4",
}

// replacementExcludeSuccessBatch builds the all-success batch for the
// replacement/exclusion scenario with the replacement list submitted in the
// given instance order:
//
//   - a,b,c,d registered at revision 1 and observed healthy at distinct
//     sequences 1..4;
//   - two plain selects return a then b, so the last actually rotated id is b;
//   - the list is replaced at revision 1 with a,c,d in the given order (b is
//     removed, every kept id keeps its address), bumping to revision 2;
//   - a select at revision 2 excluding c must continue just after the removed b
//     and choose d; two plain selects then wrap to a and continue to c.
func replacementExcludeSuccessBatch(t *testing.T, order []string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString(`{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},
			{"id":"c","address":"h3:3"},{"id":"d","address":"h4:4"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"health","service":"svc","instanceId":"d","expectedRevision":1,"sequence":4,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[`)
	for i, id := range order {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"id":"`)
		b.WriteString(id)
		b.WriteString(`","address":"`)
		b.WriteString(replacementAddr[id])
		b.WriteString(`"}`)
	}
	b.WriteString(`]},
		{"type":"select","service":"svc","expectedRevision":2,"excludeInstanceIds":["c"]},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"select","service":"svc","expectedRevision":2}
	]}`)
	return b.String()
}

// TestSelectExclusionAfterReplacementContinuesPosition is the main end-to-end
// guard: after removing the last chosen instance b by replacement, the
// c-excluded selection continues just after b to d (never resetting to the
// smallest id a and never choosing the excluded c), then plain selections wrap
// to a and continue to c. The returned address and health sequence must belong
// to the instance actually selected, and the revision is the post-replacement
// value 2. It also asserts the exclusion changed nothing durable (c stays
// healthy and registered) and that the replacement list's submission order
// cannot alter any of these results. A batch with no failed item exits 0.
func TestSelectExclusionAfterReplacementContinuesPosition(t *testing.T) {
	for _, order := range [][]string{
		{"a", "c", "d"},
		{"d", "a", "c"},
		{"c", "d", "a"},
	} {
		t.Run(strings.Join(order, "-"), func(t *testing.T) {
			out, code := runRegisterWith(t, replacementExcludeSuccessBatch(t, order))
			if code != 0 {
				t.Fatalf("all-success batch must exit 0, got %d, output: %s", code, out)
			}
			var got registerOutput
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, out)
			}
			if len(got.Results) != 11 {
				t.Fatalf("results: %+v", got.Results)
			}

			// Items 1-5 commit the service and four healthy observations.
			for i := 0; i < 5; i++ {
				if !got.Results[i].OK {
					t.Fatalf("setup item %d should succeed: %+v", i+1, got.Results[i])
				}
			}
			// Items 6-7: the ordinary rotation takes a then b; the cursor rests
			// on b just before the replacement.
			if r := got.Results[5]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 1 {
				t.Fatalf("item 6 should choose a, got %+v", r)
			}
			if r := got.Results[6]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 || r.Revision != 1 {
				t.Fatalf("item 7 should choose b, got %+v", r)
			}
			// Item 8: replacing at the current revision removes b, keeps a,c,d
			// with identical ids and addresses, and bumps exactly to revision 2.
			if r := got.Results[7]; !r.OK || !r.Changed || r.Revision != 2 {
				t.Fatalf("item 8 replacement should change to revision 2, got %+v", r)
			}
			// Item 9: excluding c continues just AFTER the removed b. Among
			// {a,d} the first id past b is d — selecting a here would mean the
			// position was wrongly reset, and c would mean the exclusion leaked.
			if r := got.Results[8]; !r.OK || r.InstanceID != "d" || r.Address != "h4:4" || r.Sequence != 4 || r.Revision != 2 {
				t.Fatalf("item 9 should continue after removed b to d, got %+v", r)
			}
			// Item 10: without an exclusion list the rotation wraps past d to
			// the smallest healthy id a.
			if r := got.Results[9]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 2 {
				t.Fatalf("item 10 should wrap to a, got %+v", r)
			}
			// Item 11: the rotation continues to c — the instance excluded by
			// item 9 was never removed or marked unhealthy.
			if r := got.Results[10]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 3 || r.Revision != 2 {
				t.Fatalf("item 11 should choose c, got %+v", r)
			}

			// The final list is exactly {a,c,d} at revision 2, sorted by id,
			// all healthy at the sequences accepted before the replacement; the
			// removed b is gone and the exclusion changed no record.
			if len(got.Services) != 1 || got.Services[0].Service != "svc" || got.Services[0].Revision != 2 {
				t.Fatalf("services: %+v", got.Services)
			}
			insts := got.Services[0].Instances
			if len(insts) != 3 {
				t.Fatalf("instances: %+v", insts)
			}
			want := []registerInstance{
				{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 1},
				{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 3},
				{ID: "d", Address: "h4:4", Health: "healthy", Sequence: 4},
			}
			for i, w := range want {
				if insts[i] != w {
					t.Fatalf("instance %d: got %+v want %+v", i, insts[i], w)
				}
			}
		})
	}
}

// TestSelectFailuresAfterReplacementKeepPositionAndExitStatus guards the
// position when a post-replacement selection fails. After replacing a,b,c,d
// with a,c,d (cursor on the removed b, revision 2):
//
//   - a select still carrying the pre-replacement revision 1 is a conflict
//     naming expected 1 and actual 2, reporting the current revision, with no
//     target id, address or sequence;
//   - a select at revision 2 that excludes every healthy instance (a,c,d) is
//     no_healthy whose reason says the exclusion removed them all, at the
//     current revision and with no fabricated target;
//   - the failed items keep their own per-item results and later items still
//     run; the next valid select resumes just after the last actually chosen id
//     (the removed b) at c — it neither skips c nor restarts at a — then
//     continues to d.
//
// The batch contains failed items, so the fully written result exits 1 even
// though every later item succeeds; the final list still shows a,c,d healthy
// with their previously accepted records.
func TestSelectFailuresAfterReplacementKeepPositionAndExitStatus(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"a","address":"h1:1"},{"id":"b","address":"h2:2"},
			{"id":"c","address":"h3:3"},{"id":"d","address":"h4:4"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"health","service":"svc","instanceId":"d","expectedRevision":1,"sequence":4,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[
			{"id":"d","address":"h4:4"},{"id":"a","address":"h1:1"},{"id":"c","address":"h3:3"}]},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":2,"excludeInstanceIds":["a","c","d"]},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"select","service":"svc","expectedRevision":2}
	]}`
	out, code := runRegisterWith(t, input)
	if code == 0 {
		t.Fatalf("batch contains failed items, exit code must be 1, output: %s", out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 12 {
		t.Fatalf("results: %+v", got.Results)
	}

	// Items 1-7 establish revision 2 with the cursor on the removed b.
	for i := 0; i < 5; i++ {
		if !got.Results[i].OK {
			t.Fatalf("setup item %d should succeed: %+v", i+1, got.Results[i])
		}
	}
	if r := got.Results[5]; !r.OK || r.InstanceID != "a" {
		t.Fatalf("item 6 should choose a, got %+v", r)
	}
	if r := got.Results[6]; !r.OK || r.InstanceID != "b" {
		t.Fatalf("item 7 should choose b, got %+v", r)
	}
	if r := got.Results[7]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("item 8 replacement should reach revision 2, got %+v", r)
	}

	// Item 9: stale revision 1 against a service now at revision 2 is a
	// conflict naming both revisions and reporting the current one; it returns
	// no target id, address or sequence.
	if r := got.Results[8]; r.OK || r.Error != "conflict" ||
		r.ExpectedRevision != 1 || r.ActualRevision != 2 || r.Revision != 2 {
		t.Fatalf("item 9 should conflict (expected 1, actual 2), got %+v", r)
	}
	if !strings.Contains(got.Results[8].Reason, "revision 2, not 1") {
		t.Fatalf("item 9 reason must state both revisions, got %q", got.Results[8].Reason)
	}
	if r := got.Results[8]; r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
		t.Fatalf("item 9 conflict must fabricate no target: %+v", r)
	}

	// Item 10: correct revision but every healthy instance excluded for this
	// request is no_healthy with a reason saying the exclusion removed them all,
	// at the current revision and with no fabricated target.
	if r := got.Results[9]; r.OK || r.Error != "no_healthy" || r.Revision != 2 {
		t.Fatalf("item 10 should be no_healthy at revision 2, got %+v", r)
	}
	if !strings.Contains(got.Results[9].Reason, "all healthy instances are excluded") {
		t.Fatalf("item 10 reason must say the exclusion removed every healthy instance, got %q", got.Results[9].Reason)
	}
	if r := got.Results[9]; r.InstanceID != "" || r.Address != "" || r.Sequence != 0 {
		t.Fatalf("item 10 no_healthy must fabricate no target: %+v", r)
	}

	// Item 11: the failures moved the cursor nowhere, so the rotation resumes
	// just after the last actually chosen id (the removed b) and lands on c —
	// skipping c would mean the conflict advanced the cursor, and a would mean
	// the position was reset to the start.
	if r := got.Results[10]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 3 || r.Revision != 2 {
		t.Fatalf("item 11 should resume after removed b at c, got %+v", r)
	}
	// Item 12: the rotation then continues normally to d.
	if r := got.Results[11]; !r.OK || r.InstanceID != "d" || r.Address != "h4:4" || r.Sequence != 4 || r.Revision != 2 {
		t.Fatalf("item 12 should choose d, got %+v", r)
	}

	// Neither failure removed an instance or changed a health record: the final
	// list is exactly {a,c,d} at revision 2 with the accepted sequences.
	if len(got.Services) != 1 || got.Services[0].Revision != 2 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 3 {
		t.Fatalf("instances: %+v", insts)
	}
	want := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 1},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 3},
		{ID: "d", Address: "h4:4", Health: "healthy", Sequence: 4},
	}
	for i, w := range want {
		if insts[i] != w {
			t.Fatalf("instance %d: got %+v want %+v", i, insts[i], w)
		}
	}
}
