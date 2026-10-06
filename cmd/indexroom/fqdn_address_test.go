package main

// This file is the end-to-end regression guard for fully qualified domain
// addresses ending in one root dot (e.g. "api.example.:8080"). It drives JSON
// batches through runRegister — the same offline path as the register CLI,
// with no name resolution or network access — and locks the observable
// behaviour across create, replace, health and select items:
//
//   - a trailing-dot domain is accepted on create and on list replacement and
//     its text (including the dot and the submitted case) is saved verbatim;
//   - the dotted spelling is a DISTINCT address from the undotted one:
//     replacing one with the other is a real change that bumps the revision
//     and restarts that instance at unknown/sequence 0 with no reason, so it
//     cannot be selected until a fresh healthy observation at the new
//     revision, while an instance whose address is unchanged keeps its record;
//   - once reported healthy under the current revision, select returns the
//     dotted address text and the latest accepted health sequence;
//   - "api..example.:8080", "api.example..:8080" and ".:8080" stay invalid;
//     an invalid item reports the address problem and the current revision
//     even when its expectedRevision also mismatches (invalid beats conflict),
//     carries no revision comparison fields, mutates no list or health
//     record, and later items keep processing against the committed state.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRegisterFullyQualifiedDomainAddresses(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"i1","address":"api.example:8080"},
			{"id":"i2","address":"h2:2"}
		]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i2","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":99,"instances":[
			{"id":"i1","address":"api..example.:8080"}
		]},
		{"type":"register","service":"svc","expectedRevision":99,"instances":[
			{"id":"i1","address":"api.example..:8080"}
		]},
		{"type":"register","service":"svc","expectedRevision":99,"instances":[
			{"id":"i1","address":".:8080"}
		]},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[
			{"id":"i1","address":"api.example.:8080"},
			{"id":"i2","address":"h2:2"}
		]},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":2,"sequence":10,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"register","service":"svc","expectedRevision":2,"instances":[
			{"id":"i1","address":".:8080"}
		]},
		{"type":"select","service":"svc","expectedRevision":2}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains invalid items, exit code must be 1, got %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 14 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0: create at revision 1; the undotted spelling is saved verbatim.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 create: %+v", r)
	}
	// 1-3: two healthy observations accepted without a revision bump, then the
	// rotation starts at the smallest healthy id with its exact address text.
	for i, seq := range []int64{1, 2} {
		if r := got.Results[1+i]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != seq {
			t.Fatalf("result %d health: %+v", 1+i, r)
		}
	}
	if r := got.Results[3]; !r.OK || r.InstanceID != "i1" ||
		r.Address != "api.example:8080" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 3 select: %+v", r)
	}

	// 4-6: illegal trailing-dot forms are invalid even though every one also
	// submits a mismatched expectedRevision. The address problem wins, the
	// current revision is stamped and no comparison fields appear.
	badAddrs := []string{"api..example.:8080", "api.example..:8080", ".:8080"}
	for i, bad := range badAddrs {
		r := got.Results[4+i]
		if r.OK || r.Error != "invalid" || r.Revision != 1 {
			t.Fatalf("result %d must be invalid at revision 1: %+v", 4+i, r)
		}
		if !strings.Contains(r.Reason, bad) {
			t.Fatalf("result %d reason must name the address %q: %q", 4+i, bad, r.Reason)
		}
		if r.ExpectedRevision != nil || r.ActualRevision != nil {
			t.Fatalf("result %d invalid must carry no revision pair: %+v", 4+i, r)
		}
	}

	// 7: the failed registrations moved nothing; the rotation continues after
	// the i1 chosen by result 3 and reaches i2.
	if r := got.Results[7]; !r.OK || r.InstanceID != "i2" ||
		r.Address != "h2:2" || r.Sequence != 2 || r.Revision != 1 {
		t.Fatalf("result 7 select after failed items: %+v", r)
	}

	// 8: replacing the undotted address with the dotted one is a real change:
	// revision 2, and i1 restarts unknown while unchanged i2 keeps its record.
	if r := got.Results[8]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("result 8 dotted replacement: %+v", r)
	}

	// 9: the reset i1 is not selectable yet; only the still-healthy i2 answers.
	if r := got.Results[9]; !r.OK || r.InstanceID != "i2" ||
		r.Address != "h2:2" || r.Sequence != 2 || r.Revision != 2 {
		t.Fatalf("result 9 select before fresh observation: %+v", r)
	}

	// 10: fresh healthy observation at revision 2.
	if r := got.Results[10]; !r.OK || !r.Changed || r.Revision != 2 || r.Sequence != 10 {
		t.Fatalf("result 10 health at revision 2: %+v", r)
	}

	// 11: selection now returns the dotted address text and the latest
	// accepted sequence.
	if r := got.Results[11]; !r.OK || r.InstanceID != "i1" ||
		r.Address != "api.example.:8080" || r.Sequence != 10 || r.Revision != 2 {
		t.Fatalf("result 11 select of dotted instance: %+v", r)
	}

	// 12: an invalid list at the otherwise current revision fails and changes
	// nothing; the raw item carries the offending address.
	if r := got.Results[12]; r.OK || r.Error != "invalid" || r.Revision != 2 ||
		!strings.Contains(r.Reason, ".:8080") {
		t.Fatalf("result 12 invalid list: %+v", r)
	}

	// 13: the rotation and health records survived the invalid item; it
	// continues after i1 and reaches the untouched i2.
	if r := got.Results[13]; !r.OK || r.InstanceID != "i2" ||
		r.Address != "h2:2" || r.Sequence != 2 || r.Revision != 2 {
		t.Fatalf("result 13 select after invalid list: %+v", r)
	}

	// The final service list saves the dotted spelling, the revision gained by
	// the real address change, and the health records each instance earned.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 2 || len(svc.Instances) != 2 {
		t.Fatalf("service snapshot: %+v", svc)
	}
	want := map[string]registerInstance{
		"i1": {ID: "i1", Address: "api.example.:8080", Health: "healthy", Sequence: 10},
		"i2": {ID: "i2", Address: "h2:2", Health: "healthy", Sequence: 2},
	}
	for _, inst := range svc.Instances {
		w, ok := want[inst.ID]
		if !ok {
			t.Fatalf("unexpected instance: %+v", inst)
		}
		if inst.Address != w.Address || inst.Health != w.Health || inst.Sequence != w.Sequence || inst.Reason != "" {
			t.Fatalf("instance %s: got %+v want %+v", inst.ID, inst, w)
		}
	}
}
