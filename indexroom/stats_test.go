package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// timeBlock builds a block at height with the given transactions and a
// timestamp pointer.
func timeBlock(height int64, hash, parent string, txs []string, time *int64) Block {
	return Block{Height: height, Hash: hash, Parent: parent, Txs: txs, Time: time}
}

func TestQueryTimeStatsBucketsAndCounts(t *testing.T) {
	index := New()
	blocks := []Block{
		timeBlock(1, "h1", "g", []string{"a", "b", "a"}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"c"}, intptr(9)),
		timeBlock(3, "h3", "h2", []string{"a", "c"}, intptr(10)), // falls in [10,20)
		timeBlock(4, "h4", "h3", []string{"d"}, intptr(19)),
		timeBlock(5, "h5", "h4", []string{"a"}, intptr(20)), // excluded: end is exclusive
		timeBlock(6, "h6", "h5", []string{"x"}, nil),        // missing time
	}
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: 20, StepSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	want := TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 4, DistinctTxIDs: 3, Blocks: 2},
			{Start: 10, End: 20, TxCount: 3, DistinctTxIDs: 3, Blocks: 2},
		},
		Totals:            TimeTotals{TxCount: 7, DistinctTxIDs: 4, Blocks: 4},
		FromHeight:        1,
		ToHeight:          6,
		MissingTimeBlocks: 1,
	}
	if !reflect.DeepEqual(stats, want) {
		t.Fatalf("stats=%+v\nwant=%+v", stats, want)
	}
}

func TestQueryTimeStatsLastBucketTruncatedAndZeroBucketsReturned(t *testing.T) {
	index := New()
	if err := index.Append(timeBlock(1, "h1", "g", []string{"a"}, intptr(5))); err != nil {
		t.Fatal(err)
	}
	// Window [0,25) with step 10 -> three buckets, last cut to 25.
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: 25, StepSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	wantBounds := [][2]int64{{0, 10}, {10, 20}, {20, 25}}
	if len(stats.Buckets) != 3 {
		t.Fatalf("buckets=%d, want 3", len(stats.Buckets))
	}
	for i, b := range stats.Buckets {
		if b.Start != wantBounds[i][0] || b.End != wantBounds[i][1] {
			t.Fatalf("bucket %d bounds=[%d,%d), want [%d,%d)", i, b.Start, b.End, wantBounds[i][0], wantBounds[i][1])
		}
	}
	// Only the first bucket has data; the other two are zero-valued.
	if stats.Buckets[0].TxCount != 1 || stats.Buckets[1].TxCount != 0 || stats.Buckets[2].TxCount != 0 {
		t.Fatalf("unexpected bucket counts: %+v", stats.Buckets)
	}
	if stats.Buckets[1].Blocks != 0 || stats.Buckets[1].DistinctTxIDs != 0 {
		t.Fatalf("empty bucket not zero: %+v", stats.Buckets[1])
	}
	if stats.Totals != (TimeTotals{TxCount: 1, DistinctTxIDs: 1, Blocks: 1}) {
		t.Fatalf("totals=%+v", stats.Totals)
	}
}

func TestQueryTimeStatsTotalsDistinctIsGlobalNotSummed(t *testing.T) {
	index := New()
	blocks := []Block{
		timeBlock(1, "h1", "g", []string{"a", "b"}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"b", "c"}, intptr(10)),
		timeBlock(3, "h3", "h2", []string{"a", "c"}, intptr(20)),
	}
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: 30, StepSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int64{2, 2, 2} {
		if stats.Buckets[i].DistinctTxIDs != want {
			t.Fatalf("bucket %d distinct=%d, want %d", i, stats.Buckets[i].DistinctTxIDs, want)
		}
	}
	// Per-bucket distinct would sum to 6, but the window only holds 3 ids.
	if stats.Totals.DistinctTxIDs != 3 {
		t.Fatalf("window distinct=%d, want 3", stats.Totals.DistinctTxIDs)
	}
	if stats.Totals.TxCount != 6 || stats.Totals.Blocks != 3 {
		t.Fatalf("totals=%+v", stats.Totals)
	}
}

func TestQueryTimeStatsFilterAndHeightRange(t *testing.T) {
	index := New()
	blocks := []Block{
		timeBlock(1, "h1", "g", []string{"a", "b", ""}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"a", ""}, intptr(10)),
		timeBlock(3, "h3", "h2", []string{"b"}, intptr(20)),
	}
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	// Height range 2..3 plus an id filter including the empty identifier.
	stats, err := index.QueryTimeStats(TimeStatsQuery{
		From: 2, To: 3, TxIDs: []string{"a", ""}, Start: 0, End: 30, StepSeconds: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.FromHeight != 2 || stats.ToHeight != 3 {
		t.Fatalf("resolved range=%d..%d", stats.FromHeight, stats.ToHeight)
	}
	// Height 1 excluded entirely, so its empty-id tx doesn't count as missing.
	if stats.MissingTimeBlocks != 0 {
		t.Fatalf("missing=%d, want 0", stats.MissingTimeBlocks)
	}
	// Bucket [10,20): height 2 has "a" and "" -> 2 occurrences, 2 ids, 1 block.
	if got := stats.Buckets[1]; got != (TimeBucket{Start: 10, End: 20, TxCount: 2, DistinctTxIDs: 2, Blocks: 1}) {
		t.Fatalf("bucket [10,20)=%+v", got)
	}
	// Bucket [20,30): height 3's "b" doesn't match the filter -> zero.
	if got := stats.Buckets[2]; got.TxCount != 0 || got.Blocks != 0 {
		t.Fatalf("bucket [20,30)=%+v, want zero counts", got)
	}
	if stats.Totals != (TimeTotals{TxCount: 2, DistinctTxIDs: 2, Blocks: 1}) {
		t.Fatalf("totals=%+v", stats.Totals)
	}
}

func TestQueryTimeStatsMissingTimeBlocksIgnoresFilter(t *testing.T) {
	index := New()
	blocks := []Block{
		timeBlock(1, "h1", "g", []string{"a"}, nil),
		timeBlock(2, "h2", "h1", []string{"b"}, nil),
		timeBlock(3, "h3", "h2", []string{"zzz"}, intptr(5)),
	}
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	// Filter matches nothing; missing blocks are still reported.
	stats, err := index.QueryTimeStats(TimeStatsQuery{
		TxIDs: []string{"none"}, Start: 0, End: 10, StepSeconds: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.MissingTimeBlocks != 2 {
		t.Fatalf("missing=%d, want 2", stats.MissingTimeBlocks)
	}
	if stats.Totals.TxCount != 0 || stats.Buckets[0].TxCount != 0 {
		t.Fatalf("filter leaked matches: %+v", stats)
	}
}

func TestQueryTimeStatsRepeatedTxsCountBlockOnce(t *testing.T) {
	index := New()
	if err := index.Append(timeBlock(1, "h1", "g", []string{"a", "a", "a"}, intptr(0))); err != nil {
		t.Fatal(err)
	}
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: 10, StepSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Buckets[0].TxCount != 3 || stats.Buckets[0].Blocks != 1 || stats.Buckets[0].DistinctTxIDs != 1 {
		t.Fatalf("bucket=%+v", stats.Buckets[0])
	}
}

func TestQueryTimeStatsZeroTimestampIsReal(t *testing.T) {
	index := New()
	if err := index.Append(timeBlock(1, "h1", "g", []string{"a"}, intptr(0))); err != nil {
		t.Fatal(err)
	}
	// A zero timestamp lands in a bucket starting at zero.
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: 1, StepSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Buckets[0].TxCount != 1 || stats.MissingTimeBlocks != 0 {
		t.Fatalf("zero timestamp mishandled: %+v", stats)
	}
	// The same block is missing relative to a window strictly above zero.
	stats, err = index.QueryTimeStats(TimeStatsQuery{Start: 1, End: 2, StepSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Buckets[0].TxCount != 0 {
		t.Fatalf("zero timestamp counted above its time: %+v", stats.Buckets[0])
	}
}

func TestQueryTimeStatsEmptyAndOutOfRangeReturnZeroBuckets(t *testing.T) {
	query := TimeStatsQuery{Start: 0, End: 30, StepSeconds: 10}
	empty, err := New().QueryTimeStats(query)
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Buckets) != 3 || empty.ToHeight != 0 || empty.MissingTimeBlocks != 0 {
		t.Fatalf("empty chain result=%+v", empty)
	}

	index := New()
	if err := index.Append(timeBlock(1, "h1", "g", []string{"a"}, intptr(0))); err != nil {
		t.Fatal(err)
	}
	// Height range entirely above the tip clamps to tip and stays empty.
	above, err := index.QueryTimeStats(TimeStatsQuery{From: 5, Start: 0, End: 10, StepSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if above.FromHeight != 5 || above.ToHeight != 1 || len(above.Buckets) != 1 || above.Buckets[0].TxCount != 0 {
		t.Fatalf("above-tip result=%+v", above)
	}
	// To above the tip is clamped, not an error.
	clamped, err := index.QueryTimeStats(TimeStatsQuery{To: 99, Start: 0, End: 10, StepSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if clamped.ToHeight != 1 || clamped.Buckets[0].TxCount != 1 {
		t.Fatalf("clamped result=%+v", clamped)
	}
	// Window over a span with no matching times.
	nomatch, err := index.QueryTimeStats(TimeStatsQuery{Start: 1000, End: 1010, StepSeconds: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range nomatch.Buckets {
		if b.TxCount != 0 || b.Blocks != 0 || b.DistinctTxIDs != 0 {
			t.Fatalf("non-matching window has data: %+v", b)
		}
	}
}

func TestQueryTimeStatsRejectsInvalidArguments(t *testing.T) {
	index := New()
	if err := index.Append(timeBlock(1, "h1", "g", []string{"a"}, intptr(0))); err != nil {
		t.Fatal(err)
	}
	maxInt := int64(^uint64(0) >> 1)
	cases := map[string]TimeStatsQuery{
		"negative start":        {Start: -1, End: 10, StepSeconds: 10},
		"negative end":          {Start: 0, End: -1, StepSeconds: 10},
		"start equals end":      {Start: 5, End: 5, StepSeconds: 1},
		"start above end":       {Start: 6, End: 5, StepSeconds: 1},
		"zero step":             {Start: 0, End: 10, StepSeconds: 0},
		"negative step":         {Start: 0, End: 10, StepSeconds: -5},
		"too many buckets":      {Start: 0, End: 100010, StepSeconds: 10}, // 10001 buckets
		"negative from":         {From: -1, Start: 0, End: 10, StepSeconds: 10},
		"negative to":           {To: -1, Start: 0, End: 10, StepSeconds: 10},
		"to below from":         {From: 3, To: 2, Start: 0, End: 10, StepSeconds: 10},
		"overflow bucket count": {Start: 0, End: maxInt, StepSeconds: 1},
	}
	for name, query := range cases {
		if _, err := index.QueryTimeStats(query); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err=%v, want ErrInvalidArgument", name, err)
		}
	}
}

func TestQueryTimeStatsBucketLimitBoundary(t *testing.T) {
	index := New()
	// Exactly 10000 buckets is allowed: width 10000 with step 1.
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: MaxTimeBuckets, StepSeconds: 1})
	if err != nil {
		t.Fatalf("exactly %d buckets refused: %v", MaxTimeBuckets, err)
	}
	if len(stats.Buckets) != MaxTimeBuckets {
		t.Fatalf("buckets=%d", len(stats.Buckets))
	}
}

func TestQueryTimeStatsNearInt64UpperBound(t *testing.T) {
	maxInt := int64(^uint64(0) >> 1)
	index := New()
	blocks := []Block{
		timeBlock(1, "h1", "g", []string{"a"}, intptr(maxInt-1)),
		timeBlock(2, "h2", "h1", []string{"b"}, intptr(maxInt)),
	}
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	// Window [maxInt-2, maxInt] with step 10 would overflow; step 1 keeps
	// it to two buckets and boundaries must not wrap to negatives.
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: maxInt - 2, End: maxInt, StepSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Buckets) != 2 {
		t.Fatalf("buckets=%d, want 2", len(stats.Buckets))
	}
	if stats.Buckets[0] != (TimeBucket{Start: maxInt - 2, End: maxInt - 1}) {
		t.Fatalf("first bucket=%+v", stats.Buckets[0])
	}
	if stats.Buckets[1] != (TimeBucket{Start: maxInt - 1, End: maxInt, TxCount: 1, DistinctTxIDs: 1, Blocks: 1}) {
		t.Fatalf("second bucket=%+v", stats.Buckets[1])
	}
	if stats.Totals.TxCount != 1 {
		t.Fatalf("totals=%+v", stats.Totals)
	}
	// A wide legal window that ends at MaxInt64 with a coarse step: the
	// saturated boundary is cut back to End instead of wrapping.
	wide, err := index.QueryTimeStats(TimeStatsQuery{Start: maxInt - 5, End: maxInt, StepSeconds: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range wide.Buckets {
		if b.Start < 0 || b.End < b.Start || b.End > maxInt {
			t.Fatalf("bucket overflowed: %+v", b)
		}
	}
	if last := wide.Buckets[len(wide.Buckets)-1]; last.End != maxInt {
		t.Fatalf("last bucket end=%d, want %d", last.End, maxInt)
	}
}

func TestQueryTimeStatsAfterReorgAndRestoreCountsCurrentChain(t *testing.T) {
	index := New()
	blocks := []Block{
		timeBlock(1, "h1", "g", []string{"a"}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"a"}, intptr(10)),
	}
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	before, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: 20, StepSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if before.Totals.TxCount != 2 {
		t.Fatalf("before totals=%+v", before.Totals)
	}
	// Replace height 2 with a block at a different timestamp.
	if _, err := index.Reorg([]Block{
		timeBlock(2, "h2b", "h1", []string{"b", "b"}, intptr(15)),
	}); err != nil {
		t.Fatal(err)
	}
	after, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: 20, StepSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if after.Buckets[0].TxCount != 1 { // only height 1 at time 0 remains
		t.Fatalf("old-chain tx still counted: %+v", after.Buckets[0])
	}
	if after.Buckets[1].TxCount != 2 || after.Buckets[1].Blocks != 1 || after.Buckets[1].DistinctTxIDs != 1 {
		t.Fatalf("new-chain tx not counted: %+v", after.Buckets[1])
	}

	// Restore a snapshot with missing times; those blocks leave the buckets.
	snap := `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"x1","parent":"g","txs":["z"]},` +
		`{"height":2,"hash":"x2","parent":"x1","txs":["z"]}` +
		`]}`
	if err := index.Restore(strings.NewReader(snap)); err != nil {
		t.Fatal(err)
	}
	restored, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: 20, StepSeconds: 10})
	if err != nil {
		t.Fatal(err)
	}
	if restored.Totals.TxCount != 0 || restored.MissingTimeBlocks != 2 {
		t.Fatalf("post-restore stats=%+v", restored)
	}
}

func TestQueryTimeStatsReadsOneConsistentState(t *testing.T) {
	index := New()
	if err := index.Append(timeBlock(1, "h1", "g", []string{"seed"}, intptr(0))); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var writers sync.WaitGroup
	writers.Add(2)
	for w := 0; w < 2; w++ {
		go func() {
			defer writers.Done()
			for height := int64(2); height <= 200; height++ {
				select {
				case <-stop:
					return
				default:
				}
				b := timeBlock(height, fmt.Sprintf("h%d", height),
					fmt.Sprintf("h%d", height-1), []string{fmt.Sprintf("tx-%d", height)}, intptr(height))
				if err := index.Append(b); err != nil {
					return
				}
			}
		}()
	}
	var readers sync.WaitGroup
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for i := 0; i < 100; i++ {
				stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: 201, StepSeconds: 7})
				if err != nil {
					t.Errorf("stats failed: %v", err)
					return
				}
				// Internal consistency against one snapshot: per-bucket
				// occurrence counts sum to the window total.
				var sum int64
				for _, b := range stats.Buckets {
					sum += b.TxCount
					if b.Start < 0 || b.End <= b.Start {
						t.Errorf("bad bucket %+v", b)
						return
					}
				}
				if sum != stats.Totals.TxCount {
					t.Errorf("bucket counts %d != total %d", sum, stats.Totals.TxCount)
					return
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	writers.Wait()
}
