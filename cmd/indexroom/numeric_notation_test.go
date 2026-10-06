package main

// End-to-end regression guard for numeric tokens that are valid JSON numbers
// but are not written in the decimal-integer notation expectedRevision and a
// health observation's sequence require. A decimal point or exponent part
// names a format problem no matter what the number's value is: even when the
// integer part overflows int64 (18446744073709551616 is 2^64) or the exponent
// drives the value below 1, the item must fail as that request's own invalid
// result with the field's "must be an integer" reason and the number exactly
// as submitted — never as an integer range error, never as the positive-value
// rule, and never accepted after numeric conversion.

import (
	"strings"
	"testing"
)

// assertIntegerNotationInvalid marks the result as an integer-notation
// rejection: invalid kind, the field's integer-type reason quoting the raw
// submitted number, no range wording, the service's current revision, and no
// expected/actual revision comparison pair.
func assertIntegerNotationInvalid(t *testing.T, r registerResult, field string, currentRevision int, submitted string) {
	t.Helper()
	if r.OK || r.Error != "invalid" {
		t.Fatalf("want invalid for %s, got %+v", field, r)
	}
	want := field + " must be an integer, got " + submitted
	if !strings.Contains(r.Reason, want) {
		t.Fatalf("reason should quote %q, got %q", want, r.Reason)
	}
	if strings.Contains(r.Reason, "between 0 and") || strings.Contains(r.Reason, "between 1 and") {
		t.Fatalf("non-integer notation must not be a range error, got %q", r.Reason)
	}
	if r.Revision != currentRevision {
		t.Fatalf("revision should report the current revision %d, got %d (%+v)", currentRevision, r.Revision, r)
	}
	if !omitsRevisionPair(r) {
		t.Fatalf("notation failure must not report a revision comparison: %+v", r)
	}
}

// TestRegisterNonIntegerNotationWithLongIntegerPart drives a decimal-point and
// an exponent token whose integer part is too long for int64 through every
// request kind, alongside an exponent whose value is below 1. Each rejected
// item is only its own invalid result: the exit status is 1, but accepted
// registrations, health records, the rotation cursor and session bindings all
// survive, later valid requests keep their own outcomes, and the final service
// list is still emitted.
func TestRegisterNonIntegerNotationWithLongIntegerPart(t *testing.T) {
	const (
		dotOverflow = "18446744073709551616.0"   // 2^64 with a decimal point
		expTiny     = "18446744073709551616e-20" // numeric value below 1
	)
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"register","service":"svc","expectedRevision":` + dotOverflow + `,"instances":[{"id":"i1","address":"h2:9090"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + dotOverflow + `,"healthy":true},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":` + expTiny + `,"sequence":999,"healthy":true},
		{"type":"select","service":"missing","expectedRevision":` + dotOverflow + `},
		{"type":"release_session","service":"svc","expectedRevision":` + expTiny + `,"sessionKey":"k"},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"k"},
		{"type":"release_session","service":"svc","expectedRevision":1,"sessionKey":"k"},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":` + expTiny + `,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains invalid items, exit code: %d, output: %s", code, out)
	}

	// Both submitted spellings survive verbatim in the raw document, quoted
	// inside their reasons rather than re-rendered as floats or integers.
	if !strings.Contains(out, dotOverflow) || !strings.Contains(out, expTiny) {
		t.Fatalf("output must quote both numbers as submitted:\n%s", out)
	}

	got := decodeRegisterOutput(t, out)
	if len(got.Results) != 11 {
		t.Fatalf("results: %+v", got.Results)
	}

	// 0: service created at revision 1; 1: healthy baseline at sequence 1.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0 register: %+v", r)
	}
	if r := got.Results[1]; !r.OK || !r.Changed || r.Revision != 1 || r.Sequence != 1 {
		t.Fatalf("result 1 health: %+v", r)
	}

	// 2: a register revision carrying a decimal point is a format error at
	// the current revision; the replacement (h2:9090) never applies.
	assertIntegerNotationInvalid(t, got.Results[2], "expectedRevision", 1, dotOverflow)

	// 3: a health sequence carrying a decimal point is a sequence format
	// error, not an out-of-range sequence.
	assertIntegerNotationInvalid(t, got.Results[3], "sequence", 1, dotOverflow)
	if got.Results[3].Sequence != 0 {
		t.Fatalf("result 3 must carry no accepted sequence: %+v", got.Results[3])
	}

	// 4: the exponent token's value is below 1, but the revision is judged
	// first and must be a format error rather than an accepted revision 0;
	// sequence 999 is never reached and writes nothing.
	assertIntegerNotationInvalid(t, got.Results[4], "expectedRevision", 1, expTiny)

	// 5: unknown service, so the invalid result stamps revision 0.
	assertIntegerNotationInvalid(t, got.Results[5], "expectedRevision", 0, dotOverflow)

	// 6: release_session shares the revision check and the same classification;
	// it must not drop the key's (still absent) binding.
	assertIntegerNotationInvalid(t, got.Results[6], "expectedRevision", 1, expTiny)

	// 7: the rejected items bound nothing; this first keyed selection rotates
	// to the single healthy instance and binds the key.
	if r := got.Results[7]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" ||
		r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 7 keyed select: %+v", r)
	}
	// 8: releasing the bound key succeeds changed; the rejected item 6 did not
	// pre-release it.
	if r := got.Results[8]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 8 release: %+v", r)
	}

	// 9: the exponent token evaluates below 1, but it must not become the
	// positive-value ("sequence must be a positive integer") error or a range
	// error: the notation itself is wrong, with the submitted spelling.
	r := got.Results[9]
	if r.OK || r.Error != "invalid" ||
		!strings.Contains(r.Reason, "sequence must be an integer, got "+expTiny) ||
		strings.Contains(r.Reason, "positive") || strings.Contains(r.Reason, "between 1 and") ||
		r.Revision != 1 || r.Sequence != 0 {
		t.Fatalf("result 9 sub-one exponent sequence: %+v", r)
	}

	// 10: a later valid selection still runs against the preserved record.
	if r := got.Results[10]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" ||
		r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 10 final select: %+v", r)
	}

	// The final list reflects only accepted work: revision 1, the original
	// address, healthy at sequence 1 — none of the rejected tokens landed.
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

// TestRegisterExponentEquallingIntegerIsNotationError pins the "even when it
// equals an integer" rule: an exponent spelling of an in-range integer cannot
// be accepted as that integer, for either field.
func TestRegisterExponentEquallingIntegerIsNotationError(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"select","service":"svc","expectedRevision":1e0},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1e0,"healthy":true}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains invalid items, exit code: %d, output: %s", code, out)
	}
	got := decodeRegisterOutput(t, out)
	assertIntegerNotationInvalid(t, got.Results[1], "expectedRevision", 1, "1e0")
	assertIntegerNotationInvalid(t, got.Results[2], "sequence", 1, "1e0")
}
