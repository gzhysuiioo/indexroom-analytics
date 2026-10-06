package indexroom

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestParseExpectedRevision locks the raw-token parser shared by every request
// kind. It establishes integer type only; the architecture-specific upper
// bound is enforced later by validateExpectedRevision, so negatives still parse
// here and are rejected at validation.
func TestParseExpectedRevision(t *testing.T) {
	cases := []struct {
		text string
		want int64
		err  string // "" means no error expected
	}{
		{"0", 0, ""},
		{"1", 1, ""},
		{"2147483647", 2147483647, ""},
		{"2147483648", 2147483648, ""},
		{"9223372036854775807", 9223372036854775807, ""},
		// Negatives parse as integers; the non-negative rule belongs to the
		// range check so its reason can state the full range.
		{"-1", -1, ""},
		{"-4294967295", -4294967295, ""},
		// Non-integer tokens get the integer-type error, quoting as submitted.
		{"1.5", 0, "expectedRevision must be an integer, got 1.5"},
		{"1e3", 0, "expectedRevision must be an integer, got 1e3"},
		{`"1"`, 0, "expectedRevision must be an integer"},
		{"true", 0, "expectedRevision must be an integer"},
		// A decimal point or exponent makes the token a format problem no matter
		// how long (or how small) its integer part is: ParseInt reports ErrRange
		// for these, but they are not written as integers and must keep the
		// integer-type reason quoting the raw text.
		{"18446744073709551616.0", 0, "expectedRevision must be an integer, got 18446744073709551616.0"},
		{"18446744073709551616e-20", 0, "expectedRevision must be an integer, got 18446744073709551616e-20"},
		{"18446744073709551616E3", 0, "expectedRevision must be an integer, got 18446744073709551616E3"},
		{"9223372036854775807.0", 0, "expectedRevision must be an integer, got 9223372036854775807.0"},
		{"0.0", 0, "expectedRevision must be an integer, got 0.0"},
		{"+1", 0, "expectedRevision must be an integer, got +1"},
		// A magnitude above int64 cannot be carried numerically; the raw text is
		// reported as an out-of-range expectedRevision, not an overflowed value.
		{"9223372036854775808", 0, "expectedRevision must be an integer between 0 and"},
		{"-9223372036854775809", 0, "expectedRevision must be an integer between 0 and"},
	}
	for _, tc := range cases {
		t.Run(tc.text, func(t *testing.T) {
			got, err := ParseExpectedRevision(tc.text)
			if tc.err == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if got != tc.want {
					t.Fatalf("got %d want %d", got, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Fatalf("got error %v, want substring %q", err, tc.err)
			}
		})
	}
}

// revisionRangeBound is the accepted upper bound for the test build's
// architecture, mirroring maxRevision.
func revisionRangeBound() int64 {
	if strconv.IntSize == 32 {
		return math.MaxInt32
	}
	return math.MaxInt64
}

// TestValidateExpectedRevisionRange checks the raw int64 value before it is
// narrowed to int. The decisive case only exists on a 32-bit build: values that
// fit int64 but wrap when converted to int (4294967297 -> 1) must be rejected.
// On a 64-bit build that value is a legitimate integer and proceeds to the
// ordinary revision comparison.
func TestValidateExpectedRevisionRange(t *testing.T) {
	bound := revisionRangeBound()

	if err := validateExpectedRevision(0); err != nil {
		t.Fatalf("0 should be valid: %v", err)
	}
	if err := validateExpectedRevision(bound); err != nil {
		t.Fatalf("upper bound %d should be valid: %v", bound, err)
	}

	wantRange := func(t *testing.T, err error) {
		t.Helper()
		if err == nil {
			t.Fatalf("expected an invalid error")
		}
		msg := err.Error()
		if !strings.Contains(msg, "expectedRevision must be an integer between 0 and") {
			t.Fatalf("reason should state the expectedRevision range, got %q", msg)
		}
		if !strings.Contains(msg, "got ") {
			t.Fatalf("reason should report the submitted value, got %q", msg)
		}
	}

	// Negatives are invalid on every architecture with the range reason.
	wantRange(t, validateExpectedRevision(-1))

	if strconv.IntSize == 32 {
		// The two wrapping values from the bug report must not reach the
		// revision comparison as 1; the integer just above the bound is the
		// smallest value int cannot carry.
		for _, n := range []int64{4294967297, -4294967295, bound + 1} {
			wantRange(t, validateExpectedRevision(n))
		}
	}
}

// TestRegistrationRejectsOutOfRangeRawRevision drives all three Validate*
// entry points with a raw int64 the architecture's int cannot represent. On a
// 32-bit build every one must fail validation (returning its zero value) before
// any revision comparison, so the caller can never Apply, ApplyHealth or
// Select with a wrapped revision.
func TestRegistrationRejectsOutOfRangeRawRevision(t *testing.T) {
	if strconv.IntSize != 32 {
		// On 64-bit int64 cannot express an in-type overflow; the
		// beyond-int64 raw text is covered through ParseExpectedRevision and
		// the command-level tests.
		t.Skip("raw int64 overflow of int is only constructible on a 32-bit build")
	}
	r := NewRegistry()
	reg, _ := r.ValidateRegistration("svc", 0, []Instance{{ID: "i1", Address: "h1:1"}})
	r.Apply(reg)
	upd0, _ := r.ValidateHealth("svc", "i1", 1, 1, true, "")
	r.ApplyHealth(upd0)

	for _, n := range []int64{4294967297, -4294967295} {
		if reg, err := r.ValidateRegistration("svc", n, []Instance{{ID: "i1", Address: "h9:9"}}); err == nil {
			t.Fatalf("register revision %d should be invalid, got %+v", n, reg)
		} else if reg.Service != "" || reg.Revision != 0 || reg.Instances != nil {
			t.Fatalf("failed register validation returned a partial value: %+v", reg)
		}
		if upd, err := r.ValidateHealth("svc", "i1", n, 2, true, ""); err == nil {
			t.Fatalf("health revision %d should be invalid, got %+v", n, upd)
		}
		if sel, err := r.ValidateSelection("svc", n); err == nil {
			t.Fatalf("select revision %d should be invalid, got %+v", n, sel)
		}
		if sel, err := r.ValidateSelectionWithSession("svc", n, strPtr("s")); err == nil {
			t.Fatalf("session select revision %d should be invalid, got %+v", n, sel)
		}
	}

	// Nothing the rejected items proposed took effect: revision, address and
	// health all stand at the state established beforehand.
	views := r.Snapshot()
	if len(views) != 1 || views[0].Revision != 1 {
		t.Fatalf("state changed after rejected raw revisions: %+v", views)
	}
	inst := views[0].Instances[0]
	if inst.Address != "h1:1" || inst.Health != HealthHealthy || inst.Sequence != 1 {
		t.Fatalf("rejected raw revisions must not change state: %+v", inst)
	}
}

// TestInRangeLargeRevisionKeepsConflictSemantics locks the 64-bit behavior:
// 2147483648 is a perfectly legal integer there, so for a service at revision
// 1 it must pass validation and produce an ordinary conflict carrying the
// original expected value — the fix must not clamp 64-bit to the 32-bit bound.
func TestInRangeLargeRevisionKeepsConflictSemantics(t *testing.T) {
	r := NewRegistry()
	reg, _ := r.ValidateRegistration("svc", 0, []Instance{{ID: "i1", Address: "h1:1"}})
	r.Apply(reg)

	// large is a variable so narrowing it to int is a runtime conversion that
	// only executes on the 64-bit path.
	large := int64(2147483648)
	if strconv.IntSize == 32 {
		if _, err := r.ValidateRegistration("svc", large, nil); err == nil {
			t.Fatalf("2147483648 must be out of range on a 32-bit build")
		}
		if _, err := r.ValidateSelection("svc", large); err == nil {
			t.Fatalf("2147483648 select must be out of range on a 32-bit build")
		}
		return
	}

	reg, err := r.ValidateRegistration("svc", large, nil)
	if err != nil {
		t.Fatalf("2147483648 should validate on 64-bit: %v", err)
	}
	if out := r.Apply(reg); out.OK || out.Kind != OutcomeConflict ||
		out.Expected != int(large) || out.Actual != 1 || out.Revision != 1 {
		t.Fatalf("large in-range revision should conflict preserving the value: %+v", out)
	}
	sel, err := r.ValidateSelection("svc", large)
	if err != nil {
		t.Fatalf("select validation: %v", err)
	}
	if out := r.Select(sel); out.OK || out.Kind != OutcomeConflict ||
		out.Expected != int(large) || out.Actual != 1 {
		t.Fatalf("large in-range select should conflict preserving the value: %+v", out)
	}
}
