package main

import (
	"encoding/json"
	"testing"
)

// decodeRawResults parses the per-item results as generic JSON objects so a
// test can tell an omitted key apart from one carrying the integer 0.
func decodeRawResults(t *testing.T, out string) []map[string]json.RawMessage {
	t.Helper()
	var doc struct {
		Results []map[string]json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return doc.Results
}

// rawInt reads an integer field that must be present in the raw object.
func rawInt(t *testing.T, r map[string]json.RawMessage, field string) int {
	t.Helper()
	tok, ok := r[field]
	if !ok {
		t.Fatalf("%s must be present, got %s", field, keys(r))
	}
	var n int
	if err := json.Unmarshal(tok, &n); err != nil {
		t.Fatalf("%s must be a JSON integer, got %s", field, string(tok))
	}
	return n
}

func keys(r map[string]json.RawMessage) []string {
	out := make([]string, 0, len(r))
	for k := range r {
		out = append(out, k)
	}
	return out
}

// assertRevisionFieldsAbsent fails if either revision comparison field is
// present: success, invalid, not_found, stale, no_healthy and health's
// same-sequence content conflict must omit both.
func assertRevisionFieldsAbsent(t *testing.T, r map[string]json.RawMessage) {
	t.Helper()
	if _, ok := r["expectedRevision"]; ok {
		t.Fatalf("expectedRevision must be omitted, got %s", string(r["expectedRevision"]))
	}
	if _, ok := r["actualRevision"]; ok {
		t.Fatalf("actualRevision must be omitted, got %s", string(r["actualRevision"]))
	}
}

// TestRevisionConflictFieldsWithZero locks the core fix: a registration
// revision conflict reports BOTH revisions as explicit integers even when one
// is 0. An existing service at revision 1 receiving expectedRevision 0 must
// show expectedRevision 0 and actualRevision 1 for every request kind, so a
// consumer never has to parse the reason and can tell an omitted field from a
// real zero.
func TestRevisionConflictFieldsWithZero(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"register","service":"svc","expectedRevision":0,"instances":[]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":0,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":0},
		{"type":"release_session","service":"svc","expectedRevision":0,"sessionKey":"k"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	results := decodeRawResults(t, out)
	if len(results) != 5 {
		t.Fatalf("results: %d", len(results))
	}
	// Item 0 succeeds and must omit the comparison fields.
	assertRevisionFieldsAbsent(t, results[0])
	// Items 1-4 all submit 0 against the service now at revision 1.
	for i := 1; i < len(results); i++ {
		r := results[i]
		if got := string(r["error"]); got != `"conflict"` {
			t.Fatalf("result %d error: got %s", i, got)
		}
		if exp := rawInt(t, r, "expectedRevision"); exp != 0 {
			t.Fatalf("result %d expectedRevision: got %d want 0", i, exp)
		}
		if act := rawInt(t, r, "actualRevision"); act != 1 {
			t.Fatalf("result %d actualRevision: got %d want 1", i, act)
		}
		if rev := rawInt(t, r, "revision"); rev != 1 {
			t.Fatalf("result %d revision: got %d want 1", i, rev)
		}
	}
}

// TestRevisionConflictFieldsUnknownService locks the other zero case: a
// nonzero expectedRevision against an unknown service is a conflict (not
// not_found) showing the submitted value and actual revision 0, with revision
// still 0. It applies to register replacement, health, select and release.
func TestRevisionConflictFieldsUnknownService(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"ghost","expectedRevision":2,"instances":[]},
		{"type":"health","service":"ghost","instanceId":"i1","expectedRevision":2,"sequence":1,"healthy":true},
		{"type":"select","service":"ghost","expectedRevision":2},
		{"type":"release_session","service":"ghost","expectedRevision":2,"sessionKey":"k"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	results := decodeRawResults(t, out)
	if len(results) != 4 {
		t.Fatalf("results: %d", len(results))
	}
	for i, r := range results {
		if got := string(r["error"]); got != `"conflict"` {
			t.Fatalf("result %d error: got %s want conflict", i, got)
		}
		if exp := rawInt(t, r, "expectedRevision"); exp != 2 {
			t.Fatalf("result %d expectedRevision: got %d want 2", i, exp)
		}
		if act := rawInt(t, r, "actualRevision"); act != 0 {
			t.Fatalf("result %d actualRevision: got %d want 0", i, act)
		}
		if rev := rawInt(t, r, "revision"); rev != 0 {
			t.Fatalf("result %d revision: got %d want 0", i, rev)
		}
	}
}

// TestRevisionConflictFieldsOmission locks that the two comparison fields stay
// absent everywhere that is not a registration-revision conflict: a successful
// item, invalid, not_found (unknown service at revision 0), stale, no_healthy,
// and health's same-sequence/different-content conflict. That last conflict
// still reports the current health sequence.
func TestRevisionConflictFieldsOmission(t *testing.T) {
	input := `{"requests":[
		{"type":"health","service":"ghost","instanceId":"i1","expectedRevision":0,"sequence":1,"healthy":true},
		{"type":"select","service":"ghost","expectedRevision":0},
		{"type":"release_session","service":"ghost","expectedRevision":0,"sessionKey":"k"},
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":5,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":5,"healthy":false,"reason":"boom"},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"register","service":"blank","expectedRevision":0,"instances":[]},
		{"type":"select","service":"blank","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":null}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	results := decodeRawResults(t, out)
	if len(results) != 10 {
		t.Fatalf("results: %d", len(results))
	}
	// 0-2: unknown service at expectedRevision 0 -> not_found, no comparison.
	for i := 0; i < 3; i++ {
		if got := string(results[i]["error"]); got != `"not_found"` {
			t.Fatalf("result %d error: got %s want not_found", i, got)
		}
		assertRevisionFieldsAbsent(t, results[i])
	}
	// 3: successful create; 4: successful health.
	for _, i := range []int{3, 4} {
		if _, ok := results[i]["error"]; ok {
			t.Fatalf("result %d should succeed, got %s", i, string(results[i]["error"]))
		}
		assertRevisionFieldsAbsent(t, results[i])
	}
	// 5: same accepted sequence with different content -> conflict reporting
	// the current sequence but NOT the registration revision comparison.
	r := results[5]
	if got := string(r["error"]); got != `"conflict"` {
		t.Fatalf("result 5 error: got %s want conflict", got)
	}
	assertRevisionFieldsAbsent(t, r)
	if seq := rawInt(t, r, "sequence"); seq != 5 {
		t.Fatalf("result 5 sequence: got %d want 5", seq)
	}
	// 6: stale; 7: successful empty-service create; 8: no_healthy (service
	// exists at a matching revision but has no instance); 9: invalid. None of
	// the failures carries the comparison fields.
	if got := string(results[6]["error"]); got != `"stale"` {
		t.Fatalf("result 6 error: got %s want stale", got)
	}
	assertRevisionFieldsAbsent(t, results[6])
	if _, ok := results[7]["error"]; ok {
		t.Fatalf("result 7 should succeed, got %s", string(results[7]["error"]))
	}
	assertRevisionFieldsAbsent(t, results[7])
	if got := string(results[8]["error"]); got != `"no_healthy"` {
		t.Fatalf("result 8 error: got %s want no_healthy", got)
	}
	assertRevisionFieldsAbsent(t, results[8])
	if got := string(results[9]["error"]); got != `"invalid"` {
		t.Fatalf("result 9 error: got %s want invalid", got)
	}
	assertRevisionFieldsAbsent(t, results[9])
}

// TestRevisionConflictFieldsSeeCommittedState locks that a conflict reports
// the service's revision at the moment that item is processed: an earlier
// successful replacement in the same batch bumps the revision first.
func TestRevisionConflictFieldsSeeCommittedState(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[{"id":"i2","address":"h2:8080"}]},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	results := decodeRawResults(t, out)
	r := results[2]
	if exp := rawInt(t, r, "expectedRevision"); exp != 1 {
		t.Fatalf("expectedRevision: got %d want 1", exp)
	}
	if act := rawInt(t, r, "actualRevision"); act != 2 {
		t.Fatalf("actualRevision must reflect the post-replacement revision 2, got %d", act)
	}
}

// TestInvalidFieldBeatsRevisionConflict locks that an invalid field is still
// classified invalid and omits the comparison fields even when the revision
// also mismatches.
func TestInvalidFieldBeatsRevisionConflict(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"register","service":"svc","expectedRevision":9,"instances":null},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":9,"sequence":0,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":9,"sessionKey":5}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	results := decodeRawResults(t, out)
	for i := 1; i < len(results); i++ {
		if got := string(results[i]["error"]); got != `"invalid"` {
			t.Fatalf("result %d error: got %s want invalid", i, got)
		}
		assertRevisionFieldsAbsent(t, results[i])
	}
}
