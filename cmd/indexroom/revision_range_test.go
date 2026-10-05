package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"testing"
)

// TestRevisionRangeCheckedOnRawValue verifies that register, health and select
// judge expectedRevision by the submitted value, not by a value truncated to
// this build's int: an integer beyond the platform range and any negative are
// per-item invalid, the failure keeps its slot and changes nothing, and later
// requests still run against the state committed by earlier ones.
func TestRevisionRangeCheckedOnRawValue(t *testing.T) {
	over := fmt.Sprintf("%d", uint64(math.MaxInt)+1)   // first integer beyond this build's range
	under := fmt.Sprintf("-%d", uint64(math.MaxInt)+2) // first negative beyond it
	max := fmt.Sprintf("%d", int64(math.MaxInt))       // largest acceptable revision

	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"a","address":"h:1"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"register","service":"svc","expectedRevision":` + over + `,"instances":[{"id":"z","address":"h:9"}]},
		{"type":"health","service":"svc","instanceId":"a","expectedRevision":` + under + `,"sequence":2,"healthy":false,"reason":"x"},
		{"type":"select","service":"svc","expectedRevision":` + over + `,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":` + max + `},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 7 {
		t.Fatalf("results: %+v", got.Results)
	}
	// The over-range and negative submissions are invalid, never conflict or
	// not_found, report the service's actual revision, and name expectedRevision.
	for _, i := range []int{2, 3, 4} {
		r := got.Results[i]
		if r.OK || r.Error != "invalid" || r.Revision != 1 {
			t.Fatalf("result %d: %+v", i, r)
		}
		if !strings.Contains(r.Reason, "expectedRevision") {
			t.Fatalf("result %d: reason %q does not name expectedRevision", i, r.Reason)
		}
	}
	// The largest in-range integer is a normal conflict keeping the raw
	// expected value, not an invalid and never clamped to a 32-bit limit.
	if r := got.Results[5]; r.OK || r.Error != "conflict" ||
		r.ExpectedRevision != int(math.MaxInt) || r.ActualRevision != 1 {
		t.Fatalf("result 5: %+v", r)
	}
	// The failed selections moved no cursor and bound no session: the next
	// valid selection takes the smallest healthy id.
	if r := got.Results[6]; !r.OK || r.InstanceID != "a" || r.Address != "h:1" || r.Sequence != 1 {
		t.Fatalf("result 6: %+v", r)
	}
	// No service was created, no instance replaced, no health record written.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 || len(svc.Instances) != 1 {
		t.Fatalf("service: %+v", svc)
	}
	inst := svc.Instances[0]
	if inst.ID != "a" || inst.Address != "h:1" || inst.Health != "healthy" || inst.Sequence != 1 {
		t.Fatalf("instance: %+v", inst)
	}
}

// TestRevisionRangeInvalidReportsActualRevision verifies the invalid result
// carries the service's revision at the time the item is handled — 0 for an
// unknown service — and creates nothing.
func TestRevisionRangeInvalidReportsActualRevision(t *testing.T) {
	over := fmt.Sprintf("%d", uint64(math.MaxInt)+1)
	input := `{"requests":[
		{"type":"select","service":"ghost","expectedRevision":` + over + `},
		{"type":"register","service":"ghost","expectedRevision":` + over + `,"instances":[]},
		{"type":"health","service":"ghost","instanceId":"a","expectedRevision":` + over + `,"sequence":1,"healthy":true}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 3 {
		t.Fatalf("results: %+v", got.Results)
	}
	for i, r := range got.Results {
		if r.OK || r.Error != "invalid" || r.Revision != 0 {
			t.Fatalf("result %d: %+v", i, r)
		}
		if !strings.Contains(r.Reason, "expectedRevision") {
			t.Fatalf("result %d: reason %q does not name expectedRevision", i, r.Reason)
		}
	}
	if len(got.Services) != 0 {
		t.Fatalf("no service should be created: %+v", got.Services)
	}
}
