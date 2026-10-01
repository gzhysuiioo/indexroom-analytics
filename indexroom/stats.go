package indexroom

import "fmt"

// MaxTimeSegments caps the number of segments a single time-windowed
// aggregation may return.
const MaxTimeSegments = 10000

// TimeStatsQuery describes a time-windowed transaction aggregation over the
// main chain. It reuses the height range and transaction filter of TxQuery
// but never paginates: the whole window is aggregated in one call.
type TimeStatsQuery struct {
	// From and To bound the searched heights, both inclusive, with the same
	// defaults and clamping rules as TxQuery.
	From int64
	To   int64
	// TxIDs restricts results to these identifiers, matched by exact string
	// comparison; an empty slice disables the filter. Duplicates and order
	// inside the slice are irrelevant, and the empty identifier participates
	// in matching like any other.
	TxIDs []string
	// Start and End bound the time window in Unix seconds. The window
	// includes Start and excludes End; both must be non-negative and Start
	// must be strictly smaller than End.
	Start int64
	End   int64
	// Segment is the segment length in seconds; it must be positive. The
	// first segment starts at Start, every later segment follows on the same
	// grid, and the last segment is truncated at End.
	Segment int64
}

// TimeSegment is one segment of a time-windowed aggregation.
type TimeSegment struct {
	// Start is the inclusive lower bound of the segment (Unix seconds).
	Start int64
	// End is the exclusive upper bound; the final segment is truncated at
	// the requested window end.
	End int64
	// TxCount is the number of matching transaction occurrences in the
	// segment: the same identifier counts once per occurrence, including
	// duplicates inside one block.
	TxCount int64
	// UniqueTxIDs is the number of distinct matching identifiers in the
	// segment.
	UniqueTxIDs int64
	// MatchedBlocks is the number of blocks in the segment holding at least
	// one matching transaction.
	MatchedBlocks int64
}

// TimeStats is the result of a time-windowed aggregation.
type TimeStats struct {
	// Segments covers the requested window in ascending time order. Every
	// segment is present even when it holds no matching transaction.
	Segments []TimeSegment
	// Summary aggregates the whole window with the same metrics as one
	// segment; its Start and End are the requested window bounds. UniqueTxIDs
	// is deduplicated over the whole window, not summed across segments.
	Summary TimeSegment
	// Untimestamped counts the blocks inside the height range that carry no
	// timestamp. Such blocks enter no segment; the count is unaffected by
	// the transaction filter.
	Untimestamped int64
}

// QueryTimeStats aggregates transaction occurrences over a fixed time window
// segmented on a fixed grid. Every call observes one complete chain state:
// concurrent Append, Reorg, and Restore calls never mix old and new blocks,
// and the returned value is a copy the caller may freely mutate. The query
// itself never modifies the index.
//
// The window [Start, End) is split into segments of Segment seconds starting
// at Start; the last segment is truncated at End. All segments are returned
// in ascending order, including empty ones. Blocks without a timestamp are
// excluded from every segment and counted in Untimestamped. On empty chains,
// height ranges above the tip, or filters with no matches, the result still
// carries the requested zero-valued segments.
func (index *Index) QueryTimeStats(query TimeStatsQuery) (TimeStats, error) {
	from, to, err := normalizeRange(query.From, query.To)
	if err != nil {
		return TimeStats{}, err
	}
	if query.Start < 0 {
		return TimeStats{}, fmt.Errorf("%w: window start must be non-negative", ErrInvalidArgument)
	}
	if query.End < 0 {
		return TimeStats{}, fmt.Errorf("%w: window end must be non-negative", ErrInvalidArgument)
	}
	if query.Start >= query.End {
		return TimeStats{}, fmt.Errorf("%w: window start must be before end", ErrInvalidArgument)
	}
	if query.Segment <= 0 {
		return TimeStats{}, fmt.Errorf("%w: segment seconds must be positive", ErrInvalidArgument)
	}
	// span cannot overflow: both bounds are non-negative and end > start.
	span := query.End - query.Start
	n := span / query.Segment
	if span%query.Segment != 0 {
		n++
	}
	if n > MaxTimeSegments {
		return TimeStats{}, fmt.Errorf("%w: window needs %d segments, limit is %d",
			ErrInvalidArgument, n, MaxTimeSegments)
	}

	index.mu.Lock()
	defer index.mu.Unlock()

	if to == 0 || to > index.Tip {
		to = index.Tip
	}
	stats := TimeStats{
		Segments: make([]TimeSegment, n),
		Summary:  TimeSegment{Start: query.Start, End: query.End},
	}
	for i := range stats.Segments {
		// (n-1)*segment <= span, so the product never overflows; the
		// addition is checked for the truncated last segment.
		segStart := query.Start + int64(i)*query.Segment
		segEnd := segStart + query.Segment
		if segEnd < segStart || segEnd > query.End {
			segEnd = query.End
		}
		stats.Segments[i] = TimeSegment{Start: segStart, End: segEnd}
	}
	if from > to {
		// Empty index or a start above the tip: the requested zero segments
		// are the whole result.
		return stats, nil
	}

	var filter map[string]struct{}
	if len(query.TxIDs) > 0 {
		filter = make(map[string]struct{}, len(query.TxIDs))
		for _, id := range query.TxIDs {
			filter[id] = struct{}{}
		}
	}

	segIDs := make([]map[string]struct{}, n)
	for i := range segIDs {
		segIDs[i] = make(map[string]struct{})
	}
	summaryIDs := make(map[string]struct{})

	for height := from; height <= to; height++ {
		block := index.Blocks[height]
		if block.Timestamp == nil {
			stats.Untimestamped++
			continue
		}
		ts := *block.Timestamp
		if ts < query.Start || ts >= query.End {
			continue
		}
		idx := (ts - query.Start) / query.Segment
		seg := &stats.Segments[idx]
		matched := false
		for _, tx := range block.Txs {
			if filter != nil {
				if _, ok := filter[tx]; !ok {
					continue
				}
			}
			seg.TxCount++
			segIDs[idx][tx] = struct{}{}
			summaryIDs[tx] = struct{}{}
			matched = true
		}
		if matched {
			seg.MatchedBlocks++
		}
	}

	for i := range stats.Segments {
		stats.Segments[i].UniqueTxIDs = int64(len(segIDs[i]))
	}
	stats.Summary.TxCount = sumTxCounts(stats.Segments)
	stats.Summary.UniqueTxIDs = int64(len(summaryIDs))
	stats.Summary.MatchedBlocks = sumMatchedBlocks(stats.Segments)
	return stats, nil
}

func sumTxCounts(segments []TimeSegment) int64 {
	var total int64
	for i := range segments {
		total += segments[i].TxCount
	}
	return total
}

func sumMatchedBlocks(segments []TimeSegment) int64 {
	var total int64
	for i := range segments {
		total += segments[i].MatchedBlocks
	}
	return total
}
