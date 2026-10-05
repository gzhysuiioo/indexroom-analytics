package indexroom

import (
	"reflect"
	"testing"
)

// This file is the regression guarantee for transaction-identifier filtering
// in QueryTimeStats: every assertion below is made against the counts the
// query actually returns (per-segment buckets, whole-window totals, resolved
// heights, and MissingTimeBlocks) — never against the filter in isolation.
// The query entry point, parameters, and return structure are the existing
// TimeStatsQuery / TimeStats API.
//
// The shared fixture (window [0,20), ten-second segments) is:
//
//	h1 t=0   txs [a, a, "", A, " a "]   // five occurrences, four ids
//	h2 t=nil txs ["", a]                 // no timestamp: never enters a bucket,
//	                                     // always counts toward MissingTimeBlocks
//	h3 t=10  txs ["", a]                 // two occurrences, two ids
//
// The distinct spellings "a", "A", and " a " deliberately exercise exact
// string comparison: case and surrounding whitespace are significant.

func filteredStatsChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	blocks := []Block{
		timeBlock(1, "h1", "g", []string{"a", "a", "", "A", " a "}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"", "a"}, nil),
		timeBlock(3, "h3", "h2", []string{"", "a"}, intptr(10)),
	}
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

// filteredStatsQuery fixes the height range at 1..3 and the window at
// [0,20)/10s, leaving only the TxIDs selection free per test.
func filteredStatsQuery(txIDs []string) TimeStatsQuery {
	return TimeStatsQuery{
		From:        1,
		To:          3,
		TxIDs:       txIDs,
		Start:       0,
		End:         20,
		StepSeconds: 10,
	}
}

// TestQueryTimeStatsFilterNilAndEmptyListCountEverything pins the first two
// of the three calling conventions: no identifier supplied (nil) and an empty
// list both mean "every transaction", occurrences of the empty identifier
// included. The untimestamped h2 contributes no occurrence, id, or block,
// but is still reported in MissingTimeBlocks.
func TestQueryTimeStatsFilterNilAndEmptyListCountEverything(t *testing.T) {
	index := filteredStatsChain(t)
	// h1: 5 occurrences over ids {a,"",A," a "} = 4; h3: 2 occurrences over
	// {"",a} = 2; h2 has no time and enters no bucket. Window distinct is
	// {a,"",A," a "} = 4, and two timestamped blocks hold matches.
	want := TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 5, DistinctTxIDs: 4, Blocks: 1},
			{Start: 10, End: 20, TxCount: 2, DistinctTxIDs: 2, Blocks: 1},
		},
		Totals:            TimeTotals{TxCount: 7, DistinctTxIDs: 4, Blocks: 2},
		FromHeight:        1,
		ToHeight:          3,
		MissingTimeBlocks: 1,
	}
	for name, txIDs := range map[string][]string{
		"nil list":   nil,
		"empty list": {},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := index.QueryTimeStats(filteredStatsQuery(txIDs))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("stats=%+v\nwant =%+v", got, want)
			}
		})
	}
}

// TestQueryTimeStatsFilterOneEmptyStringMatchesOnlyEmpty is the third
// convention and the sharpest distinction: the one-element set {""} is a
// real filter, not the unrestricted case. Each segment has exactly one
// empty-id transaction; the window holds one distinct id across two blocks.
func TestQueryTimeStatsFilterOneEmptyStringMatchesOnlyEmpty(t *testing.T) {
	index := filteredStatsChain(t)
	got, err := index.QueryTimeStats(filteredStatsQuery([]string{""}))
	if err != nil {
		t.Fatal(err)
	}
	want := TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
			{Start: 10, End: 20, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
		},
		Totals:            TimeTotals{TxCount: 2, DistinctTxIDs: 1, Blocks: 2},
		FromHeight:        1,
		ToHeight:          3,
		MissingTimeBlocks: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stats=%+v\nwant =%+v", got, want)
	}
	// Explicitly: h1's "a", "A", and " a " must all be rejected by {""}.
	if got.Buckets[0].TxCount != 1 || got.Totals.DistinctTxIDs != 1 {
		t.Fatalf(`{""} filter admitted non-empty identifiers: %+v`, got)
	}
}

// TestQueryTimeStatsFilterExactStringBoundaries pins exact comparison:
// "a", "A", and " a " are three different identifiers. "a" appears twice in
// h1 and again in h3; "A" and " a " each occur just once, in h1 only.
func TestQueryTimeStatsFilterExactStringBoundaries(t *testing.T) {
	index := filteredStatsChain(t)
	cases := []struct {
		name string
		txID string
		want TimeStats
	}{
		{
			name: "lowercase a", txID: "a",
			want: TimeStats{
				Buckets: []TimeBucket{
					{Start: 0, End: 10, TxCount: 2, DistinctTxIDs: 1, Blocks: 1},
					{Start: 10, End: 20, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
				},
				Totals:            TimeTotals{TxCount: 3, DistinctTxIDs: 1, Blocks: 2},
				FromHeight:        1,
				ToHeight:          3,
				MissingTimeBlocks: 1,
			},
		},
		{
			name: "uppercase A", txID: "A",
			want: TimeStats{
				Buckets: []TimeBucket{
					{Start: 0, End: 10, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
					{Start: 10, End: 20},
				},
				Totals:            TimeTotals{TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
				FromHeight:        1,
				ToHeight:          3,
				MissingTimeBlocks: 1,
			},
		},
		{
			name: "padded a", txID: " a ",
			want: TimeStats{
				Buckets: []TimeBucket{
					{Start: 0, End: 10, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
					{Start: 10, End: 20},
				},
				Totals:            TimeTotals{TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
				FromHeight:        1,
				ToHeight:          3,
				MissingTimeBlocks: 1,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := index.QueryTimeStats(filteredStatsQuery([]string{tc.txID}))
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("filter %q stats=%+v\nwant=%+v", tc.txID, got, tc.want)
			}
		})
	}

	// A filter spelling that resembles "a" but equals none of them matches
	// nothing at all — while the missing-time block is still reported.
	got, err := index.QueryTimeStats(filteredStatsQuery([]string{" a"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Totals.TxCount != 0 || got.Totals.Blocks != 0 || got.Totals.DistinctTxIDs != 0 {
		t.Fatalf(`" a" unexpectedly matched: %+v`, got.Totals)
	}
	for i, b := range got.Buckets {
		if b.TxCount != 0 || b.Blocks != 0 || b.DistinctTxIDs != 0 {
			t.Fatalf("bucket %d not zeroed under non-matching filter: %+v", i, b)
		}
	}
	if got.MissingTimeBlocks != 1 {
		t.Fatalf("missing=%d, want 1", got.MissingTimeBlocks)
	}
}

// TestQueryTimeStatsFilterDuplicateInBlockAndAcrossSegments pins occurrence
// counting under a filter: repeated identifiers inside one block count once
// per appearance, the block counts once, and distinct identifiers dedup
// within the segment and again over the whole window.
//
//	h1 t=0 [a,a,"",A," a "]: filter [a,""] -> a,a,"" = 3 occurrences,
//	                                          distinct {a,""} = 2, 1 block
//	h3 t=10 ["",a]:                    -> "",a = 2 occurrences,
//	                                          distinct {a,""} = 2, 1 block
//	window: 5 occurrences, 2 distinct ids, 2 blocks.
func TestQueryTimeStatsFilterDuplicateInBlockAndAcrossSegments(t *testing.T) {
	index := filteredStatsChain(t)
	got, err := index.QueryTimeStats(filteredStatsQuery([]string{"a", ""}))
	if err != nil {
		t.Fatal(err)
	}
	want := TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 3, DistinctTxIDs: 2, Blocks: 1},
			{Start: 10, End: 20, TxCount: 2, DistinctTxIDs: 2, Blocks: 1},
		},
		Totals:            TimeTotals{TxCount: 5, DistinctTxIDs: 2, Blocks: 2},
		FromHeight:        1,
		ToHeight:          3,
		MissingTimeBlocks: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stats=%+v\nwant =%+v", got, want)
	}

	// Same id also appears in the untimestamped h2: it must not add a third
	// occurrence, a second window id, or a block, but h2 is still missing-time.
	if got.Totals.TxCount != 5 || got.Totals.Blocks != 2 || got.Totals.DistinctTxIDs != 2 {
		t.Fatalf("untimestamped block leaked into totals: %+v", got.Totals)
	}
	if got.MissingTimeBlocks != 1 {
		t.Fatalf("missing=%d, want 1", got.MissingTimeBlocks)
	}
}

// TestQueryTimeStatsFilterOrderAndRepetitionsAreEquivalent: the identifier
// selection is a set, so reordering and repeating members must return
// byte-identical TimeStats for every segment and the whole window. The
// caller's slice, however, keeps its exact contents and order — including
// duplicate entries — after the call.
func TestQueryTimeStatsFilterOrderAndRepetitionsAreEquivalent(t *testing.T) {
	index := filteredStatsChain(t)
	spellings := [][]string{
		{"a", ""},
		{"", "a", "a"},
		{"a", "", "a", ""},
		{"", "", "a"},
	}
	first, err := index.QueryTimeStats(filteredStatsQuery(spellings[0]))
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range spellings[1:] {
		wantCopy := append([]string{}, in...)
		got, err := index.QueryTimeStats(filteredStatsQuery(in))
		if err != nil {
			t.Fatalf("query %q failed: %v", in, err)
		}
		if !reflect.DeepEqual(got, first) {
			t.Fatalf("spelling %q:\n%+v\n!=\n%+v", in, got, first)
		}
		if !reflect.DeepEqual(in, wantCopy) {
			t.Fatalf("caller's TxIDs list altered: got %q, want %q", in, wantCopy)
		}
	}
	// Also pin the baseline list itself is untouched.
	if got, want := spellings[0], []string{"a", ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("baseline list altered: %q, want %q", got, want)
	}
}

// TestQueryTimeStatsFilterNoMatchKeepsWindowAndMissingCount: when the
// selection has no matching transaction anywhere on chain, the query still
// succeeds and returns every segment covering the original window with all
// transaction counts zero (segments and totals), while MissingTimeBlocks is
// computed purely from the resolved height range.
func TestQueryTimeStatsFilterNoMatchKeepsWindowAndMissingCount(t *testing.T) {
	index := filteredStatsChain(t)
	query := TimeStatsQuery{
		From: 1, To: 3, TxIDs: []string{"nope", "also-nope"},
		Start: 0, End: 20, StepSeconds: 5, // four segments must all come back
	}
	got, err := index.QueryTimeStats(query)
	if err != nil {
		t.Fatalf("no-match query must succeed: %v", err)
	}
	if len(got.Buckets) != 4 {
		t.Fatalf("buckets=%d, want 4 covering the full window", len(got.Buckets))
	}
	wantBounds := [][2]int64{{0, 5}, {5, 10}, {10, 15}, {15, 20}}
	for i, b := range got.Buckets {
		if b.Start != wantBounds[i][0] || b.End != wantBounds[i][1] {
			t.Fatalf("bucket %d bounds=[%d,%d), want [%d,%d)",
				i, b.Start, b.End, wantBounds[i][0], wantBounds[i][1])
		}
		if b.TxCount != 0 || b.Blocks != 0 || b.DistinctTxIDs != 0 {
			t.Fatalf("bucket %d has counts under no-match filter: %+v", i, b)
		}
	}
	if got.Totals != (TimeTotals{}) {
		t.Fatalf("totals=%+v, want zero", got.Totals)
	}
	if got.FromHeight != 1 || got.ToHeight != 3 {
		t.Fatalf("resolved range=%d..%d, want 1..3", got.FromHeight, got.ToHeight)
	}
	// h2 has no timestamp even though its txs can never match; it still counts.
	if got.MissingTimeBlocks != 1 {
		t.Fatalf("missing=%d, want 1 from the selected height range", got.MissingTimeBlocks)
	}
}

// TestQueryTimeStatsFilterMissingBlockWithMatchStaysOutOfBuckets sharpens the
// missing-time independence rule: build the same scenario but give the
// untimestamped block identifiers that DO match the filter. It still enters
// no segment and contributes no occurrence or block, yet is reported missing.
func TestQueryTimeStatsFilterMissingBlockWithMatchStaysOutOfBuckets(t *testing.T) {
	index := New()
	blocks := []Block{
		timeBlock(1, "h1", "g", []string{"a"}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"a", "a", ""}, nil), // matches [a,""]; no time
		timeBlock(3, "h3", "h2", []string{"z"}, intptr(10)),    // does not match
	}
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	got, err := index.QueryTimeStats(TimeStatsQuery{
		From: 1, To: 3, TxIDs: []string{"a", ""},
		Start: 0, End: 20, StepSeconds: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
			{Start: 10, End: 20},
		},
		Totals:            TimeTotals{TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
		FromHeight:        1,
		ToHeight:          3,
		MissingTimeBlocks: 1,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stats=%+v\nwant =%+v", got, want)
	}
}

// TestQueryTimeStatsFilteredCountsAgreeWithIndependentOracle cross-checks the
// actual query return for every three filter conventions and several
// selections against referenceTimeStats, an oracle that scans a separately
// supplied chain rather than the index under test — so a shared counting bug
// cannot make an expectation match itself.
func TestQueryTimeStatsFilteredCountsAgreeWithIndependentOracle(t *testing.T) {
	chain := []Block{
		timeBlock(1, "h1", "g", []string{"a", "a", "", "A", " a "}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"", "a"}, nil),
		timeBlock(3, "h3", "h2", []string{"", "a"}, intptr(10)),
	}
	index := filteredStatsChain(t)
	selections := [][]string{
		nil,
		{},
		{""},
		{"a"},
		{"A"},
		{" a "},
		{"a", ""},
		{"", "a", "a"},
		{"none"},
	}
	for _, txIDs := range selections {
		q := filteredStatsQuery(txIDs)
		got, err := index.QueryTimeStats(q)
		if err != nil {
			t.Fatalf("selection %q failed: %v", txIDs, err)
		}
		if want := referenceTimeStats(chain, q); !reflect.DeepEqual(got, want) {
			t.Fatalf("selection %q:\n got=%+v\nwant=%+v", txIDs, got, want)
		}
	}
}
