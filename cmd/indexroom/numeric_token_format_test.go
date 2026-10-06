package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// These are the two tokens from the defect report: legal JSON numbers whose
// integer part is too long for int64 and which strconv.ParseInt rejects with
// ErrRange, even though one carries a decimal point and the other an exponent.
// The exponent token even evaluates to far less than 1. None of that matters:
// neither is written as a decimal integer, so it is a format problem, never a
// range problem and never an accepted converted value.
const (
	longDecimalToken  = "18446744073709551616.0"
	longExponentToken = "18446744073709551616e-20"
	// A capital-E variant keeps the long integer part as well.
	longExponentUpperToken = "18446744073709551616E3"
)

// assertIntegerFormatInvalid marks a result as the field's integer-format
// rejection: invalid kind, the "must be an integer" reason quoting the raw
// token, no range wording, the service's current revision, and neither a
// revision-comparison pair nor (for health) a fabricated sequence.
func assertIntegerFormatInvalid(t *testing.T, r registerResult, field string, currentRevision int, token string) {
	t.Helper()
	want := field + " must be an integer, got " + token
	if r.OK || r.Error != "invalid" {
		t.Fatalf("want invalid for %s token %s, got %+v", field, token, r)
	}
	if r.Reason != want {
		t.Fatalf("reason:\n got %q\nwant %q", r.Reason, want)
	}
	if strings.Contains(r.Reason, "between 0 and") || strings.Contains(r.Reason, "between 1 and") {
		t.Fatalf("a non-integer token must not be reported as out of range: %q", r.Reason)
	}
	if r.Revision != currentRevision {
		t.Fatalf("revision should report the current revision %d, got %d (%+v)", currentRevision, r.Revision, r)
	}
	if !omitsRevisionPair(r) {
		t.Fatalf("a format failure must not report a revision comparison: %+v", r)
	}
}

// TestRegisterExpectedRevisionNonIntegerNotationAcrossKinds is the
// end-to-end guard for the expectedRevision field. A decimal point or exponent
// makes the token a format problem for every request kind that carries
// expectedRevision (register, health, select, release_session), regardless of
// the token's magnitude: the two long-integer-part tokens that ParseInt
// range-rejects must be classified exactly like 1.5, not like an out-of-range
// integer. Each rejection stamps the service's current revision, carries no
// comparison pair, and changes no state; later valid requests and the final
// service list prove the accepted registration, health record, rotation and
// session binding all survived.
func TestRegisterExpectedRevisionNonIntegerNotationAcrossKinds(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-42"},
		{"type":"register","service":"svc","expectedRevision":` + longDecimalToken + `,"instances":[{"id":"i1","address":"h1:9999"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":` + longExponentToken + `,"sequence":2,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":` + longExponentUpperToken + `},
		{"type":"release_session","service":"svc","expectedRevision":` + longDecimalToken + `,"sessionKey":"user-42"},
		{"type":"select","service":"missing","expectedRevision":` + longDecimalToken + `},
		{"type":"register","service":"svc","expectedRevision":1.5,"instances":[]},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"user-42"}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains rejections, exit code: %d, output: %s", code, out)
	}
	got := decodeRegisterOutput(t, out)
	if len(got.Results) != 10 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0-2: the accepted state the rejected items must not touch.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 register: %+v", r)
	}
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 1 {
		t.Fatalf("result 1 health: %+v", r)
	}
	if r := got.Results[2]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" || r.Sequence != 1 {
		t.Fatalf("result 2 session select: %+v", r)
	}

	// 3-7: every long-integer-part non-integer token is a format rejection at
	// the current revision (1 for the existing service, 0 for an unknown one),
	// never a range rejection, conflict or not_found.
	assertIntegerFormatInvalid(t, got.Results[3], "expectedRevision", 1, longDecimalToken)
	assertIntegerFormatInvalid(t, got.Results[4], "expectedRevision", 1, longExponentToken)
	assertIntegerFormatInvalid(t, got.Results[5], "expectedRevision", 1, longExponentUpperToken)
	assertIntegerFormatInvalid(t, got.Results[6], "expectedRevision", 1, longDecimalToken)
	assertIntegerFormatInvalid(t, got.Results[7], "expectedRevision", 0, longDecimalToken)
	// 8: the pre-existing ordinary decimal token keeps the same classification.
	assertIntegerFormatInvalid(t, got.Results[8], "expectedRevision", 1, "1.5")

	// 9: the rejected release never dropped the session binding (result 2), so
	// reusing the key returns the same instance without rotating; health,
	// address and revision are exactly what the accepted items established.
	if r := got.Results[9]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" ||
		r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 9 session must survive rejected items: %+v", r)
	}

	// Final snapshot: revision 1, i1 at its original address healthy at
	// sequence 1. The rejected replacement to h1:9999 and the rejected
	// sequence-2 health never landed.
	if len(got.Services) != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	svc := got.Services[0]
	if svc.Service != "svc" || svc.Revision != 1 || len(svc.Instances) != 1 {
		t.Fatalf("service view: %+v", svc)
	}
	wantInst := registerInstance{ID: "i1", Address: "h1:8080", Health: "healthy", Sequence: 1}
	if svc.Instances[0] != wantInst {
		t.Fatalf("final instance: got %+v want %+v", svc.Instances[0], wantInst)
	}
}

// TestRegisterHealthSequenceNonIntegerNotation is the end-to-end guard for the
// health sequence field. A token containing a decimal point or exponent is a
// sequence format problem quoting the raw text, even when its integer part is
// too long for int64 and even when its value is below 1 (so it must not be
// reported as the zero/not-positive rule or accepted by conversion). A pure
// decimal integer past int64 keeps the range reason, and the accepted record
// and later requests are unaffected.
func TestRegisterHealthSequenceNonIntegerNotation(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + longDecimalToken + `,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + longExponentToken + `,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":0.0,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":9223372036854775808,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains rejections, exit code: %d, output: %s", code, out)
	}

	// The rejected tokens appear verbatim in the document only inside reasons,
	// never as accepted numbers.
	for _, token := range []string{longDecimalToken, longExponentToken} {
		if !strings.Contains(out, token) {
			t.Fatalf("output must quote the submitted token %s:\n%s", token, out)
		}
	}

	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	if len(got.Results) != 8 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0-2: accepted registration, health and selection.
	if r := got.Results[0]; !r.OK || r.Revision != 1 {
		t.Fatalf("result 0 register: %+v", r)
	}
	if r := got.Results[1]; !r.OK || r.Sequence != 1 {
		t.Fatalf("result 1 health: %+v", r)
	}
	if r := got.Results[2]; !r.OK || r.InstanceID != "i1" || r.Sequence != 1 {
		t.Fatalf("result 2 select: %+v", r)
	}

	// 3-5: every token with a decimal point or exponent is the sequence format
	// rejection, with no accepted sequence field and no range or positive-int
	// wording. The exponent token evaluates below 1 yet is still a format
	// problem rather than the zero/negative rule.
	for i, token := range []string{longDecimalToken, longExponentToken, "0.0"} {
		r := got.Results[i+3]
		assertIntegerFormatInvalid(t, r, "sequence", 1, token)
		if r.Sequence != 0 {
			t.Fatalf("result %d must fabricate no accepted sequence: %+v", i+3, r)
		}
		if strings.Contains(r.Reason, "positive integer") {
			t.Fatalf("non-integer notation must not be reported as the positive rule: %q", r.Reason)
		}
	}

	// 6: a pure decimal integer one past int64 is a magnitude problem and keeps
	// the range reason quoting the integer — notation and range stay distinct.
	r := got.Results[6]
	if r.OK || r.Error != "invalid" || r.Revision != 1 ||
		!strings.Contains(r.Reason, "sequence must be an integer between 1 and 9223372036854775807, got 9223372036854775808") {
		t.Fatalf("result 6 overflow must keep the range reason: %+v", r)
	}

	// 7: none of the rejected health items landed; selection still returns i1
	// at sequence 1.
	if r := got.Results[7]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" ||
		r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 7 select must see only accepted state: %+v", r)
	}

	svc := got.Services[0]
	wantInst := registerInstance{ID: "i1", Address: "h1:8080", Health: "healthy", Sequence: 1}
	if svc.Revision != 1 || len(svc.Instances) != 1 || svc.Instances[0] != wantInst {
		t.Fatalf("final state must keep only the accepted observation: %+v", svc)
	}
}

// TestRegisterSequenceZeroAndNegativeUnchanged locks that genuinely integer
// tokens 0 and negatives still reach the positive-value rule with its original
// wording: the notation fix must not reclassify them as format errors.
func TestRegisterSequenceZeroAndNegativeUnchanged(t *testing.T) {
	for _, token := range []string{"0", "-1", "-9223372036854775808"} {
		input := `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":0,"sequence":` + token + `,"healthy":true}]}`
		out, code := runRegisterWith(t, input)
		if code != 1 {
			t.Fatalf("token %s: exit code: %d", token, code)
		}
		r := decodeRegisterOutput(t, out).Results[0]
		if r.OK || r.Error != "invalid" || r.Reason != "sequence must be a positive integer" {
			t.Fatalf("token %s should keep the positive-integer reason, got %+v", token, r)
		}
	}
}
