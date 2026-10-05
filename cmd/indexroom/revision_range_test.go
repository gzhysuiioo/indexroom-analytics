package main

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
)

// hugeRevisionJSON is a decimal integer larger than int64 can hold. It is out
// of the accepted range on every architecture, so it exercises the raw-token
// range rejection end to end without depending on the word size.
const hugeRevisionJSON = "99999999999999999999999"

// maxAcceptedRevisionJSON is the upper bound accepted by the test binary.
func maxAcceptedRevisionJSON() string { return strconv.FormatInt(int64(math.MaxInt), 10) }

// decodeRegisterOutput parses one register command output.
func decodeRegisterOutput(t *testing.T, out string) registerOutput {
	t.Helper()
	var got registerOutput
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, out)
	}
	return got
}

// assertRangeInvalid marks the result as an expectedRevision range rejection:
// invalid kind, a reason stating the range and the submitted value, the
// service's current revision, and no expected/actual comparison fields.
func assertRangeInvalid(t *testing.T, r registerResult, currentRevision int, submitted string) {
	t.Helper()
	if r.OK || r.Error != "invalid" {
		t.Fatalf("want invalid, got %+v", r)
	}
	if !strings.Contains(r.Reason, "expectedRevision must be an integer between 0 and") {
		t.Fatalf("reason should state the expectedRevision range, got %q", r.Reason)
	}
	if !strings.Contains(r.Reason, "got "+submitted) {
		t.Fatalf("reason should report the submitted value %q, got %q", submitted, r.Reason)
	}
	if r.Revision != currentRevision {
		t.Fatalf("revision should report the current revision %d, got %d (%+v)", currentRevision, r.Revision, r)
	}
	if r.ExpectedRevision != 0 || r.ActualRevision != 0 {
		t.Fatalf("range failure must not report a revision comparison: %+v", r)
	}
}

// TestRegisterExpectedRevisionBeyondInt64 covers the word-size-independent
// overflow: an integer too large even for int64 is invalid for every request
// kind, with the range reason and the raw value, and never reported as a
// conflict or not_found.
func TestRegisterExpectedRevisionBeyondInt64(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":` + hugeRevisionJSON + `,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":` + hugeRevisionJSON + `,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":` + hugeRevisionJSON + `},
		{"type":"select","service":"missing","expectedRevision":` + hugeRevisionJSON + `}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d, output: %s", code, out)
	}
	got := decodeRegisterOutput(t, out)
	if len(got.Results) != 5 {
		t.Fatalf("results: %+v", got.Results)
	}
	assertRangeInvalid(t, got.Results[0], 0, hugeRevisionJSON)
	assertRangeInvalid(t, got.Results[2], 1, hugeRevisionJSON)
	assertRangeInvalid(t, got.Results[3], 1, hugeRevisionJSON)
	assertRangeInvalid(t, got.Results[4], 0, hugeRevisionJSON)
	// The overflowing register created nothing; only item 1 (the valid create)
	// is reflected in the final snapshot.
	if len(got.Services) != 1 || got.Services[0].Service != "svc" || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
}

// TestRegisterExpectedRevisionNegativeRangeReason checks that a negative
// revision names the range problem and stays ahead of the revision comparison
// for every request kind.
func TestRegisterExpectedRevisionNegativeRangeReason(t *testing.T) {
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"register","service":"svc","expectedRevision":-4294967295,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":-1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":-1},
		{"type":"select","service":"missing","expectedRevision":-1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	got := decodeRegisterOutput(t, out)
	assertRangeInvalid(t, got.Results[1], 1, "-4294967295")
	assertRangeInvalid(t, got.Results[2], 1, "-1")
	assertRangeInvalid(t, got.Results[3], 1, "-1")
	assertRangeInvalid(t, got.Results[4], 0, "-1")
}

// TestRegisterExpectedRevisionIntegerTypeUnchanged locks that the existing
// integer-type and required-field checks survive the range work, including the
// distinction between "missing" and "wrong type" messages.
func TestRegisterExpectedRevisionIntegerTypeUnchanged(t *testing.T) {
	cases := map[string]string{
		"register float":  `{"requests":[{"service":"s","expectedRevision":1.5,"instances":[]}]}`,
		"register exp":    `{"requests":[{"service":"s","expectedRevision":1e3,"instances":[]}]}`,
		"register string": `{"requests":[{"service":"s","expectedRevision":"1","instances":[]}]}`,
		"register bool":   `{"requests":[{"service":"s","expectedRevision":true,"instances":[]}]}`,
		"health float":    `{"requests":[{"type":"health","service":"s","instanceId":"a","expectedRevision":1.5,"sequence":1,"healthy":true}]}`,
		"select float":    `{"requests":[{"type":"select","service":"s","expectedRevision":1.5}]}`,
	}
	for name, input := range cases {
		t.Run(name, func(t *testing.T) {
			out, code := runRegisterWith(t, input)
			if code != 1 {
				t.Fatalf("exit code: %d", code)
			}
			got := decodeRegisterOutput(t, out)
			r := got.Results[0]
			if r.OK || r.Error != "invalid" {
				t.Fatalf("want invalid, got %+v", r)
			}
			if !strings.Contains(r.Reason, "expectedRevision must be an integer") ||
				strings.Contains(r.Reason, "between 0 and") {
				t.Fatalf("want the integer-type reason, got %q", r.Reason)
			}
		})
	}
	// Missing and explicit null keep the required message.
	for name, input := range map[string]string{
		"missing register": `{"requests":[{"service":"s","instances":[]}]}`,
		"null register":    `{"requests":[{"service":"s","expectedRevision":null,"instances":[]}]}`,
		"missing health":   `{"requests":[{"type":"health","service":"s","instanceId":"a","sequence":1,"healthy":true}]}`,
		"missing select":   `{"requests":[{"type":"select","service":"s"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			out, code := runRegisterWith(t, input)
			if code != 1 {
				t.Fatalf("exit code: %d", code)
			}
			r := decodeRegisterOutput(t, out).Results[0]
			if r.OK || r.Error != "invalid" || !strings.Contains(r.Reason, "required") {
				t.Fatalf("want the required-field reason, got %+v", r)
			}
		})
	}
}

// TestRegisterExpectedRevisionBoundaries locks the accepted upper bound and
// the first rejected integer on the running architecture.
func TestRegisterExpectedRevisionBoundaries(t *testing.T) {
	bound := maxAcceptedRevisionJSON()
	// The bound itself is a legal integer: for an unknown service it reaches the
	// revision comparison and conflicts (new services require 0), carrying the
	// original value rather than a truncated one.
	input := `{"requests":[{"type":"register","service":"new","expectedRevision":` + bound + `,"instances":[]}]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	r := decodeRegisterOutput(t, out).Results[0]
	if r.OK || r.Error != "conflict" || r.Revision != 0 {
		t.Fatalf("bound value should reach the revision comparison: %+v", r)
	}
	if strconv.FormatInt(int64(r.ExpectedRevision), 10) != bound || r.ActualRevision != 0 {
		t.Fatalf("conflict should preserve expected %s, got %+v", bound, r)
	}

	// One more than the bound is invalid on every architecture. On 64-bit that
	// value also exceeds int64, so it arrives through the raw-text path; it is
	// computed in uint64 to avoid overflowing the comparison itself.
	boundN, err := strconv.ParseUint(bound, 10, 64)
	if err != nil {
		t.Fatalf("parse bound: %v", err)
	}
	over := strconv.FormatUint(boundN+1, 10)
	input = `{"requests":[{"type":"select","service":"s","expectedRevision":` + over + `}]}`
	out, code = runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	assertRangeInvalid(t, decodeRegisterOutput(t, out).Results[0], 0, over)
}

// TestRegisterOutOfRangeRevisionBatchIsolation is the end-to-end regression
// guard for the 32-bit wrapping values. Out-of-range items keep their slots as
// invalid rejections; they create nothing, replace nothing, write no health,
// return no target, move no cursor and bind no session. Later valid requests
// run against the pre-rejection state and the existing rotation/session rules.
func TestRegisterOutOfRangeRevisionBatchIsolation(t *testing.T) {
	if strconv.IntSize != 32 {
		// int64 cannot hold a value that overflows 64-bit int; the word-size
		// independent overflow path is covered by TestRegisterExpectedRevisionBeyondInt64.
		t.Skip("wrapping int64 values only exist on a 32-bit build")
	}
	const wrapPositive = "4294967297" // wraps to 1
	const wrapNegative = "-4294967295"
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"},{"id":"i2","address":"h2:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":` + wrapPositive + `,"sequence":1,"healthy":true},
		{"type":"register","service":"svc","expectedRevision":` + wrapNegative + `,"instances":[{"id":"i1","address":"h2:9090"}]},
		{"type":"select","service":"svc","expectedRevision":` + wrapPositive + `},
		{"type":"select","service":"svc","expectedRevision":` + wrapPositive + `,"sessionKey":"s"},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":1,"sequence":1,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":1,"sessionKey":"s"},
		{"type":"select","service":"svc","expectedRevision":1}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("batch contains failures, exit code: %d, output: %s", code, out)
	}
	got := decodeRegisterOutput(t, out)
	if len(got.Results) != 8 {
		t.Fatalf("results: %+v", got.Results)
	}
	// 0: create.
	if r := got.Results[0]; !r.OK || !r.Changed || r.Revision != 1 {
		t.Fatalf("result 0: %+v", r)
	}
	// 1-4: every wrapping value is an invalid range rejection at revision 1,
	// never a conflict/not_found, carrying no target or comparison fields.
	for i, submitted := range []string{wrapPositive, wrapNegative, wrapPositive, wrapPositive} {
		r := got.Results[i+1]
		assertRangeInvalid(t, r, 1, submitted)
		if r.InstanceID != "" || r.Address != "" {
			t.Fatalf("result %d must fabricate no target: %+v", i+1, r)
		}
	}
	// 5: the rejected health wrote nothing; this valid report is the first
	// observation and lands at sequence 1.
	if r := got.Results[5]; !r.OK || !r.Changed || r.Sequence != 1 || r.Revision != 1 {
		t.Fatalf("result 5 valid health: %+v", r)
	}
	// 6: the failed item 4 never bound "s", so this first success rotates to the
	// smallest healthy id (i1) and binds now.
	if r := got.Results[6]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" || r.Sequence != 1 {
		t.Fatalf("result 6 first session select: %+v", r)
	}
	// 7: the rejected selects moved no cursor; with i1 the only healthy
	// instance it is chosen again, and the rejected replacement never changed
	// the address to h2:9090.
	if r := got.Results[7]; !r.OK || r.InstanceID != "i1" || r.Address != "h1:8080" {
		t.Fatalf("result 7: %+v", r)
	}
	// Final snapshot: revision 1, i1 at its original address healthy at 1, i2
	// still unknown at its original address.
	if len(got.Services) != 1 || got.Services[0].Revision != 1 {
		t.Fatalf("services: %+v", got.Services)
	}
	insts := got.Services[0].Instances
	if len(insts) != 2 {
		t.Fatalf("instances: %+v", insts)
	}
	if insts[0].ID != "i1" || insts[0].Address != "h1:8080" || insts[0].Health != "healthy" || insts[0].Sequence != 1 {
		t.Fatalf("i1 state: %+v", insts[0])
	}
	if insts[1].ID != "i2" || insts[1].Address != "h2:8080" || insts[1].Health != "unknown" {
		t.Fatalf("i2 state: %+v", insts[1])
	}
}

// TestRegisterOutOfRangeFieldPrecedence locks where the range rejection sits
// relative to the other field checks.
//
// A value that parses as int64 (the reported 4294967297 and negatives) reaches
// the registry, so the long-standing field order applies on every
// architecture: service name and instance id are checked before the revision
// range, and an explicit null/non-string sessionKey is rejected in the handler
// before anything else. The 32-bit-only subtest adds that, on a 32-bit build,
// the out-of-range revision is judged before the remaining content fields and
// the revision conflict, since it is still a field check.
//
// A token that cannot even be carried as int64 (the huge value) fails at the
// handler's raw-token parse, the same tier that rejects float/string revision
// tokens; TestRegisterExpectedRevisionBeyondInt64 covers it.
func TestRegisterOutOfRangeFieldPrecedence(t *testing.T) {
	const overflow32 = "4294967297" // parses as int64; exceeds int only on 32-bit

	// Earlier-checked fields win on every architecture (on 64-bit the value is
	// in range, but the name/id checks still precede the revision comparison).
	out, code := runRegisterWith(t, `{"requests":[
		{"type":"register","service":"  ","expectedRevision":`+overflow32+`,"instances":[]},
		{"type":"health","service":"svc","instanceId":" ","expectedRevision":`+overflow32+`,"sequence":1,"healthy":true},
		{"type":"select","service":"  ","expectedRevision":`+overflow32+`},
		{"type":"select","service":"svc","expectedRevision":`+overflow32+`,"sessionKey":null}
	]}`)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	got := decodeRegisterOutput(t, out)
	want := []string{
		"service name must not be empty",
		"instance id must not be empty",
		"service name must not be empty",
		"sessionKey must be a string, not null",
	}
	for i, sub := range want {
		r := got.Results[i]
		if r.OK || r.Error != "invalid" || !strings.Contains(r.Reason, sub) {
			t.Fatalf("result %d: want invalid containing %q, got %+v", i, sub, r)
		}
		if r.ExpectedRevision != 0 || r.ActualRevision != 0 {
			t.Fatalf("result %d must not report a revision comparison: %+v", i, r)
		}
	}

	// Same ordering for a parsable negative, on every architecture.
	out, code = runRegisterWith(t, `{"requests":[
		{"type":"register","service":"  ","expectedRevision":-1,"instances":[]},
		{"type":"health","service":"svc","instanceId":" ","expectedRevision":-1,"sequence":1,"healthy":true}
	]}`)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	got = decodeRegisterOutput(t, out)
	for i, sub := range []string{"service name must not be empty", "instance id must not be empty"} {
		if r := got.Results[i]; r.OK || r.Error != "invalid" || !strings.Contains(r.Reason, sub) {
			t.Fatalf("negative result %d: %+v", i, r)
		}
	}

	if strconv.IntSize != 32 {
		return
	}

	// On 32-bit the parsable value is out of range: it beats the remaining
	// content fields and the revision conflict, at the service's revision 1.
	out, code = runRegisterWith(t, `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"register","service":"svc","expectedRevision":`+overflow32+`,"instances":[{"id":"","address":"h1:8080"}]},
		{"type":"health","service":"svc","instanceId":"i1","expectedRevision":`+overflow32+`,"sequence":0,"healthy":true},
		{"type":"select","service":"svc","expectedRevision":`+overflow32+`},
		{"type":"select","service":"missing","expectedRevision":`+overflow32+`}
	]}`)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	got = decodeRegisterOutput(t, out)
	for i, current := range []int{1, 1, 1, 0} {
		r := got.Results[i+1]
		assertRangeInvalid(t, r, current, overflow32)
	}
}

// TestRegisterLargeInRangeRevisionOn64Bit locks that the fix does not clamp
// 64-bit builds to the 32-bit bound: 2147483648 is a legal integer on 64-bit
// and yields an ordinary conflict against a revision-1 service while
// preserving the submitted value. On 32-bit it is rejected as out of range.
func TestRegisterLargeInRangeRevisionOn64Bit(t *testing.T) {
	const large = "2147483648"
	input := `{"requests":[
		{"type":"register","service":"svc","expectedRevision":0,"instances":[{"id":"i1","address":"h1:8080"}]},
		{"type":"register","service":"svc","expectedRevision":` + large + `,"instances":[]},
		{"type":"select","service":"svc","expectedRevision":` + large + `}
	]}`
	out, code := runRegisterWith(t, input)
	if code != 1 {
		t.Fatalf("exit code: %d", code)
	}
	got := decodeRegisterOutput(t, out)
	if strconv.IntSize == 32 {
		assertRangeInvalid(t, got.Results[1], 1, large)
		assertRangeInvalid(t, got.Results[2], 1, large)
		return
	}
	wantExpected, err := strconv.Atoi(large)
	if err != nil {
		t.Fatalf("parse %s: %v", large, err)
	}
	for i := 1; i <= 2; i++ {
		r := got.Results[i]
		if r.OK || r.Error != "conflict" ||
			r.ExpectedRevision != wantExpected || r.ActualRevision != 1 || r.Revision != 1 {
			t.Fatalf("result %d should be a normal conflict preserving %s: %+v", i, large, r)
		}
	}
}
