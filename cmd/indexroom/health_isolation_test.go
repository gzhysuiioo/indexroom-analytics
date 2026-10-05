package main

import (
	"encoding/json"
	"testing"
)

// TestHealthIsolationAcrossServicesSameInstanceID is the regression guard for
// two services registering the same instance id — here even at the very same
// address — inside one batch: an instance id is meaningful only inside its
// own service, so alpha's i1 and beta's i1 keep independent health records,
// sequences and reasons. The batch interleaves both services' observations:
//
//   - alpha's i1 accepts an unhealthy observation at sequence 20; beta's i1
//     still accepts sequence 1 — it is not stale against alpha's 20 and does
//     not inherit alpha's reason;
//   - each later observation compares only against its own service's accepted
//     record: beta turns unhealthy at its own next sequence 2 while alpha
//     sits at 20, and alpha recovers at its own next sequence 21;
//   - health updates never bump either registration revision, and the final
//     service list carries each instance's own state, sequence and normalized
//     reason.
//
// Every request succeeds, so the exit status is 0.
func TestHealthIsolationAcrossServicesSameInstanceID(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"alpha","expectedRevision":0,"instances":[{"id":"i1","address":"shared:8080"}]},
		{"type":"register","service":"beta","expectedRevision":0,"instances":[{"id":"i1","address":"shared:8080"}]},
		{"type":"health","service":"alpha","instanceId":"i1","expectedRevision":1,"sequence":20,"healthy":false,"reason":"alpha 连接超时"},
		{"type":"health","service":"beta","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"beta","expectedRevision":1},
		{"type":"health","service":"beta","instanceId":"i1","expectedRevision":1,"sequence":2,"healthy":false,"reason":"  beta 磁盘满  "},
		{"type":"health","service":"alpha","instanceId":"i1","expectedRevision":1,"sequence":21,"healthy":true},
		{"type":"select","service":"alpha","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 0 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 8 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 0-1: both services registered at revision 1.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0: %+v", r)
	}
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 1: %+v", r)
	}
	// 2: alpha's i1 accepts sequence 20 unhealthy.
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 20 {
		t.Fatalf("result 2: %+v", r)
	}
	// 3: beta's i1 accepts sequence 1 — alpha's 20 must not make it stale.
	if r := got.Results[3]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 1 {
		t.Fatalf("result 3: %+v", r)
	}
	// 4: beta selects its own healthy i1 with its own latest sequence.
	if r := got.Results[4]; !r.OK || r.InstanceID != "i1" || r.Address != "shared:8080" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 4: %+v", r)
	}
	// 5: beta turns unhealthy at its own next sequence 2 while alpha sits at 20.
	if r := got.Results[5]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 2 {
		t.Fatalf("result 5: %+v", r)
	}
	// 6: alpha recovers at its own next sequence 21.
	if r := got.Results[6]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 21 {
		t.Fatalf("result 6: %+v", r)
	}
	// 7: alpha is selectable again with its own recovered record.
	if r := got.Results[7]; !r.OK || r.InstanceID != "i1" || r.Address != "shared:8080" || r.Sequence != 21 || r.Revision != 1 {
		t.Fatalf("result 7: %+v", r)
	}

	// The final list carries each service's own record: alpha healthy at 21
	// with its reason cleared, beta unhealthy at 2 with its trimmed reason;
	// neither revision moved from 1 because health updates never bump it.
	if len(got.Services) != 2 {
		t.Fatalf("services: %+v", got.Services)
	}
	alpha, beta := got.Services[0], got.Services[1]
	if alpha.Service != "alpha" || alpha.Revision != 1 || len(alpha.Instances) != 1 {
		t.Fatalf("alpha view: %+v", alpha)
	}
	if inst := alpha.Instances[0]; inst.ID != "i1" || inst.Address != "shared:8080" ||
		inst.Health != "healthy" || inst.Sequence != 21 || inst.Reason != "" {
		t.Fatalf("alpha i1: %+v", inst)
	}
	if beta.Service != "beta" || beta.Revision != 1 || len(beta.Instances) != 1 {
		t.Fatalf("beta view: %+v", beta)
	}
	if inst := beta.Instances[0]; inst.ID != "i1" || inst.Address != "shared:8080" ||
		inst.Health != "unhealthy" || inst.Sequence != 2 || inst.Reason != "beta 磁盘满" {
		t.Fatalf("beta i1: %+v", inst)
	}
}

// TestHealthFailureIsolationAcrossServicesSameInstanceID is the regression
// guard for the failure side of the same rule: with both services holding an
// i1 at the same address, a stale or conflicting observation rejected by one
// service overwrites nothing there and changes nothing in the other service,
// and selection reflects each service's own accepted health. The batch:
//
//   - alpha's i1 is unhealthy at sequence 20, beta's i1 healthy at sequence 1;
//   - selecting alpha is no_healthy (naming no instance or address) while
//     beta still selects its own i1 with its own address and sequence;
//   - an older sequence for alpha is stale and reports alpha's own current
//     sequence 20; the same sequence 20 with different content is a conflict;
//   - right after alpha's failures beta still accepts its own next sequence 2;
//   - alpha recovers at its own next sequence 21 and becomes selectable again
//     without affecting beta's record.
//
// The batch contains failures, so the exit status is 1; the failed items keep
// their result slots and the final list reflects only accepted observations.
func TestHealthFailureIsolationAcrossServicesSameInstanceID(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"alpha","expectedRevision":0,"instances":[{"id":"i1","address":"shared:8080"}]},
		{"type":"register","service":"beta","expectedRevision":0,"instances":[{"id":"i1","address":"shared:8080"}]},
		{"type":"health","service":"alpha","instanceId":"i1","expectedRevision":1,"sequence":20,"healthy":false,"reason":"alpha down"},
		{"type":"health","service":"beta","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"alpha","expectedRevision":1},
		{"type":"select","service":"beta","expectedRevision":1},
		{"type":"health","service":"alpha","instanceId":"i1","expectedRevision":1,"sequence":19,"healthy":true},
		{"type":"health","service":"alpha","instanceId":"i1","expectedRevision":1,"sequence":20,"healthy":true},
		{"type":"health","service":"beta","instanceId":"i1","expectedRevision":1,"sequence":2,"healthy":true},
		{"type":"select","service":"beta","expectedRevision":1},
		{"type":"health","service":"alpha","instanceId":"i1","expectedRevision":1,"sequence":21,"healthy":true},
		{"type":"select","service":"alpha","expectedRevision":1}
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
	if len(got.Results) != 12 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 2-3: the interleaved observations land on their own services.
	if r := got.Results[2]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 20 {
		t.Fatalf("result 2: %+v", r)
	}
	if r := got.Results[3]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 1 {
		t.Fatalf("result 3: %+v", r)
	}
	// 4: alpha has no healthy instance: no_healthy with a reason and the
	// current revision, fabricating neither an instance id nor an address.
	if r := got.Results[4]; r.OK || r.Error != "no_healthy" || r.Reason == "" ||
		r.Revision != 1 || r.InstanceID != "" || r.Address != "" {
		t.Fatalf("result 4 no_healthy: %+v", r)
	}
	// 5: beta still selects its own healthy i1 with its own address and its
	// latest accepted sequence.
	if r := got.Results[5]; !r.OK || r.InstanceID != "i1" || r.Address != "shared:8080" || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 5: %+v", r)
	}
	// 6: an older sequence for alpha is stale and reports alpha's own current
	// sequence 20 — not anything from beta's record.
	if r := got.Results[6]; r.OK || r.Error != "stale" ||
		r.Reason != "sequence 19 is older than the current sequence 20" ||
		r.Sequence != 20 || r.Revision != 1 {
		t.Fatalf("result 6 stale: %+v", r)
	}
	// 7: alpha's accepted sequence 20 reused with different content conflicts.
	if r := got.Results[7]; r.OK || r.Error != "conflict" ||
		r.Reason != "sequence 20 already used with different health content" ||
		r.Sequence != 20 || r.Revision != 1 {
		t.Fatalf("result 7 conflict: %+v", r)
	}
	// 8: right after alpha's failures beta still accepts its own next
	// sequence 2 — the rejections neither blocked nor resequenced beta.
	if r := got.Results[8]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 2 {
		t.Fatalf("result 8: %+v", r)
	}
	// 9: beta selects with its newly accepted sequence.
	if r := got.Results[9]; !r.OK || r.InstanceID != "i1" || r.Address != "shared:8080" || r.Sequence != 2 || r.Revision != 1 {
		t.Fatalf("result 9: %+v", r)
	}
	// 10: alpha recovers at its own next sequence 21.
	if r := got.Results[10]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 21 {
		t.Fatalf("result 10: %+v", r)
	}
	// 11: alpha is selectable again with its own recovered record.
	if r := got.Results[11]; !r.OK || r.InstanceID != "i1" || r.Address != "shared:8080" || r.Sequence != 21 || r.Revision != 1 {
		t.Fatalf("result 11: %+v", r)
	}

	// The final list reflects only accepted observations: alpha healthy at 21
	// with its old reason cleared, beta healthy at 2; the rejected stale and
	// conflicting reports left no trace, and both revisions stayed at 1.
	if len(got.Services) != 2 {
		t.Fatalf("services: %+v", got.Services)
	}
	alpha, beta := got.Services[0], got.Services[1]
	if alpha.Service != "alpha" || alpha.Revision != 1 || len(alpha.Instances) != 1 {
		t.Fatalf("alpha view: %+v", alpha)
	}
	if inst := alpha.Instances[0]; inst.ID != "i1" || inst.Address != "shared:8080" ||
		inst.Health != "healthy" || inst.Sequence != 21 || inst.Reason != "" {
		t.Fatalf("alpha i1: %+v", inst)
	}
	if beta.Service != "beta" || beta.Revision != 1 || len(beta.Instances) != 1 {
		t.Fatalf("beta view: %+v", beta)
	}
	if inst := beta.Instances[0]; inst.ID != "i1" || inst.Address != "shared:8080" ||
		inst.Health != "healthy" || inst.Sequence != 2 || inst.Reason != "" {
		t.Fatalf("beta i1: %+v", inst)
	}
}
