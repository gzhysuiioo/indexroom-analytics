package indexroom

import (
	"fmt"
)

// MaxTimeBuckets is the largest number of segments one QueryTimeStats call
// may return.
const MaxTimeBuckets = 10000

// statsQueryHookLocked is a test-only rendezvous invoked once per
// QueryTimeStats call while index.mu is held, with the resolved tip-bound
// height (after clamping, before any block is scanned). It is nil in
// production; reorg regression tests set it to park a query that has already
// pinned its height range with the lock held, forcing a concurrent Reorg to
// wait and making the "one complete main-chain state" observation
// deterministic instead of relying on scheduling luck.
var statsQueryHookLocked func(resolvedTo int64)

// TimeStatsQuery asks for transaction statistics over a half-open time
// window, segmented into fixed-length buckets.
//
// From and To reuse the TxQuery height semantics: heights are inclusive, a
// zero From starts at height 1, a zero To pins the chain tip, and an
// explicit To above the tip is clamped to it. TxIDs reuses the exact-string
// filter semantics, including duplicates and empty identifiers.
//
// Start and End are Unix seconds bounding the window [Start, End): Start is
// included, End is excluded. Both must be non-negative and Start must be
// below End. StepSeconds is the bucket length in seconds and must be
// positive; buckets start at Start and the final bucket is cut off at End.
// Times outside any int64 range cannot be requested.
type TimeStatsQuery struct {
	From        int64
	To          int64
	TxIDs       []string
	Start       int64
	End         int64
	StepSeconds int64
}

// TimeBucket is statistics for one half-open segment [Start, End). Buckets
// with no matching transactions are returned with every count at zero.
type TimeBucket struct {
	Start int64
	End   int64
	// TxCount counts matching transaction occurrences in the segment.
	TxCount int64
	// DistinctTxIDs counts the different matching identifiers in the segment.
	DistinctTxIDs int64
	// Blocks counts blocks whose timestamp falls in the segment and which
	// hold at least one matching transaction.
	Blocks int64
}

// TimeTotals summarizes the whole window with the same counters as a bucket.
// DistinctTxIDs is deduplicated over the whole window, not summed from the
// per-bucket values.
type TimeTotals struct {
	TxCount       int64
	DistinctTxIDs int64
	Blocks        int64
}

// TimeStats is the result of QueryTimeStats. Buckets are ordered by ascending
// time and always cover the whole requested window, even on an empty chain,
// above the chain tip, or when nothing matches. MissingTimeBlocks reports how
// many blocks in the resolved height range carry no timestamp; those blocks
// never enter a bucket, and the count ignores the transaction filter.
type TimeStats struct {
	Buckets []TimeBucket
	Totals  TimeTotals
	// FromHeight and ToHeight are the resolved inclusive height bounds,
	// clamped to the chain tip observed by this call.
	FromHeight int64
	ToHeight   int64
	// MissingTimeBlocks counts blocks in [FromHeight, ToHeight] without a
	// timestamp, regardless of the transaction filter.
	MissingTimeBlocks int64
}

// QueryTimeStats buckets matching transactions by block timestamp over the
// requested height range. The call observes one complete main-chain state:
// concurrent Append, Reorg, or Restore calls never mix old and new blocks
// into the answer, and the index is never modified.
func (index *Index) QueryTimeStats(query TimeStatsQuery) (TimeStats, error) {
	from, to, err := normalizeRange(query.From, query.To)
	if err != nil {
		return TimeStats{}, err
	}
	// The window is always required for statistics and follows the same
	// legality rule as QueryTxs' optional one; only the reason wording is
	// entry-specific.
	window, err := normalizeStatsTimeWindow(query.Start, query.End)
	if err != nil {
		return TimeStats{}, err
	}
	if query.StepSeconds <= 0 {
		return TimeStats{}, fmt.Errorf("%w: step seconds must be positive", ErrInvalidArgument)
	}

	// Window length is positive and bounded by MaxInt64 since End > Start.
	width := query.End - query.Start
	count := width / query.StepSeconds
	if width%query.StepSeconds != 0 {
		count++
	}
	if count > MaxTimeBuckets {
		return TimeStats{}, fmt.Errorf("%w: time window would produce %d buckets, limit is %d",
			ErrInvalidArgument, count, MaxTimeBuckets)
	}

	// One canonical filter backs the statistics scan, so matching and the
	// empty-list-vs-{""} distinction behave exactly as in QueryTxs.
	filter := newTxFilter(query.TxIDs)

	stats := TimeStats{Buckets: make([]TimeBucket, count)}
	for i := range stats.Buckets {
		stats.Buckets[i].Start = bucketBoundary(query.Start, query.StepSeconds, int64(i))
		// A saturated boundary (MaxInt64) and the overshooting final step
		// are both cut back to the requested window end.
		end := bucketBoundary(query.Start, query.StepSeconds, int64(i)+1)
		if end > query.End {
			end = query.End
		}
		stats.Buckets[i].End = end
	}

	index.mu.Lock()
	defer index.mu.Unlock()

	if to == 0 || to > index.Tip {
		to = index.Tip
	}
	stats.FromHeight = from
	stats.ToHeight = to
	if from > to {
		// Empty index or a start above the tip: zeroed buckets are still
		// returned for the whole requested window.
		return stats, nil
	}

	if statsQueryHookLocked != nil {
		statsQueryHookLocked(to)
	}

	distinct := make([]map[string]struct{}, count)
	windowDistinct := make(map[string]struct{})
	for height := from; height <= to; height++ {
		block := index.Blocks[height]
		if block.Time == nil {
			// A missing timestamp never hits the window, yet it is still
			// reported over the resolved height range regardless of the
			// transaction filter.
			stats.MissingTimeBlocks++
			continue
		}
		t := *block.Time
		if !window.contains(block.Time) {
			continue
		}
		bucket := (t - query.Start) / query.StepSeconds
		matched := false
		for _, tx := range block.Txs {
			if !filter.matches(tx) {
				continue
			}
			stats.Buckets[bucket].TxCount++
			stats.Totals.TxCount++
			if distinct[bucket] == nil {
				distinct[bucket] = make(map[string]struct{})
			}
			distinct[bucket][tx] = struct{}{}
			windowDistinct[tx] = struct{}{}
			matched = true
		}
		if matched {
			stats.Buckets[bucket].Blocks++
			stats.Totals.Blocks++
		}
	}
	for i := range distinct {
		stats.Buckets[i].DistinctTxIDs = int64(len(distinct[i]))
	}
	stats.Totals.DistinctTxIDs = int64(len(windowDistinct))
	return stats, nil
}

// bucketBoundary returns Start + step*index, saturated at MaxInt64 instead
// of overflowing. Callers cut saturated and trailing boundaries back to the
// requested window end.
func bucketBoundary(start, step, index int64) int64 {
	offset := saturatingMul(step, index)
	if offset > 1<<63-1-start {
		return 1<<63 - 1
	}
	return start + offset
}

// saturatingMul multiplies non-negative a and b, saturating at MaxInt64.
func saturatingMul(a, b int64) int64 {
	if a == 0 || b == 0 {
		return 0
	}
	if a > (1<<63-1)/b {
		return 1<<63 - 1
	}
	return a * b
}
