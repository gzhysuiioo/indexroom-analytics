package indexroom

import (
	"reflect"
	"testing"
)

// filteredStatsChain builds the shared fixture for QueryTimeStats TxIDs
// filtering regressions:
//
//	height 1, time  0: ["a", "a", "", "A", " a "]
//	height 2, time 10: ["", "a"]
//	height 3, no time: ["a", "", "zzz"]
//	height 4, time 20: ["x"]
//
// Over window [0,20) step 10, height 1 lands in bucket [0,10) and height 2
// in [10,20); height 3 has no timestamp and must only surface in
// MissingTimeBlocks even though it carries matching identifiers; height 4's
// timestamp equals the excluded window end. The fixture deliberately keeps
// case ("a" vs "A"), surrounding whitespace (" a "), the empty identifier,
// and a repeated in-block identifier side by side.
func filteredStatsChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	blocks := []Block{
		timeBlock(1, "h1", "g", []string{"a", "a", "", "A", " a "}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"", "a"}, intptr(10)),
		timeBlock(3, "h3", "h2", []string{"a", "", "zzz"}, nil),
		timeBlock(4, "h4", "h3", []string{"x"}, intptr(20)),
	}
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

func filteredStatsQuery(txIDs []string) TimeStatsQuery {
	return TimeStatsQuery{TxIDs: txIDs, Start: 0, End: 20, StepSeconds: 10}
}

// TestQueryTimeStatsNilEmptyAndSoleEmptyStringFilter pins the three ways to
// supply TxIDs: a nil list and an empty list both count every transaction,
// including empty identifiers; the one-element list [""] counts only
// empty-identifier transactions.
func TestQueryTimeStatsNilEmptyAndSoleEmptyStringFilter(t *testing.T) {
	index := filteredStatsChain(t)

	// No identifier given and an explicitly empty list are the same
	// unrestricted query: every occurrence counts, the empty identifier in
	// both timed blocks included.
	allWant := TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 5, DistinctTxIDs: 4, Blocks: 1},
			{Start: 10, End: 20, TxCount: 2, DistinctTxIDs: 2, Blocks: 1},
		},
		Totals:            TimeTotals{TxCount: 7, DistinctTxIDs: 4, Blocks: 2},
		FromHeight:        1,
		ToHeight:          4,
		MissingTimeBlocks: 1,
	}
	for name, txIDs := range map[string][]string{
		"nil TxIDs":   nil,
		"empty TxIDs": {},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := index.QueryTimeStats(filteredStatsQuery(txIDs))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, allWant) {
				t.Fatalf("stats=%+v\nwant=%+v", got, allWant)
			}
		})
	}

	// [""] is a real filter matching only the empty identifier, exactly once
	// in each timed block: one occurrence, one distinct id, one block per
	// bucket; two occurrences but still one distinct id over the window.
	got, err := index.QueryTimeStats(filteredStatsQuery([]string{""}))
	if err != nil {
		t.Fatal(err)
	}
	emptyWant := TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
			{Start: 10, End: 20, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
		},
		Totals:            TimeTotals{TxCount: 2, DistinctTxIDs: 1, Blocks: 2},
		FromHeight:        1,
		ToHeight:          4,
		MissingTimeBlocks: 1,
	}
	if !reflect.DeepEqual(got, emptyWant) {
		t.Fatalf("stats=%+v\nwant=%+v", got, emptyWant)
	}
}

// TestQueryTimeStatsFilterExactStringComparison pins exact-string semantics on
// the returned counters: case, leading/trailing whitespace, and string
// boundaries all matter.
func TestQueryTimeStatsFilterExactStringComparison(t *testing.T) {
	index := New()
	blocks := []Block{
		timeBlock(1, "h1", "g", []string{"a", "A", " a ", "a ", " a", "ab", "a"}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"a", "ab"}, intptr(5)),
	}
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	query := TimeStatsQuery{Start: 0, End: 10, StepSeconds: 10}

	// Each spelling matches only its own occurrences; "a" appears twice in
	// block 1 and once in block 2, and "ab" must never be swallowed by "a".
	cases := []struct {
		name     string
		txIDs    []string
		txCount  int64
		distinct int64
		blocks   int64
	}{
		{"lowercase a", []string{"a"}, 3, 1, 2},
		{"uppercase A", []string{"A"}, 1, 1, 1},
		{"padded a", []string{" a "}, 1, 1, 1},
		{"trailing space", []string{"a "}, 1, 1, 1},
		{"leading space", []string{" a"}, 1, 1, 1},
		{"boundary ab", []string{"ab"}, 2, 1, 2},
		{"whitespace is not empty", []string{" "}, 0, 0, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			query.TxIDs = tc.txIDs
			got, err := index.QueryTimeStats(query)
			if err != nil {
				t.Fatal(err)
			}
			b := got.Buckets[0]
			if b.TxCount != tc.txCount || b.DistinctTxIDs != tc.distinct || b.Blocks != tc.blocks {
				t.Fatalf("filter %q bucket=%+v, want tx=%d distinct=%d blocks=%d",
					tc.txIDs, b, tc.txCount, tc.distinct, tc.blocks)
			}
			if got.Totals != (TimeTotals{TxCount: tc.txCount, DistinctTxIDs: tc.distinct, Blocks: tc.blocks}) {
				t.Fatalf("filter %q totals=%+v", tc.txIDs, got.Totals)
			}
		})
	}

	// The three easily-confused spellings are three distinct window ids,
	// never merged by case folding or trimming.
	query.TxIDs = []string{"a", "A", " a "}
	got, err := index.QueryTimeStats(query)
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals.DistinctTxIDs != 3 || got.Totals.TxCount != 5 {
		t.Fatalf("distinct spellings merged: totals=%+v", got.Totals)
	}
}

// TestQueryTimeStatsFilterOrderAndDuplicatesEquivalent pins that reordering or
// repeating filter members changes neither per-bucket nor whole-window
// counters, while the caller's slice keeps its exact contents and order.
func TestQueryTimeStatsFilterOrderAndDuplicatesEquivalent(t *testing.T) {
	index := filteredStatsChain(t)

	spellings := [][]string{
		{"a", ""},
		{"", "a"},
		{"", "a", "a"},
		{"a", "", "a", "", "a"},
		{"a", "a", "", ""},
	}
	want := TimeStats{
		Buckets: []TimeBucket{
			// Height 1: "a", "a", "" match -> 3 occurrences, 2 ids, 1 block.
			{Start: 0, End: 10, TxCount: 3, DistinctTxIDs: 2, Blocks: 1},
			// Height 2: "" and "a" match -> 2 occurrences, 2 ids, 1 block.
			{Start: 10, End: 20, TxCount: 2, DistinctTxIDs: 2, Blocks: 1},
		},
		// 5 occurrences over the window but only the two ids "a" and "".
		Totals:            TimeTotals{TxCount: 5, DistinctTxIDs: 2, Blocks: 2},
		FromHeight:        1,
		ToHeight:          4,
		MissingTimeBlocks: 1,
	}
	for _, in := range spellings {
		saved := append([]string{}, in...)
		got, err := index.QueryTimeStats(filteredStatsQuery(in))
		if err != nil {
			t.Fatalf("filter %q: %v", in, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("filter %q stats=%+v\nwant=%+v", in, got, want)
		}
		if !reflect.DeepEqual(in, saved) {
			t.Fatalf("caller list mutated: got %q, want %q", in, saved)
		}
	}

	// Repeating the sole member is equivalent to the one-member set.
	single, err := index.QueryTimeStats(filteredStatsQuery([]string{"a"}))
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := index.QueryTimeStats(filteredStatsQuery([]string{"a", "a", "a"}))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(single, repeated) {
		t.Fatalf("[a] and [a,a,a] differ:\n%+v\n%+v", single, repeated)
	}
	singleWant := TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 2, DistinctTxIDs: 1, Blocks: 1},
			{Start: 10, End: 20, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
		},
		Totals:            TimeTotals{TxCount: 3, DistinctTxIDs: 1, Blocks: 2},
		FromHeight:        1,
		ToHeight:          4,
		MissingTimeBlocks: 1,
	}
	if !reflect.DeepEqual(single, singleWant) {
		t.Fatalf("single-id stats=%+v\nwant=%+v", single, singleWant)
	}
}

// TestQueryTimeStatsDuplicateOccurrencesBlockOnceAndDistinctDedup is the
// canonical counting scenario: repeated identifiers inside one block count by
// occurrence for TxCount, a matching block counts once, and distinct ids are
// deduplicated both inside a segment (across blocks) and over the window.
func TestQueryTimeStatsDuplicateOccurrencesBlockOnceAndDistinctDedup(t *testing.T) {
	// Two blocks in the same segment share identifiers: occurrences and
	// blocks both rise, but the per-segment distinct count stays deduplicated.
	index := New()
	blocks := []Block{
		timeBlock(1, "h1", "g", []string{"a", "b", "a"}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"b", "b", "a"}, intptr(5)),
		timeBlock(3, "h3", "h2", []string{"a"}, intptr(10)),
	}
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	got, err := index.QueryTimeStats(TimeStatsQuery{
		TxIDs: []string{"a", "b", "a"}, Start: 0, End: 20, StepSeconds: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := TimeStats{
		Buckets: []TimeBucket{
			// Segment [0,10): 3+3=6 occurrences over 2 blocks, 2 distinct ids.
			{Start: 0, End: 10, TxCount: 6, DistinctTxIDs: 2, Blocks: 2},
			{Start: 10, End: 20, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
		},
		// "a" and "b" both recur across segments, so window distinct is 2,
		// not the per-bucket sum of 3.
		Totals:     TimeTotals{TxCount: 7, DistinctTxIDs: 2, Blocks: 3},
		FromHeight: 1,
		ToHeight:   3,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stats=%+v\nwant=%+v", got, want)
	}

	// The task's worked example verbatim: filter [""] over the two-block
	// fixture gives one occurrence / one id / one block per segment and two
	// occurrences / one id / two blocks over the window; ["a",""] (in either
	// order, with or without repeats) gives 3 and 2 occurrences and two
	// distinct window ids.
	example := filteredStatsChain(t)
	emptyOnly, err := example.QueryTimeStats(filteredStatsQuery([]string{""}))
	if err != nil {
		t.Fatal(err)
	}
	if emptyOnly.Buckets[0] != (TimeBucket{Start: 0, End: 10, TxCount: 1, DistinctTxIDs: 1, Blocks: 1}) ||
		emptyOnly.Buckets[1] != (TimeBucket{Start: 10, End: 20, TxCount: 1, DistinctTxIDs: 1, Blocks: 1}) {
		t.Fatalf("[\"\"] buckets=%+v", emptyOnly.Buckets)
	}
	if emptyOnly.Totals != (TimeTotals{TxCount: 2, DistinctTxIDs: 1, Blocks: 2}) {
		t.Fatalf("[\"\"] totals=%+v", emptyOnly.Totals)
	}
	for _, txIDs := range [][]string{{"a", ""}, {"", "a", "a"}} {
		stats, err := example.QueryTimeStats(filteredStatsQuery(txIDs))
		if err != nil {
			t.Fatal(err)
		}
		if stats.Buckets[0].TxCount != 3 || stats.Buckets[1].TxCount != 2 {
			t.Fatalf("filter %q tx counts=%d,%d, want 3,2", txIDs, stats.Buckets[0].TxCount, stats.Buckets[1].TxCount)
		}
		if stats.Totals.DistinctTxIDs != 2 {
			t.Fatalf("filter %q window distinct=%d, want 2", txIDs, stats.Totals.DistinctTxIDs)
		}
	}
}

// TestQueryTimeStatsMissingTimeBlocksUnchangedByFilter pins that blocks
// without a timestamp stay out of every bucket (even when they hold matching
// transactions) while MissingTimeBlocks is counted over the resolved height
// range regardless of the filter.
func TestQueryTimeStatsMissingTimeBlocksUnchangedByFilter(t *testing.T) {
	// Height 3 lacks a time yet holds matches for every filter below.
	index := filteredStatsChain(t)

	for name, txIDs := range map[string][]string{
		"unrestricted":   nil,
		"empty id":       {""},
		"a and empty":    {"a", ""},
		"no chain match": {"zzz"},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := index.QueryTimeStats(filteredStatsQuery(txIDs))
			if err != nil {
				t.Fatal(err)
			}
			if got.MissingTimeBlocks != 1 {
				t.Fatalf("MissingTimeBlocks=%d, want 1", got.MissingTimeBlocks)
			}
			// The untimed block must never contribute a block to a bucket,
			// including for filters it matches.
			for i, b := range got.Buckets {
				if b.Blocks > 1 {
					t.Fatalf("bucket %d counted the untimed block: %+v", i, b)
				}
			}
			if got.Totals.Blocks > 2 {
				t.Fatalf("window totals counted the untimed block: %+v", got.Totals)
			}
		})
	}

	// Restricting the height range to the untimed block alone still reports
	// it as missing, while no segment receives anything.
	ranged, err := index.QueryTimeStats(TimeStatsQuery{
		From: 3, To: 3, TxIDs: []string{"a", ""}, Start: 0, End: 20, StepSeconds: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ranged.FromHeight != 3 || ranged.ToHeight != 3 || ranged.MissingTimeBlocks != 1 {
		t.Fatalf("ranged resolution/missing=%+v", ranged)
	}
	for i, b := range ranged.Buckets {
		if b != (TimeBucket{Start: b.Start, End: b.End}) {
			t.Fatalf("bucket %d carries data from an untimed block: %+v", i, b)
		}
	}
	if ranged.Totals != (TimeTotals{}) {
		t.Fatalf("totals=%+v, want zero", ranged.Totals)
	}
}

// TestQueryTimeStatsNoMatchReturnsFullZeroedWindow pins that a filter with no
// match on chain is a successful query: buckets still cover the whole
// requested window (including a truncated final segment) with every counter
// at zero, while MissingTimeBlocks follows the selected height range.
func TestQueryTimeStatsNoMatchReturnsFullZeroedWindow(t *testing.T) {
	index := filteredStatsChain(t)

	got, err := index.QueryTimeStats(TimeStatsQuery{
		TxIDs: []string{"zzz"}, Start: 0, End: 20, StepSeconds: 10,
	})
	if err != nil {
		t.Fatalf("no-match query failed: %v", err)
	}
	if got.FromHeight != 1 || got.ToHeight != 4 || got.MissingTimeBlocks != 1 {
		t.Fatalf("resolved range/missing=%+v", got)
	}
	wantBounds := [][2]int64{{0, 10}, {10, 20}}
	if len(got.Buckets) != len(wantBounds) {
		t.Fatalf("buckets=%d, want %d", len(got.Buckets), len(wantBounds))
	}
	for i, b := range got.Buckets {
		if b.Start != wantBounds[i][0] || b.End != wantBounds[i][1] {
			t.Fatalf("bucket %d bounds=[%d,%d), want [%d,%d)",
				i, b.Start, b.End, wantBounds[i][0], wantBounds[i][1])
		}
		if b.TxCount != 0 || b.DistinctTxIDs != 0 || b.Blocks != 0 {
			t.Fatalf("bucket %d non-zero without matches: %+v", i, b)
		}
	}
	if got.Totals != (TimeTotals{}) {
		t.Fatalf("totals=%+v, want zero", got.Totals)
	}

	// Full coverage persists with a truncated final bucket and a height
	// range that starts on the untimed block; height 4 at time 20 sits in
	// the last segment but its "x" does not match, so it stays zero.
	truncated, err := index.QueryTimeStats(TimeStatsQuery{
		TxIDs: []string{"zzz"}, From: 2, Start: 0, End: 25, StepSeconds: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	truncBounds := [][2]int64{{0, 10}, {10, 20}, {20, 25}}
	if len(truncated.Buckets) != 3 {
		t.Fatalf("buckets=%d, want 3", len(truncated.Buckets))
	}
	for i, b := range truncated.Buckets {
		if b.Start != truncBounds[i][0] || b.End != truncBounds[i][1] {
			t.Fatalf("bucket %d bounds=[%d,%d), want [%d,%d)",
				i, b.Start, b.End, truncBounds[i][0], truncBounds[i][1])
		}
		if b.TxCount != 0 || b.DistinctTxIDs != 0 || b.Blocks != 0 {
			t.Fatalf("bucket %d non-zero: %+v", i, b)
		}
	}
	if truncated.Totals != (TimeTotals{}) {
		t.Fatalf("totals=%+v, want zero", truncated.Totals)
	}
	if truncated.FromHeight != 2 || truncated.ToHeight != 4 || truncated.MissingTimeBlocks != 1 {
		t.Fatalf("resolved range/missing=%+v", truncated)
	}

	// A multi-member filter where nothing exists on chain at all.
	none, err := index.QueryTimeStats(TimeStatsQuery{
		TxIDs: []string{"nope", "nada", ""}, Start: 0, End: 20, StepSeconds: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	// The empty id DOES exist, so this filter matches the two empty-id
	// occurrences; a truly absent set must not include "".
	if none.Totals.TxCount != 2 {
		t.Fatalf("filter containing \"\" must match empty ids: totals=%+v", none.Totals)
	}
	absent, err := index.QueryTimeStats(TimeStatsQuery{
		TxIDs: []string{"nope", "nada"}, Start: 0, End: 20, StepSeconds: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if absent.Totals != (TimeTotals{}) || absent.MissingTimeBlocks != 1 {
		t.Fatalf("absent filter result=%+v", absent)
	}
}
