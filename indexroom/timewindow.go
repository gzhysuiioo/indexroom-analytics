package indexroom

import "fmt"

// timeWindow is the canonical half-open timestamp window [start, end) of
// non-negative Unix seconds. enabled is false only for QueryTxs when both
// bounds are absent, so an unused window stays distinct from [0, end); when
// enabled, start and end satisfy start < end. QueryTimeStats always uses an
// enabled window.
type timeWindow struct {
	enabled bool
	start   int64
	end     int64
}

// timeWindowFault classifies the first way a pair of bounds breaks the window
// legality rule shared by QueryTxs and QueryTimeStats. Each entry point turns
// a fault into its own reason text, so the rule lives here once while the
// public error wording stays entry-specific.
type timeWindowFault int

const (
	windowOK timeWindowFault = iota
	windowNegativeStart
	windowNegativeEnd
	windowStartNotBelowEnd
)

// checkTimeWindow applies the single legality rule: both bounds must be
// non-negative Unix seconds and the start must be strictly below the end.
// Checks run start, then end, then width so every caller reports several
// invalid bounds in the same fixed order.
func checkTimeWindow(start, end int64) timeWindowFault {
	if start < 0 {
		return windowNegativeStart
	}
	if end < 0 {
		return windowNegativeEnd
	}
	if start >= end {
		return windowStartNotBelowEnd
	}
	return windowOK
}

// timeWindowWords maps each legality fault to one entry point's reason text.
type timeWindowWords struct {
	negativeStart string
	negativeEnd   string
	startNotBelow string
}

// errorFor wraps the entry-specific reason for fault in ErrInvalidArgument;
// windowOK must never reach it.
func (words timeWindowWords) errorFor(fault timeWindowFault) error {
	switch fault {
	case windowNegativeStart:
		return fmt.Errorf("%w: %s", ErrInvalidArgument, words.negativeStart)
	case windowNegativeEnd:
		return fmt.Errorf("%w: %s", ErrInvalidArgument, words.negativeEnd)
	default: // windowStartNotBelowEnd
		return fmt.Errorf("%w: %s", ErrInvalidArgument, words.startNotBelow)
	}
}

// requiredTimeWindow validates a window that is always present, such as
// QueryTimeStats' Start/End, reporting the first violated rule with the
// caller's wording.
func requiredTimeWindow(start, end int64, words timeWindowWords) (timeWindow, error) {
	if fault := checkTimeWindow(start, end); fault != windowOK {
		return timeWindow{}, words.errorFor(fault)
	}
	return timeWindow{enabled: true, start: start, end: end}, nil
}

// queryTimeWindowWords keeps QueryTxs' historical reason text.
var queryTimeWindowWords = timeWindowWords{
	negativeStart: "time window start must not be negative",
	negativeEnd:   "time window end must not be negative",
	startNotBelow: "time window start must be below end",
}

// normalizeTimeWindow validates QueryTxs' optional half-open window: both
// bounds must be absent together, and when present they follow the shared
// legality rule. Exactly one bound is rejected before either value is
// examined; a zero start with an explicit end is a genuine window, not the
// absent one.
func normalizeTimeWindow(startPtr, endPtr *int64) (timeWindow, error) {
	if (startPtr == nil) != (endPtr == nil) {
		return timeWindow{}, fmt.Errorf("%w: time window needs both start and end or neither", ErrInvalidArgument)
	}
	if startPtr == nil {
		return timeWindow{}, nil
	}
	return requiredTimeWindow(*startPtr, *endPtr, queryTimeWindowWords)
}

// statsTimeWindowWords keeps QueryTimeStats' historical reason text.
var statsTimeWindowWords = timeWindowWords{
	negativeStart: "start time must not be negative",
	negativeEnd:   "end time must not be negative",
	startNotBelow: "start time must be below end time",
}

// normalizeStatsTimeWindow validates QueryTimeStats' always-required window
// with that entry point's wording.
func normalizeStatsTimeWindow(start, end int64) (timeWindow, error) {
	return requiredTimeWindow(start, end, statsTimeWindowWords)
}

// contains reports whether a block timestamp survives a window: an enabled
// window rejects a missing timestamp, while start is included and end
// excluded; a real zero is judged numerically like any other value. A
// disabled window matches regardless of the timestamp. Timestamps need not be
// monotonic with height; callers order results solely by height and block
// position.
func (w timeWindow) contains(when *int64) bool {
	if !w.enabled {
		return true
	}
	if when == nil {
		return false
	}
	return *when >= w.start && *when < w.end
}
