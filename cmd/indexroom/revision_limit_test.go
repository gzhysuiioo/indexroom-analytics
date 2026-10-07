package main

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/gzhysuiioo/indexroom-analytics/indexroom"
)

// The registration revision cap (2147483647 on a 32-bit build,
// 9223372036854775807 on a 64-bit build) cannot be reached end to end by real
// replacements in a test batch; the refusal itself is produced and asserted at
// the registry level. What the command layer adds is the JSON contract, and it
// maps every outcome through the same reject path, so this test feeds that
// path the cap-invalid outcome shape and locks its serialization:
// ok:false, error invalid, revision kept at the cap, the limit named, with no
// changed flag and neither expectedRevision nor actualRevision.
func TestRegisterRevisionLimitInvalidResultShape(t *testing.T) {
	limit := math.MaxInt
	proc := &registerProcessor{}
	// This is exactly the tuple registerRequest passes from Apply's invalid
	// outcome at the cap: Kind invalid, RevisionMismatch false so the revision
	// comparison pointers stay nil, Changed false so omitempty drops it.
	reason := "registration revision has reached its limit and cannot save the list change"
	proc.reject("svc", indexroom.OutcomeInvalid, reason, limit, 0, 0, false, 0)

	raw, err := json.Marshal(proc.results[0])
	if err != nil {
		t.Fatalf("marshal result: %v", err)
	}
	text := string(raw)
	if !strings.Contains(text, `"service":"svc"`) ||
		!strings.Contains(text, `"ok":false`) ||
		!strings.Contains(text, `"error":"invalid"`) ||
		!strings.Contains(text, `"revision":`+strconv.Itoa(limit)) {
		t.Fatalf("cap-invalid result must report ok:false, error invalid and the cap revision, got %s", text)
	}
	if strings.Contains(text, "changed") ||
		strings.Contains(text, "expectedRevision") ||
		strings.Contains(text, "actualRevision") ||
		strings.Contains(text, "instanceId") ||
		strings.Contains(text, "address") ||
		strings.Contains(text, "sequence") {
		t.Fatalf("cap-invalid result must omit changed, the revision pair and target fields, got %s", text)
	}
	if !proc.failed {
		t.Fatalf("the invalid item must mark the batch as failed")
	}
}
