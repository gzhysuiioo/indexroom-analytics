package main

// This file is the end-to-end regression guard for the register command's
// session stickiness when an offline health observation targeting the bound
// instance is REJECTED. With a, b, c all healthy, session s first rotates to a
// and a following plain select moves the rotation to b. Two rejection kinds
// are then exercised against a, each from that identical starting state:
//
//   - a stale report (sequence 9 below the accepted 10) returns "stale";
//   - a same-sequence/different-content report (sequence 10 flipped to
//     unhealthy with a non-empty reason) returns "conflict".
//
// Both rejections must report ok:false with the current registration revision,
// the accepted health sequence 10 and a reason naming the specific rejection,
// must omit changed:true, and the same-sequence conflict must keep omitting
// expectedRevision/actualRevision so it cannot be misread as a registration
// revision mismatch. Because the unhealthy content is not accepted, the
// instance stays healthy: the bound session s must keep returning a's existing
// address and sequence 10 rather than falling back to the rotation and
// rebinding, the next plain select must continue just after the previously
// chosen b and land on c (no skipped slot, no restart at a, no consumed
// position), and reusing s afterwards must still answer a, proving the plain
// selection rewrote no binding. The final service list keeps a healthy at
// sequence 10 with no reason, leaves b and c untouched, and does not bump the
// registration revision.
//
// The batch drives the same path as the CLI through runRegister: every
// invocation starts from an empty registry, so the registration, the offline
// observations, the rejections and all selections are items of the one batch,
// processed strictly in input order. The rejected item keeps its result slot
// beside the later successful selections, and a batch containing it exits 1.

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestRejectedHealthKeepsSessionBindingAndRotation drives both rejection kinds
// through identical batches that differ only in the rejected health item.
func TestRejectedHealthKeepsSessionBindingAndRotation(t *testing.T) {
	cases := []struct {
		name      string
		rejection string
		errorKind string
		reason    string
	}{
		{
			name:      "stale sequence",
			rejection: `{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":9,"healthy":false,"reason":"心跳超时"}`,
			errorKind: "stale",
			reason:    "sequence 9 is older than the current sequence 10",
		},
		{
			name:      "same sequence different content",
			rejection: `{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":10,"healthy":false,"reason":"心跳超时"}`,
			errorKind: "conflict",
			reason:    "sequence 10 already used with different health content",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := `{"requests":[
				{"type":"register","service":"svc","expectedRevision":0,"instances":[
					{"id":"a","address":"h1:8080"},
					{"id":"b","address":"h2:8080"},
					{"id":"c","address":"h3:8080"}
				]},
				{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":10,"healthy":true},
				{"type":"health","service":"svc","instanceId":"b","expectedRevision":1,"sequence":1,"healthy":true},
				{"type":"health","service":"svc","instanceId":"c","expectedRevision":1,"sequence":1,"healthy":true},
				{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
				{"type":"select","service":"svc","expectedRevision":1},
				` + tc.rejection + `,
				{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
				{"type":"select","service":"svc","expectedRevision":1},
				{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"}
			]}`
			out, code := runRegisterWith(t, input)
			if code != 1 {
				t.Fatalf("batch contains a rejected item, exit code must be 1, got %d, output: %s", code, out)
			}
			var got registerOutput
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output is not JSON: %v\n%s", err, out)
			}
			// One result per request, in input order: the rejected item keeps its
			// slot and the later selections still run against the accepted state.
			if len(got.Results) != 10 {
				t.Fatalf("results: %+v", got.Results)
			}

			// 0: create at revision 1.
			if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
				t.Fatalf("result 0 create: %+v", r)
			}
			// 1-3: the healthy observations are accepted without bumping the
			// registration revision; a's accepted sequence is 10.
			if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 10 {
				t.Fatalf("result 1 a health: %+v", r)
			}
			for i := 2; i <= 3; i++ {
				if r := got.Results[i]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 1 {
					t.Fatalf("result %d health: %+v", i, r)
				}
			}
			// 4: s first rotates to the smallest healthy id a and binds s -> a;
			// the rotation rests on a.
			if r := got.Results[4]; !r.OK || r.InstanceID != "a" || r.Address != "h1:8080" || r.Sequence != 10 || r.Revision != 1 {
				t.Fatalf("result 4 session bind: %+v", r)
			}
			// 5: the plain select continues just after a and chooses b; the
			// rotation rests on b.
			if r := got.Results[5]; !r.OK || r.InstanceID != "b" || r.Address != "h2:8080" || r.Sequence != 1 || r.Revision != 1 {
				t.Fatalf("result 5 plain select: %+v", r)
			}

			// 6: the rejected observation. It reports ok:false, the specific
			// error kind and reason, the current registration revision 1 and the
			// accepted sequence 10. No revision comparison pair is reported for
			// either rejection (the conflict is a health-content conflict, not a
			// registration mismatch), and changed must not appear.
			r := got.Results[6]
			if r.OK || r.Error != tc.errorKind || r.Reason != tc.reason ||
				r.Revision != 1 || r.Sequence != 10 {
				t.Fatalf("result 6 rejection: %+v want kind %q reason %q", r, tc.errorKind, tc.reason)
			}
			if !omitsRevisionPair(r) {
				t.Fatalf("result 6 must omit expectedRevision/actualRevision: %+v", r)
			}
			if r.InstanceID != "" || r.Address != "" {
				t.Fatalf("result 6 must fabricate no target: %+v", r)
			}
			raw := string(mustResultJSON(t, out, 6))
			if strings.Contains(raw, `"changed"`) {
				t.Fatalf("result 6 must not carry changed: %s", raw)
			}

			// 7: the rejected unhealthy content was not accepted, so a is still
			// healthy and the session keeps its existing binding: s returns a's
			// existing address and the accepted sequence 10 instead of falling
			// back to the rotation (which would have chosen c).
			if r := got.Results[7]; !r.OK || r.InstanceID != "a" || r.Address != "h1:8080" || r.Sequence != 10 || r.Revision != 1 {
				t.Fatalf("result 7 session must keep a: %+v", r)
			}
			// 8: the plain rotation still rests on b — the rejection and the
			// session reuse moved nothing — so it continues just after b and
			// chooses c: not a restart at a, not a skipped slot, not b again.
			if r := got.Results[8]; !r.OK || r.InstanceID != "c" || r.Address != "h3:8080" || r.Sequence != 1 || r.Revision != 1 {
				t.Fatalf("result 8 plain select must continue after b to c: %+v", r)
			}
			// 9: reusing s still answers a, proving the plain selection through c
			// rewrote the session binding neither to c nor to anything else.
			if r := got.Results[9]; !r.OK || r.InstanceID != "a" || r.Address != "h1:8080" || r.Sequence != 10 || r.Revision != 1 {
				t.Fatalf("result 9 session must still be bound to a: %+v", r)
			}

			// The final list reflects only accepted state: revision unchanged at
			// 1; a stays healthy at sequence 10 with no unhealthy reason; b and c
			// keep their own addresses and records exactly as accepted.
			if len(got.Services) != 1 {
				t.Fatalf("services: %+v", got.Services)
			}
			svc := got.Services[0]
			if svc.Service != "svc" || svc.Revision != 1 {
				t.Fatalf("service view: %+v", svc)
			}
			wantInsts := []registerInstance{
				{ID: "a", Address: "h1:8080", Health: "healthy", Sequence: 10},
				{ID: "b", Address: "h2:8080", Health: "healthy", Sequence: 1},
				{ID: "c", Address: "h3:8080", Health: "healthy", Sequence: 1},
			}
			if len(svc.Instances) != len(wantInsts) {
				t.Fatalf("instances: %+v", svc.Instances)
			}
			for i, want := range wantInsts {
				if svc.Instances[i] != want {
					t.Fatalf("instance %d: got %+v want %+v", i, svc.Instances[i], want)
				}
			}
		})
	}
}
