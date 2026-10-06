package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// A revision-mismatch conflict must carry both expectedRevision and
// actualRevision as explicit JSON integers even when one side is 0: a
// new-service request submits 0 and an unknown service sits at 0. Before the
// fix omitempty dropped the zero side, forcing callers to parse the reason
// text. This test pins the raw JSON rather than only the decoded struct so an
// omitted field can never again look like a present zero.
func TestRevisionConflictFieldsAreExplicitIntegers(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"register","service":"svc","expectedRevision":0,"instances":[]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":0,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":0},
		{"type":"release_session","service":"svc","expectedRevision":0,"sessionKey":"k"},
		{"type":"register","service":"ghost","expectedRevision":2,"instances":[]},
		{"type":"health","service":"ghost","instanceId":"z","expectedRevision":2,"sequence":1,"healthy":true},
		{"type":"select","service":"ghost","expectedRevision":2},
		{"type":"release_session","service":"ghost","expectedRevision":2,"sessionKey":"k"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains conflicts, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 9 {
		t.Fatalf("results: %+v", got.Results)
	}

	// Existing service at revision 1, request submits 0: pair (0, 1).
	for i := 1; i <= 4; i++ {
		r := got.Results[i]
		if r.OK || r.Error != "conflict" || r.Revision != 1 {
			t.Fatalf("result %d want conflict at revision 1: %+v", i, r)
		}
		if !hasRevisionPair(r, 0, 1) {
			t.Fatalf("result %d want explicit pair (0, 1): %+v", i, r)
		}
		raw := string(mustResultJSON(t, out, i))
		if !strings.Contains(raw, `"expectedRevision": 0`) || !strings.Contains(raw, `"actualRevision": 1`) {
			t.Fatalf("result %d raw JSON must show both integers 0 and 1:\n%s", i, raw)
		}
	}

	// Unknown service (revision 0), request submits 2: pair (2, 0), revision 0.
	for i := 5; i <= 8; i++ {
		r := got.Results[i]
		if r.OK || r.Error != "conflict" || r.Revision != 0 {
			t.Fatalf("result %d want conflict at revision 0: %+v", i, r)
		}
		if !hasRevisionPair(r, 2, 0) {
			t.Fatalf("result %d want explicit pair (2, 0): %+v", i, r)
		}
		raw := string(mustResultJSON(t, out, i))
		if !strings.Contains(raw, `"expectedRevision": 2`) || !strings.Contains(raw, `"actualRevision": 0`) {
			t.Fatalf("result %d raw JSON must show both integers 2 and 0:\n%s", i, raw)
		}
	}

	// A revision-gate health conflict reports no current health sequence.
	if raw := string(mustResultJSON(t, out, 6)); strings.Contains(raw, `"sequence"`) {
		t.Fatalf("revision-gate health conflict must carry no sequence:\n%s", raw)
	}

	// The unknown service was never created by its conflicting items.
	if len(got.Services) != 1 || got.Services[0].Service != "svc" || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
}

// Success, invalid (even with a simultaneously wrong revision), not_found,
// stale, no_healthy and the same-sequence/different-content health conflict all
// omit the two revision comparison fields. In particular the health content
// conflict stays conflict, reports the current sequence and a reason, and
// must not be mistaken for a registration-revision mismatch.
func TestNonRevisionMismatchOutcomesOmitRevisionPair(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"register","service":"svc","expectedRevision":9,"instances":[{"id":"","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":9,"sequence":0,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":9,"sessionKey":"   "},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"ghost","instanceId":"i1","expectedRevision":0,"sequence":1,"healthy":true},
		{"type":"select","service":"missing","expectedRevision":0},
		{"type":"release_session","service":"missing","expectedRevision":0,"sessionKey":"k"},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":false,"reason":"连接失败"},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"register","service":"empty","expectedRevision":0,"instances":[]},
		{"type":"select","service":"empty","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains failures, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 14 {
		t.Fatalf("results: %+v", got.Results)
	}

	// Every raw result must omit both revision comparison fields.
	for i, r := range got.Results {
		raw := string(mustResultJSON(t, out, i))
		if strings.Contains(raw, `"expectedRevision"`) || strings.Contains(raw, `"actualRevision"`) {
			t.Fatalf("result %d (%s) must omit the revision comparison fields:\n%s", i, r.Error, raw)
		}
	}

	// Invalid precedence: a bad field wins over the simultaneously mismatching
	// revision (results 1-3).
	for _, i := range []int{1, 2, 3} {
		if r := got.Results[i]; r.OK || r.Error != "invalid" || r.Revision != 1 {
			t.Fatalf("result %d want invalid at revision 1: %+v", i, r)
		}
	}

	// Unknown service at expectedRevision 0: non-register requests are not_found.
	for _, i := range []int{5, 6, 7} {
		if r := got.Results[i]; r.OK || r.Error != "not_found" || r.Revision != 0 {
			t.Fatalf("result %d want not_found at revision 0: %+v", i, r)
		}
	}

	// Same accepted sequence reused with different health content: conflict,
	// reports the current sequence and a reason, but NO revision pair.
	if r := got.Results[9]; r.OK || r.Error != "conflict" || r.Sequence != 1 || r.Reason == "" {
		t.Fatalf("result 9 want a health-content conflict at sequence 1: %+v", r)
	}
	if raw := string(mustResultJSON(t, out, 9)); !strings.Contains(raw, `"sequence": 1`) {
		t.Fatalf("result 9 must report the current sequence 1:\n%s", raw)
	}

	// A smaller sequence is stale and reports the current sequence.
	if r := got.Results[11]; r.OK || r.Error != "stale" || r.Sequence != 2 || r.Revision != 1 {
		t.Fatalf("result 11 want stale reporting sequence 2: %+v", r)
	}

	// A register request at expectedRevision 0 still creates the unknown
	// service (result 12), so its matching-revision select reaches no_healthy
	// rather than not_found.
	if r := got.Results[12]; !r.OK || r.Revision != 1 {
		t.Fatalf("result 12 register should create the service: %+v", r)
	}
	if r := got.Results[13]; r.OK || r.Error != "no_healthy" || r.Revision != 1 {
		t.Fatalf("result 13 want no_healthy on the created empty service: %+v", r)
	}
}

// Each conflict reports the revision current when that item is handled: a
// successful replacement earlier in the batch bumps it, and a later conflict
// must use the new value rather than the batch's starting or ending revision.
// Conflicts also mutate nothing — instance list, health records, session
// bindings and the rotation cursor — later items keep running in order, and
// the batch still exits 1.
func TestRevisionConflictsUsePerItemRevisionAndMutateNothing(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i2","address":"h2:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i2","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":2,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i2","address":"h2:8080"}]},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i2","address":"h2:8080"},{"id":"i3","address":"h3:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":9,"healthy":true},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"select","service":"svc","expectedRevision":2,"sessionKey":"k"},
		{"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"k"},
		{"type":"select","service":"svc","expectedRevision":2,"sessionKey":"k"},
		{"type":"select","service":"svc","expectedRevision":2}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains conflicts, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 14 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 3: first rotation selects i1 and rests the cursor there.
	if r := got.Results[3]; !r.OK || r.InstanceID != "i1" {
		t.Fatalf("result 3 first select: %+v", r)
	}
	// 4: revision mismatch while still at 1 reports pair (2, 1) and changes nothing.
	if r := got.Results[4]; r.OK || !hasRevisionPair(r, 2, 1) || r.Revision != 1 {
		t.Fatalf("result 4 conflict before bump: %+v", r)
	}
	// 5: real replacement adds i3, bumping to revision 2; i1/i2 keep health.
	if r := got.Results[5]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("result 5 replacement: %+v", r)
	}
	// 6: health under the stale revision conflicts with pair (1, 2) and leaves
	// i1's accepted sequence at 1.
	if r := got.Results[6]; r.OK || r.Error != "conflict" || !hasRevisionPair(r, 1, 2) {
		t.Fatalf("result 6 stale-revision health: %+v", r)
	}
	// 7: register under the stale revision reports the NEW revision 2.
	if r := got.Results[7]; r.OK || r.Error != "conflict" || !hasRevisionPair(r, 1, 2) || r.Revision != 2 {
		t.Fatalf("result 7 conflict must use per-item revision 2: %+v", r)
	}
	// 8: a stale-revision select likewise reports (1, 2) and must not rotate.
	if r := got.Results[8]; r.OK || r.Error != "conflict" || !hasRevisionPair(r, 1, 2) {
		t.Fatalf("result 8 stale-revision select: %+v", r)
	}
	// 9: the failed select did not move the cursor from i1, so the next real
	// rotation lands on i2 with its accepted health sequence.
	if r := got.Results[9]; !r.OK || r.InstanceID != "i2" || r.Sequence != 1 {
		t.Fatalf("result 9 cursor must still be just after i1: %+v", r)
	}
	// 10: the key's first selection rotates too — past i2, wrapping to i1
	// because the newly added i3 is still unknown — and binds k -> i1.
	if r := got.Results[10]; !r.OK || r.InstanceID != "i1" {
		t.Fatalf("result 10 first keyed selection wraps to i1: %+v", r)
	}
	// 11: releasing under a stale revision conflicts and preserves the binding.
	if r := got.Results[11]; r.OK || r.Error != "conflict" || !hasRevisionPair(r, 1, 2) {
		t.Fatalf("result 11 stale-revision release: %+v", r)
	}
	// 12: the key still reuses i1 — the failed release unbound nothing and a
	// reuse does not rotate.
	if r := got.Results[12]; !r.OK || r.InstanceID != "i1" {
		t.Fatalf("result 12 binding must survive the failed release: %+v", r)
	}
	// 13: a plain select continues just after the cursor left at i1 and gets i2.
	if r := got.Results[13]; !r.OK || r.InstanceID != "i2" {
		t.Fatalf("result 13 plain rotation must continue at i2: %+v", r)
	}

	// Final state reflects only accepted items: revision 2, all three
	// instances present, i1/i2 still healthy at sequence 1 (the failed seq 9
	// report never applied), i3 unknown at sequence 0.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 2 || len(svc.Instances) != 3 {
		t.Fatalf("final service: %+v", svc)
	}
	want := []struct {
		id, health string
		sequence   int64
	}{
		{"i1", "healthy", 1},
		{"i2", "healthy", 1},
		{"i3", "unknown", 0},
	}
	for i, w := range want {
		inst := svc.Instances[i]
		if inst.ID != w.id || inst.Health != w.health || inst.Sequence != w.sequence {
			t.Fatalf("instance %d: got %+v, want id %s health %s seq %d", i, inst, w.id, w.health, w.sequence)
		}
	}
}
