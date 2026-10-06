package main

// This file is the end-to-end regression guard for resubmitting the complete
// instance list of an already live service through the register command. It
// drives one JSON batch through runRegister (the same path as the CLI) and
// locks the whole behaviour including raw output fields and the exit status:
//
//   - after registration, mixed health observations, a bound sessionKey and an
//     advanced rotation, resubmitting the same normalized list — reordered and
//     padded with whitespace on the service name, ids and addresses — succeeds
//     with no "changed" field and the same revision; it is not a recreation;
//   - the session (trimmed key) keeps returning its bound instance's current
//     address and latest accepted sequence; the reuse does not advance the
//     rotation, while plain selects continue after the last real rotation with
//     end wrap, regardless of the duplicate list's submission order;
//   - health states, sequences and reasons are retained (unknown/unhealthy
//     instances never become selectable, no fresh report is required);
//   - identical content does not bypass the revision gate: a valid list with a
//     mismatched expectedRevision is a conflict with the reason and the current
//     revision, clearing neither the session nor the cursor, and later
//     correct-revision duplicates, observations and selections proceed against
//     only the previously accepted state;
//   - the batch's failure makes the exit status 1 while the final service list
//     keeps its sorting and field meanings.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRegisterResubmittedIdenticalListKeepsSessionRotationAndRevision(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"i1","address":"h1:1"},
			{"id":"i2","address":"h2:2"},
			{"id":"i3","address":"h3:3"},
			{"id":"i4","address":"h4:4"}
		]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i2","expectedRevision":1,"sequence":22,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i3","expectedRevision":1,"sequence":33,"healthy":false,"reason":" 磁盘故障 "},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"  s  "},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"register","service":"  svc  ","expectedRevision":1,"instances":[
			{"id":" i3 ","address":" h3:3 "},
			{"id":"i1","address":"\t h1:1 \t"},
			{"id":"  i4  ","address":"  h4:4  "},
			{"id":" i2 ","address":" h2:2 "}
		]},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"   s   "},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":2,"instances":[
			{"id":"i2","address":"h2:2"},
			{"id":"i4","address":"h4:4"},
			{"id":"i1","address":"h1:1"},
			{"id":"i3","address":"h3:3"}
		]},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[
			{"id":"i1","address":"h1:1"},
			{"id":"i2","address":"h2:2"},
			{"id":"i3","address":"h3:3"},
			{"id":"i4","address":"h4:4"}
		]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":12,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"  s  "}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("the conflict item makes the batch exit 1, got %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 16 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0: create at revision 1; 1-3: the offline observations are accepted
	// without bumping the registration revision.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 create: %+v", r)
	}
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 11 {
		t.Fatalf("result 1 healthy i1: %+v", r)
	}
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 22 {
		t.Fatalf("result 2 healthy i2: %+v", r)
	}
	if r := got.Results[3]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 33 {
		t.Fatalf("result 3 unhealthy i3: %+v", r)
	}

	// 4: the padded key's first success rotates to the smallest healthy id (i3
	// is unhealthy and i4 unknown, so neither is eligible) and binds s -> i1.
	if r := got.Results[4]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:1" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("result 4 first session select: %+v", r)
	}
	// 5: a plain select advances the rotation to i2.
	if r := got.Results[5]; !r.OK || r.InstanceID != "i2" || r.Address != "h2:2" || r.Sequence != 22 || r.Revision != 1 {
		t.Fatalf("result 5 plain select: %+v", r)
	}
	// 6: session reuse returns i1 without moving the cursor.
	if r := got.Results[6]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:1" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("result 6 session reuse: %+v", r)
	}

	// 7: resubmitting the complete list in another order with padding on every
	// field trims back to the identical content: success with no "changed" and
	// no revision bump — not a recreation of the service.
	if r := got.Results[7]; !r.OK || r.Changed || r.Revision != 1 {
		t.Fatalf("result 7 identical resubmission must be unchanged: %+v", r)
	}
	if strings.Contains(string(mustResultJSON(t, out, 7)), `"changed"`) {
		t.Fatalf("result 7 JSON must not contain a changed field: %s", mustResultJSON(t, out, 7))
	}

	// 8: the padded key trims to the SAME session: its binding survived the
	// resubmission and returns i1's current address and accepted sequence.
	if r := got.Results[8]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:1" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("result 8 session survives resubmission: %+v", r)
	}
	// 9: the reuse did not move the cursor off i2; unhealthy i3 and unknown i4
	// are not candidates, so the plain rotation wraps past the end to i1.
	if r := got.Results[9]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:1" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("result 9 plain select wraps to i1 regardless of resubmission order: %+v", r)
	}

	// 10: identical content does not bypass the revision gate. The list is
	// valid (reordered again) but expectedRevision 2 mismatches the current 1:
	// conflict, with the reason and both revisions, and no false success.
	if r := got.Results[10]; r.OK || r.Error != "conflict" ||
		r.Reason != `service "svc" is at revision 1, not 2` ||
		r.Revision != 1 || !hasRevisionPair(r, 2, 1) {
		t.Fatalf("result 10 identical content with wrong revision must conflict: %+v", r)
	}

	// 11: the conflict cleared neither the session binding nor the cursor.
	if r := got.Results[11]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:1" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("result 11 session must survive the conflict: %+v", r)
	}
	// 12: a plain select continues just after the last real rotation (i1 from
	// result 9) to i2 instead of restarting at the smallest id.
	if r := got.Results[12]; !r.OK || r.InstanceID != "i2" || r.Address != "h2:2" || r.Sequence != 22 || r.Revision != 1 {
		t.Fatalf("result 12 plain select continues after i1: %+v", r)
	}

	// 13: a later duplicate at the correct revision still succeeds unchanged.
	if r := got.Results[13]; !r.OK || r.Changed || r.Revision != 1 {
		t.Fatalf("result 13 correct-revision duplicate must be unchanged: %+v", r)
	}
	// 14: later items in the batch keep working against only the accepted
	// state: a newer observation for i1 is accepted without a revision bump.
	if r := got.Results[14]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 12 {
		t.Fatalf("result 14 later observation: %+v", r)
	}
	// 15: the existing session reflects i1's latest accepted sequence through
	// its binding — the conflict and resubmissions never replaced it.
	if r := got.Results[15]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:1" || r.Sequence != 12 || r.Revision != 1 {
		t.Fatalf("result 15 session reflects the latest sequence: %+v", r)
	}

	// The final list keeps the existing sorting (by service then by id) and
	// field meanings: revision stays 1; i1 healthy at the new sequence; i2
	// untouched; i3 unhealthy with its trimmed reason; i4 still unknown/0.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	wantInsts := []registerInstance{
		{ID: "i1", Address: "h1:1", Health: "healthy", Sequence: 12},
		{ID: "i2", Address: "h2:2", Health: "healthy", Sequence: 22},
		{ID: "i3", Address: "h3:3", Health: "unhealthy", Sequence: 33, Reason: "磁盘故障"},
		{ID: "i4", Address: "h4:4", Health: "unknown", Sequence: 0},
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

// mustResultJSON re-marshals the indexed result so individual raw JSON fields
// (such as changed's omitempty) can be asserted.
func mustResultJSON(t *testing.T, out string, index int) []byte {
	t.Helper()
	var doc struct {
		Results []json.RawMessage `json:"results"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("re-parse output: %v\n%s", err, out)
	}
	if index >= len(doc.Results) {
		t.Fatalf("result index %d out of range (%d)", index, len(doc.Results))
	}
	return doc.Results[index]
}
