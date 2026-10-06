package main

// End-to-end regression guard for a fully qualified domain name's single
// trailing dot ("api.example.:8080") driven through the register command's
// JSON batch. It locks:
//
//   - the dotted address is accepted both when the service is created and when
//     the instance list is replaced, and is saved verbatim (case and root dot);
//   - the dotted and non-dotted texts are different registrations: replacing one
//     with the other bumps the revision and restarts that instance at
//     unknown/0/no reason, so it cannot be selected until a fresh healthy
//     observation at the current revision, after which select returns the same
//     address text and the latest accepted sequence;
//   - api..example.:8080, api.example..:8080 and .:8080 are each invalid with a
//     reason naming the address and stamped with the current revision; the bad
//     address takes precedence even when expectedRevision also mismatches (no
//     revision pair is emitted);
//   - a failed item rewrites neither the list nor the health record, and later
//     items keep running against the last accepted state.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestRegisterFullyQualifiedDomainAddress(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[
			{"id":"i1","address":"api.example:8080"}
		]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"register","service":"svc","expectedRevision":1,"instances":[
			{"id":"i1","address":"Api.Example.:8080"}
		]},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":2,"sequence":3,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":2},
		{"type":"register","service":"svc","expectedRevision":9,"instances":[
			{"id":"i1","address":"api..example.:8080"}
		]},
		{"type":"register","service":"svc","expectedRevision":2,"instances":[
			{"id":"i1","address":"api.example..:8080"}
		]},
		{"type":"register","service":"svc","expectedRevision":9,"instances":[
			{"id":"i1","address":".:8080"}
		]},
		{"type":"register","service":"svc","expectedRevision":2,"instances":[
			{"id":"i1","address":"Api.Example.:8080"}
		]},
		{"type":"select","service":"svc","expectedRevision":2}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("the three invalid items make the batch exit 1, got %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 11 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0: service created at revision 1 with the non-dotted address.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 create: %+v", r)
	}
	// 1: healthy observation accepted without bumping the registration revision.
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 1 {
		t.Fatalf("result 1 healthy: %+v", r)
	}
	// 2: replacing the address text with the dotted form is a real change.
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 2 {
		t.Fatalf("result 2 dotted replacement must bump revision: %+v", r)
	}
	// 3: the address change reset i1 to unknown, so before a fresh observation
	// at the new revision it is not selectable.
	if r := got.Results[3]; r.OK || r.Error != "no_healthy" || r.Revision != 2 ||
		r.InstanceID != "" || r.Address != "" {
		t.Fatalf("result 3 reset instance must not be selectable: %+v", r)
	}
	// 4: a fresh healthy observation at the current revision is accepted.
	if r := got.Results[4]; !r.OK || !r.Changed || r.Revision != 2 || r.Sequence != 3 {
		t.Fatalf("result 4 fresh observation: %+v", r)
	}
	// 5: selection returns the verbatim dotted, case-preserving address and the
	// latest accepted sequence.
	if r := got.Results[5]; !r.OK || r.InstanceID != "i1" ||
		r.Address != "Api.Example.:8080" || r.Sequence != 3 || r.Revision != 2 {
		t.Fatalf("result 5 select must return the dotted address text: %+v", r)
	}

	// 6-8: each malformed FQDN is invalid; the reason names the offending
	// address, the current revision (2) is stamped, and no revision comparison
	// pair is emitted — the address problem wins even over expectedRevision 9.
	bad := []string{"api..example.:8080", "api.example..:8080", ".:8080"}
	for i, addr := range bad {
		r := got.Results[6+i]
		if r.OK || r.Error != "invalid" || r.Revision != 2 || !omitsRevisionPair(r) {
			t.Fatalf("result %d invalid: %+v", 6+i, r)
		}
		if !strings.Contains(r.Reason, addr) {
			t.Fatalf("result %d reason must name %q, got %q", 6+i, addr, r.Reason)
		}
	}

	// 9: the rejected items changed nothing; resubmitting the accepted dotted
	// list at revision 2 is identical content (unchanged), and i1's observation
	// survives.
	if r := got.Results[9]; !r.OK || r.Changed || r.Revision != 2 {
		t.Fatalf("result 9 identical dotted list must be unchanged: %+v", r)
	}
	// 10: selection still returns the verbatim dotted address with its sequence.
	if r := got.Results[10]; !r.OK || r.InstanceID != "i1" ||
		r.Address != "Api.Example.:8080" || r.Sequence != 3 || r.Revision != 2 {
		t.Fatalf("result 10 select after rejected items: %+v", r)
	}

	// The final service list stores revision 2 and the verbatim dotted address.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 2 || len(svc.Instances) != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	if want := (registerInstance{ID: "i1", Address: "Api.Example.:8080", Health: "healthy", Sequence: 3}); svc.Instances[0] != want {
		t.Fatalf("instance: got %+v want %+v", svc.Instances[0], want)
	}
}
