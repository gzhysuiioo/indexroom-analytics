package indexroom

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

// TestParseExpectedRevisionRange verifies the submitted expectedRevision is
// judged by its raw value: the platform range is 0 to MaxExpectedRevision, and
// an integer beyond it or any negative is invalid, as is non-integer text.
func TestParseExpectedRevisionRange(t *testing.T) {
	max := int64(math.MaxInt)
	for _, raw := range []string{"0", "1", strconv.FormatInt(max, 10)} {
		got, err := ParseExpectedRevision(raw)
		if err != nil {
			t.Fatalf("%s rejected: %v", raw, err)
		}
		if strconv.FormatInt(int64(got), 10) != strings.TrimSpace(raw) {
			t.Fatalf("%s narrowed to %d", raw, got)
		}
	}

	outOfRange := strconv.FormatUint(uint64(math.MaxInt)+1, 10)
	cases := []struct {
		raw  string
		want string // substring the reason must carry
	}{
		{outOfRange, "expectedRevision"},
		{"9223372036854775808", "expectedRevision"},
		{"-9223372036854775809", "expectedRevision"},
		{"-1", "expectedRevision"},
		{"-2147483648", "expectedRevision"},
		{"1.5", "expectedRevision"},
		{`"1"`, "expectedRevision"},
		{"true", "expectedRevision"},
		{"", "expectedRevision"},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if _, err := ParseExpectedRevision(tc.raw); err == nil {
				t.Fatalf("%s accepted", tc.raw)
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("%s: reason %q does not name expectedRevision", tc.raw, err)
			}
		})
	}
}

// TestParseExpectedRevisionNoTruncation pins the 32-bit failure mode: values
// that a narrower int would truncate to a plausible revision must be rejected
// rather than narrowed. On a 64-bit program 4294967297 is an ordinary integer
// and must survive unchanged, never clamped to the 32-bit limit; -4294967295
// stays invalid everywhere because it is negative.
func TestParseExpectedRevisionNoTruncation(t *testing.T) {
	if _, err := ParseExpectedRevision("-4294967295"); err == nil {
		t.Fatalf("-4294967295 accepted")
	}
	got, err := ParseExpectedRevision("4294967297")
	if strconv.IntSize == 32 {
		if err == nil {
			t.Fatalf("4294967297 truncated to %d instead of rejected", got)
		}
		return
	}
	if err != nil {
		t.Fatalf("4294967297 rejected on 64-bit: %v", err)
	}
	if int64(got) != 4294967297 {
		t.Fatalf("4294967297 narrowed to %d", got)
	}
}
