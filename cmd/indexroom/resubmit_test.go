package main

import (
	"bytes"
	"encoding/json"
	"testing"
)

// This file is the end-to-end regression guard for resubmitting a service's
// complete instance list after sessions already exist in the same register
// batch. It exercises only the existing JSON input/output and error
// classification of the register command: no new request kind, session entry
// point or product rule is introduced.
//
// The protected behavior:
//
//   - posting the complete list again — reordered, or with surrounding
//     whitespace on the service name, instance ids and addresses — trims back
//     to the identical content, so the item succeeds WITHOUT a "changed"
//     field, the revision is unchanged and the service is not recreated;
//   - an established session keeps returning its bound healthy instance (with
//     its current address and latest accepted health sequence) through every
//     padded variant of the key; the reuse never advances the rotation;
//   - keyless selections continue just after the instance last chosen by a
//     real rotation, wrapping by ascending id, regardless of the resubmitted
//     list's order; unknown and unhealthy instances never become candidates and
//     their accepted state/sequence/reason survive the resubmission;
//   - an identical list carrying a legal but non-current expectedRevision is
//     still a conflict stating the reason and the current revision; it neither
//     reports success nor clears the session, and later duplicates carrying the
//     correct revision (and duplicate health reports) proceed unchanged.

// assertNoChangedKey serializes one result and asserts the "changed" property
// is absent: a duplicate register/health item must omit the field entirely,
// which a bool comparison alone cannot distinguish.
func assertNoChangedKey(t *testing.T, result registerResult) {
	t.Helper()
	raw, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	if bytes.Contains(raw, []byte(`"changed"`)) {
		t.Fatalf("duplicate item must omit changed from its JSON, got %s", raw)
	}
}

// TestRegisterResubmitIdenticalListKeepsSessionRotationAndHealth walks the full
// happy path: register, health reports and a session-keyed selection establish
// the state, then the identical complete list is posted twice (reversed, and
// with whitespace padding) while selections interleave. Every item succeeds,
// so the batch exits 0; duplicate register/health results omit "changed"; the
// revision stays 1; the session survives padded key variants and reflects the
// latest accepted sequence; the keyless rotation resumes from the last real
// rotation; and the final service list keeps the original health records,
// including an unhealthy instance with its trimmed reason and an instance that
// was never reported (unknown/0).
func TestRegisterResubmitIdenticalListKeepsSessionRotationAndHealth(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"d","address":"h4:4"},
			{"id":"c","address":"h3:3"},
			{"id":" b ","address":" h2:2 "},
			{"id":"a","address":"h1:1"}
		]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":10,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":20,"healthy":false,"reason":" 磁盘故障 "},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":30,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"  user-7  "},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"  svc  ","expectedRevision":1,"instances":[
			{"id":"  d  ","address":"  h4:4  "},
			{"id":" c ","address":"\t h3:3 \n"},
			{"id":" b ","address":" h2:2 "},
			{"id":"a","address":"h1:1"}
		]},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-7"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[
			{"id":"c","address":"h3:3"},
			{"id":"a","address":" h1:1 "},
			{"id":"d","address":"h4:4"},
			{"id":"b","address":"h2:2"}
		]},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"\tuser-7\n"},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":11,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-7"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"  user-7  "}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("every item succeeds, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 17 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 0: service created once at revision 1 (reversed/padded submission order is
	// irrelevant; instances are matched by trimmed id).
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 create: %+v", r)
	}
	// 1-3: observations accepted without a registration revision bump; b is
	// unhealthy with a trimmed reason, d stays unknown.
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 10 {
		t.Fatalf("result 1: %+v", r)
	}
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 20 {
		t.Fatalf("result 2: %+v", r)
	}
	if r := got.Results[3]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 30 {
		t.Fatalf("result 3: %+v", r)
	}
	// 4: the padded key's first success rotates to the smallest HEALTHY id a,
	// skipping unhealthy b and never-reported d, and binds user-7 -> a.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("result 4 first session select: %+v", r)
	}
	// 5: a keyless selection continues just after a; b is unhealthy, so c is
	// chosen — the last real rotation before the resubmissions.
	if r := got.Results[5]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 30 || r.Revision != 1 {
		t.Fatalf("result 5 keyless rotation: %+v", r)
	}
	// 6: the reversed, padded resubmission trims to the identical list: success,
	// no revision bump, and crucially no "changed" key in the emitted JSON — it
	// must not look like a second creation.
	if r := got.Results[6]; !r.OK || r.Changed || r.Revision != 1 {
		t.Fatalf("result 6 identical resubmission must be unchanged: %+v", r)
	}
	assertNoChangedKey(t, got.Results[6])
	// 7: the unpadded key is the same session; the binding survived the
	// resubmission and the reuse reports a's own address and sequence without
	// rotating.
	if r := got.Results[7]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("result 7 session reuse after resubmission: %+v", r)
	}
	// 8-9: the reuse moved no cursor: keyless rotation continues after c, wraps
	// to a, then advances to c again — never driven by the resubmitted list's
	// (d-first) order, and never selecting b or d.
	if r := got.Results[8]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("result 8 keyless wrap to a: %+v", r)
	}
	if r := got.Results[9]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 30 || r.Revision != 1 {
		t.Fatalf("result 9 keyless rotation to c: %+v", r)
	}
	// 10: a second resubmission in a different order is unchanged again.
	if r := got.Results[10]; !r.OK || r.Changed || r.Revision != 1 {
		t.Fatalf("result 10 second identical resubmission: %+v", r)
	}
	assertNoChangedKey(t, got.Results[10])
	// 11: a differently padded key variant still names the original session.
	if r := got.Results[11]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("result 11 padded key reuse: %+v", r)
	}
	// 12: a newer accepted observation on the bound instance is a real change.
	if r := got.Results[12]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 11 {
		t.Fatalf("result 12 newer observation: %+v", r)
	}
	// 13: the reuse reports a's LATEST accepted sequence (11), not the one
	// frozen at bind time (10).
	if r := got.Results[13]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("result 13 reuse must carry latest sequence: %+v", r)
	}
	// 14-15: keyless rotation still resumes after c (wraps to a), then c,
	// carrying a's latest sequence 11; b and d stay ineligible.
	if r := got.Results[14]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("result 14 keyless wrap: %+v", r)
	}
	if r := got.Results[15]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 30 || r.Revision != 1 {
		t.Fatalf("result 15 keyless rotation: %+v", r)
	}
	// 16: plain rotation rewrote no binding.
	if r := got.Results[16]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 11 || r.Revision != 1 {
		t.Fatalf("result 16 final session reuse: %+v", r)
	}

	// Final snapshot: revision still 1; ids sorted; a healthy at its latest
	// sequence; b unhealthy with the trimmed reason; c healthy; d unknown at 0.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	if len(svc.Instances) != 4 {
		t.Fatalf("instances: %+v", svc.Instances)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 11},
		{ID: "b", Address: "h2:2", Health: "unhealthy", Sequence: 20, Reason: "磁盘故障"},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 30},
		{ID: "d", Address: "h4:4", Health: "unknown", Sequence: 0},
	}
	for i, want := range wantInsts {
		if svc.Instances[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, svc.Instances[i], want)
		}
	}
}

// TestRegisterResubmitWrongRevisionConflictsButSessionAndStateSurvive locks the
// revision gate on a content-identical resubmission: a legal
// expectedRevision that differs from the current one is a conflict with a
// reason and the current revision, never a false success and never a recreation
// that wipes the session. The rejected item keeps its slot while the batch
// continues: a padded session key still reuses the bound instance, the same
// list at the correct revision then succeeds without "changed" (as does a
// duplicate health report), and a keyless selection resumes from the rotation
// position the conflict left intact. Because the batch contains the conflict,
// the process exits 1 even though the JSON result is complete.
func TestRegisterResubmitWrongRevisionConflictsButSessionAndStateSurvive(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"a","address":"h1:1"},
			{"id":"b","address":"h2:2"},
			{"id":"c","address":"h3:3"}
		]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"  svc  ","expectedRevision":7,"instances":[
			{"id":" c ","address":" h3:3 "},
			{"id":" b ","address":" h2:2 "},
			{"id":"a","address":"h1:1"}
		]},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"  s  "},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[
			{"id":"b","address":"h2:2"},
			{"id":"a","address":" h1:1 "},
			{"id":"c","address":"h3:3"}
		]},
		{"type":"health","service":"svc","instanceId":" a ","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("the conflict item makes the batch fail, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 13 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 0-3: create at revision 1 and three observations accepted.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 create: %+v", r)
	}
	for i, seq := range []int64{1, 2, 3} {
		if r := got.Results[i+1]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != seq {
			t.Fatalf("result %d observation: %+v", i+1, r)
		}
	}
	// 4: first session selection rotates to a and binds s -> a.
	if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 4 session bind: %+v", r)
	}
	// 5: keyless rotation moves to b, the last real rotation before the
	// rejected resubmission.
	if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:2" || r.Sequence != 2 || r.Revision != 1 {
		t.Fatalf("result 5 keyless rotation: %+v", r)
	}
	// 6: the list is content-identical once trimmed, but expectedRevision 7
	// does not match the current 1: conflict with the reason and both
	// revisions, and no fabricated change. The identical content does not
	// bypass the revision check.
	if r := got.Results[6]; r.OK || r.Error != "conflict" || r.Changed ||
		r.Revision != 1 || r.ExpectedRevision != 7 || r.ActualRevision != 1 || r.Reason == "" {
		t.Fatalf("result 6 wrong-revision duplicate must conflict: %+v", r)
	}
	if !bytes.Contains([]byte(got.Results[6].Reason), []byte("revision 1, not 7")) {
		t.Fatalf("result 6 reason must state the current and requested revisions: %q", got.Results[6].Reason)
	}
	// 7: the conflict cleared no session — the padded key still reuses a — and
	// the reuse advanced no cursor.
	if r := got.Results[7]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 7 session must survive the conflict: %+v", r)
	}
	// 8: the same list at the correct current revision succeeds unchanged.
	if r := got.Results[8]; !r.OK || r.Changed || r.Revision != 1 {
		t.Fatalf("result 8 correct-revision duplicate must be unchanged: %+v", r)
	}
	assertNoChangedKey(t, got.Results[8])
	// 9: a duplicate health observation (same sequence/content) also succeeds
	// without a change; later items see only the state accepted earlier.
	if r := got.Results[9]; !r.OK || r.Changed || r.Revision != 1 || r.Sequence != 1 {
		t.Fatalf("result 9 duplicate health must be unchanged: %+v", r)
	}
	assertNoChangedKey(t, got.Results[9])
	// 10: the conflict moved no cursor: keyless rotation continues just after
	// the pre-conflict position b and lands on c; a reset would have returned a.
	if r := got.Results[10]; !r.OK || r.InstanceID != "c" || r.Address != "h3:3" || r.Sequence != 3 || r.Revision != 1 {
		t.Fatalf("result 10 keyless rotation must resume at c: %+v", r)
	}
	// 11: past c the rotation wraps to a.
	if r := got.Results[11]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 11 keyless wrap: %+v", r)
	}
	// 12: the session binding is still the original one.
	if r := got.Results[12]; !r.OK || r.InstanceID != "a" || r.Address != "h1:1" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 12 final session reuse: %+v", r)
	}

	// The conflict and the duplicates left the service at revision 1 with only
	// the originally accepted records, sorted by id.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	if len(svc.Instances) != 3 {
		t.Fatalf("instances: %+v", svc.Instances)
	}
	wantInsts := []registerInstance{
		{ID: "a", Address: "h1:1", Health: "healthy", Sequence: 1},
		{ID: "b", Address: "h2:2", Health: "healthy", Sequence: 2},
		{ID: "c", Address: "h3:3", Health: "healthy", Sequence: 3},
	}
	for i, want := range wantInsts {
		if svc.Instances[i] != want {
			t.Fatalf("instance %d: got %+v want %+v", i, svc.Instances[i], want)
		}
	}
}
