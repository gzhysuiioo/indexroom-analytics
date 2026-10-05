package main

// This file is the end-to-end regression guard for health-record isolation
// between services that reuse the same instance identifier. An instance id is
// only meaningful inside its own service: two services may both register "i1"
// — even at the very same address — and each must keep its own health state,
// sequence and reason. One JSON batch drives the register command (the same
// path as the CLI) with interleaved requests for two such services and locks
// the whole behaviour including raw output fields and the exit status:
//
//   - interleaved observations compare only against the owning service's
//     accepted record: svc-a's i1 at sequence 20 never makes svc-b's i1
//     sequence 1 stale, and svc-a's reason never leaks into svc-b's record;
//   - a stale report and a same-sequence conflicting report to one service
//     fail against that service's own current sequence, overwrite neither
//     service's record, and the next valid observation in the batch still
//     takes effect at its own sequence;
//   - selection reflects each service's own accepted health: the
//     all-unhealthy service answers no_healthy while the other selects its
//     own healthy instance with its own address and latest sequence, and a
//     recovery in one service never changes the other's state;
//   - health reports never bump either service's registration revision;
//   - the batch contains failures, so the exit status is 1, while every
//     later success and the final service list are still emitted.

import (
	"encoding/json"
	"testing"
)

func TestHealthSameInstanceIDIsolatedAcrossServices(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc-a","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"register","service":"svc-b","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc-a","instanceId":"i1","expectedRevision":1,"sequence":20,"healthy":false,"reason":" 心跳超时 "},
		{"type":"health","service":"svc-b","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc-a","expectedRevision":1},
		{"type":"select","service":"svc-b","expectedRevision":1},
		{"type":"health","service":"svc-a","instanceId":"i1","expectedRevision":1,"sequence":5,"healthy":true},
		{"type":"health","service":"svc-a","instanceId":"i1","expectedRevision":1,"sequence":20,"healthy":false,"reason":"别的理由"},
		{"type":"select","service":"svc-a","expectedRevision":1},
		{"type":"health","service":"svc-b","instanceId":"i1","expectedRevision":1,"sequence":2,"healthy":false,"reason":" 磁盘故障 "},
		{"type":"select","service":"svc-b","expectedRevision":1},
		{"type":"health","service":"svc-a","instanceId":"i1","expectedRevision":1,"sequence":21,"healthy":true},
		{"type":"select","service":"svc-a","expectedRevision":1},
		{"type":"select","service":"svc-b","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains failures, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	// One result per request, in input order; failed items keep their slots.
	if len(got.Results) != 14 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0-1: both services register an instance "i1" at the SAME address; each
	// starts at revision 1 with its own unknown/0 record.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 create svc-a: %+v", r)
	}
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 1 create svc-b: %+v", r)
	}

	// 2: svc-a's i1 accepts the unhealthy observation at sequence 20; the
	// registration revision stays 1.
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 20 {
		t.Fatalf("result 2 svc-a unhealthy at 20: %+v", r)
	}
	// 3: svc-b's i1 still accepts sequence 1 — svc-a's higher sequence 20 must
	// not make it stale, and svc-a's reason must not leak into svc-b's record.
	if r := got.Results[3]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 1 {
		t.Fatalf("result 3 svc-b healthy at its own sequence 1: %+v", r)
	}

	// 4: svc-a has no healthy instance of its own: no_healthy names the
	// current revision and fabricates neither an instance id nor an address;
	// svc-b's healthy i1 does not count.
	if r := got.Results[4]; r.OK || r.Error != "no_healthy" || r.Reason == "" ||
		r.Revision != 1 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("result 4 svc-a no_healthy: %+v", r)
	}
	// 5: svc-b selects its own healthy i1 with its own address and its own
	// latest accepted sequence 1 — not svc-a's 20.
	if r := got.Results[5]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 5 svc-b selects its own i1: %+v", r)
	}

	// 6: a stale report to svc-a is judged against svc-a's own current
	// sequence 20 — not svc-b's 1 — and reports that sequence.
	if r := got.Results[6]; r.OK || r.Error != "stale" ||
		r.Reason != "sequence 5 is older than the current sequence 20" ||
		r.Sequence != 20 || r.Revision != 1 {
		t.Fatalf("result 6 stale against svc-a's own record: %+v", r)
	}
	// 7: the same sequence 20 with different health content conflicts against
	// svc-a's record; svc-b's identical-id record is not consulted.
	if r := got.Results[7]; r.OK || r.Error != "conflict" ||
		r.Reason != "sequence 20 already used with different health content" ||
		r.Sequence != 20 || r.Revision != 1 {
		t.Fatalf("result 7 conflict against svc-a's own record: %+v", r)
	}
	// 8: neither rejection overwrote svc-a's record — the rejected healthy
	// sequence-5 report did not heal svc-a's i1.
	if r := got.Results[8]; r.OK || r.Error != "no_healthy" ||
		r.Revision != 1 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("result 8 svc-a still no_healthy after rejections: %+v", r)
	}

	// 9: immediately after svc-a's failures, svc-b's next observation takes
	// effect at its OWN next sequence 2 — svc-a's accepted 20 is irrelevant.
	if r := got.Results[9]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 2 {
		t.Fatalf("result 9 svc-b unhealthy at its own sequence 2: %+v", r)
	}
	// 10: svc-b's own record now drives its eligibility: no_healthy, and
	// svc-a's identical-id record does not leak in.
	if r := got.Results[10]; r.OK || r.Error != "no_healthy" ||
		r.Revision != 1 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("result 10 svc-b no_healthy on its own record: %+v", r)
	}

	// 11: svc-a recovers at its own next sequence 21 while svc-b's record
	// stays where svc-b left it; the revision stays 1.
	if r := got.Results[11]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 21 {
		t.Fatalf("result 11 svc-a recovers at 21: %+v", r)
	}
	// 12: svc-a is selectable again with its own address and latest sequence.
	if r := got.Results[12]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" || r.Sequence != 21 || r.Revision != 1 {
		t.Fatalf("result 12 svc-a selectable after recovery: %+v", r)
	}
	// 13: svc-a's recovery does not affect svc-b: still no_healthy on svc-b's
	// own unhealthy record.
	if r := got.Results[13]; r.OK || r.Error != "no_healthy" ||
		r.Revision != 1 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("result 13 svc-b unaffected by svc-a's recovery: %+v", r)
	}

	// The final service list shows two fully independent records under the
	// same id and the same address: svc-a's i1 healthy at sequence 21 with its
	// reason cleared by the recovery; svc-b's i1 unhealthy at sequence 2 with
	// its own trimmed reason — svc-a's 心跳超时 never leaked in. Neither health
	// report bumped a registration revision.
	if len(got.Services) != 2 {
		t.Fatalf("services: %+v", got.Services)
	}
	if got.Services[0].Service != "svc-a" || got.Services[0].Revision != 1 {
		t.Fatalf("svc-a view: %+v", got.Services[0])
	}
	if got.Services[1].Service != "svc-b" || got.Services[1].Revision != 1 {
		t.Fatalf("svc-b view: %+v", got.Services[1])
	}
	wantA := []registerInstance{{ID: "i1", Address: "h1:8080", Health: "healthy", Sequence: 21}}
	if len(got.Services[0].Instances) != 1 || got.Services[0].Instances[0] != wantA[0] {
		t.Fatalf("svc-a instances: got %+v want %+v", got.Services[0].Instances, wantA)
	}
	wantB := []registerInstance{{ID: "i1", Address: "h1:8080", Health: "unhealthy", Sequence: 2, Reason: "磁盘故障"}}
	if len(got.Services[1].Instances) != 1 || got.Services[1].Instances[0] != wantB[0] {
		t.Fatalf("svc-b instances: got %+v want %+v", got.Services[1].Instances, wantB)
	}
}

// TestHealthSameInstanceIDFailureIsolationAcrossServices isolates the failure
// paths: a stale report and a same-sequence conflicting report aimed at one
// service must overwrite neither that service's record nor the other
// service's identical-id record, and the valid observation immediately
// following the failures still lands at its own sequence. The batch's
// failures make the exit status 1 while later successes and the final list
// are still emitted.
func TestHealthSameInstanceIDFailureIsolationAcrossServices(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc-a","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"register","service":"svc-b","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc-a","instanceId":"i1","expectedRevision":1,"sequence":10,"healthy":true},
		{"type":"health","service":"svc-b","instanceId":"i1","expectedRevision":1,"sequence":3,"healthy":true},
		{"type":"health","service":"svc-a","instanceId":"i1","expectedRevision":1,"sequence":4,"healthy":false,"reason":"误报重发"},
		{"type":"health","service":"svc-a","instanceId":"i1","expectedRevision":1,"sequence":10,"healthy":false,"reason":"误报重发"},
		{"type":"health","service":"svc-b","instanceId":"i1","expectedRevision":1,"sequence":4,"healthy":true},
		{"type":"select","service":"svc-a","expectedRevision":1},
		{"type":"select","service":"svc-b","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains failures, exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 9 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 2-3: each service's i1 accepts its own healthy observation at its own
	// sequence; neither report bumps a registration revision.
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 10 {
		t.Fatalf("result 2 svc-a healthy at 10: %+v", r)
	}
	if r := got.Results[3]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 3 {
		t.Fatalf("result 3 svc-b healthy at its own sequence 3: %+v", r)
	}

	// 4: stale against svc-a's own current sequence 10 — svc-b's 3 is not
	// consulted — and the failure reports svc-a's sequence.
	if r := got.Results[4]; r.OK || r.Error != "stale" ||
		r.Reason != "sequence 4 is older than the current sequence 10" ||
		r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("result 4 stale against svc-a's own record: %+v", r)
	}
	// 5: the same sequence 10 flipping svc-a's i1 to unhealthy conflicts.
	if r := got.Results[5]; r.OK || r.Error != "conflict" ||
		r.Reason != "sequence 10 already used with different health content" ||
		r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("result 5 conflict against svc-a's own record: %+v", r)
	}

	// 6: the valid observation immediately after the failures belongs to
	// svc-b and lands at svc-b's own next sequence 4 — svc-a's accepted 10
	// and its rejected reports play no part.
	if r := got.Results[6]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 4 {
		t.Fatalf("result 6 svc-b healthy at its own sequence 4: %+v", r)
	}

	// 7-8: both services still select their own healthy i1 with its own
	// latest accepted sequence — the rejected "unhealthy" reports knocked
	// neither record out.
	if r := got.Results[7]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" || r.Sequence != 10 || r.Revision != 1 {
		t.Fatalf("result 7 svc-a keeps its accepted record: %+v", r)
	}
	if r := got.Results[8]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" || r.Sequence != 4 || r.Revision != 1 {
		t.Fatalf("result 8 svc-b keeps its own record: %+v", r)
	}

	// The final list holds only the accepted observations: both i1 healthy,
	// svc-a at sequence 10 and svc-b at sequence 4, no reason anywhere, and
	// both revisions still 1.
	if len(got.Services) != 2 {
		t.Fatalf("services: %+v", got.Services)
	}
	wantInsts := map[string]registerInstance{
		"svc-a": {ID: "i1", Address: "h1:8080", Health: "healthy", Sequence: 10},
		"svc-b": {ID: "i1", Address: "h1:8080", Health: "healthy", Sequence: 4},
	}
	for _, svc := range got.Services {
		want, ok := wantInsts[svc.Service]
		if !ok {
			t.Fatalf("unexpected service: %+v", svc)
		}
		if svc.Revision != 1 || len(svc.Instances) != 1 || svc.Instances[0] != want {
			t.Fatalf("service %s: got %+v want revision 1 with instance %+v", svc.Service, svc, want)
		}
	}
}
