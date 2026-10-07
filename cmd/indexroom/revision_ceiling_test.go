package main

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestRegisterRevisionCeilingResultShape pins the serialized per-item result
// for a real list change once the service's revision is at the architecture
// limit. The registry package seeds that state directly (the limit cannot be
// reached by ordinary increments); here the processor maps exactly the outcome
// Apply returns at the ceiling through the same reject path every business
// failure uses, so the JSON contract is observable: ok:false, error:"invalid",
// the limit kept as revision, a reason naming the limit, and neither changed
// nor the revision-mismatch comparison fields.
func TestRegisterRevisionCeilingResultShape(t *testing.T) {
	limit := math.MaxInt
	proc := &registerProcessor{}
	proc.reject("svc", "invalid",
		"registration revision has reached the limit "+strconv.Itoa(limit)+"; cannot save instance list changes",
		limit, 0, 0, false, 0)
	if len(proc.results) != 1 {
		t.Fatalf("results: %+v", proc.results)
	}
	r := proc.results[0]
	if r.OK || r.Error != "invalid" || r.Revision != limit || r.Changed {
		t.Fatalf("ceiling rejection fields: %+v", r)
	}
	if !strings.Contains(r.Reason, "registration revision has reached the limit") ||
		!strings.Contains(r.Reason, strconv.Itoa(limit)) {
		t.Fatalf("ceiling reason should name the limit %d, got %q", limit, r.Reason)
	}
	if !omitsRevisionPair(r) {
		t.Fatalf("ceiling rejection must omit expectedRevision/actualRevision: %+v", r)
	}

	var raw map[string]json.RawMessage
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, absent := range []string{"changed", "expectedRevision", "actualRevision", "sequence", "instanceId", "address"} {
		if _, ok := raw[absent]; ok {
			t.Fatalf("ceiling result JSON must omit %q, got %s", absent, data)
		}
	}
	var fields struct {
		Service  string `json:"service"`
		OK       bool   `json:"ok"`
		Error    string `json:"error"`
		Reason   string `json:"reason"`
		Revision int    `json:"revision"`
	}
	if err := json.Unmarshal(data, &fields); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if fields.Service != "svc" || fields.OK || fields.Error != "invalid" || fields.Revision != limit || fields.Reason == "" {
		t.Fatalf("ceiling JSON contract: %+v", fields)
	}
}
