package indexroom

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"testing"
)

// tb is one block spec for tsChain: a missing timestamp is nil.
type tb struct {
	ts  *int64
	txs []string
}

// tsp returns a pointer to v for use in tb literals.
func tsp(v int64) *int64 { return &v }

// tsChain builds a chain with one block per spec, hashes h1..hN.
func tsChain(t *testing.T, blocks ...tb) *Index {
	t.Helper()
	index := New()
	for i, b := range blocks {
		height := int64(i + 1)
		parent := "genesis"
		if height > 1 {
			parent = fmt.Sprintf("h%d", height-1)
		}
		block := Block{
			Height:    height,
			Hash:      fmt.Sprintf("h%d", height),
			Parent:    parent,
			Txs:       b.txs,
			Timestamp: b.ts,
		}
		if err := index.Append(block); err != nil {
			t.Fatalf("setup append at height %d: %v", height, err)
		}
	}
	return index
}

func TestQueryTimeStatsSegmentsAndSummary(t *testing.T) {
	index := tsChain(t,
		tb{tsp(100), []string{"a", "b"}},
		tb{tsp(200), []string{"a"}},
		tb{tsp(300), nil},
		tb{tsp(400), []string{"c"}},
		tb{tsp(500), []string{"a"}},
		tb{nil, []string{"d"}},
	)
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 150, End: 450, Segment: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := []TimeSegment{
		{Start: 150, End: 250, TxCount: 1, UniqueTxIDs: 1, MatchedBlocks: 1},
		{Start: 250, End: 350},
		{Start: 350, End: 450, TxCount: 1, UniqueTxIDs: 1, MatchedBlocks: 1},
	}
	if !reflect.DeepEqual(stats.Segments, want) {
		t.Fatalf("segments=%+v, want %+v", stats.Segments, want)
	}
	wantSummary := TimeSegment{Start: 150, End: 450, TxCount: 2, UniqueTxIDs: 2, MatchedBlocks: 2}
	if !reflect.DeepEqual(stats.Summary, wantSummary) {
		t.Fatalf("summary=%+v, want %+v", stats.Summary, wantSummary)
	}
	// The block without a timestamp is reported, unaffected by any filter.
	if stats.Untimestamped != 1 {
		t.Fatalf("untimestamped=%d, want 1", stats.Untimestamped)
	}
}

func TestQueryTimeStatsWindowBoundariesAndGrid(t *testing.T) {
	index := tsChain(t,
		tb{tsp(100), []string{"a", "b"}},
		tb{tsp(200), []string{"a"}},
		tb{tsp(300), nil},
		tb{tsp(400), []string{"c"}},
		tb{tsp(500), []string{"a"}},
	)
	// [start, end): ts 100 is included, ts 500 is excluded; the 200-second
	// grid starts at the request start, not at a round number.
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 100, End: 500, Segment: 200})
	if err != nil {
		t.Fatal(err)
	}
	want := []TimeSegment{
		{Start: 100, End: 300, TxCount: 3, UniqueTxIDs: 2, MatchedBlocks: 2},
		{Start: 300, End: 500, TxCount: 1, UniqueTxIDs: 1, MatchedBlocks: 1},
	}
	if !reflect.DeepEqual(stats.Segments, want) {
		t.Fatalf("segments=%+v, want %+v", stats.Segments, want)
	}
	// The summary deduplicates over the whole window: {a,b,c} is 3, not the
	// sum of the per-segment counts (2+1).
	wantSummary := TimeSegment{Start: 100, End: 500, TxCount: 4, UniqueTxIDs: 3, MatchedBlocks: 3}
	if !reflect.DeepEqual(stats.Summary, wantSummary) {
		t.Fatalf("summary=%+v, want %+v", stats.Summary, wantSummary)
	}
	if stats.Untimestamped != 0 {
		t.Fatalf("untimestamped=%d, want 0", stats.Untimestamped)
	}
}

func TestQueryTimeStatsLastSegmentTruncated(t *testing.T) {
	index := tsChain(t,
		tb{tsp(100), []string{"a"}},
		tb{tsp(250), []string{"a"}},
	)
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 100, End: 300, Segment: 150})
	if err != nil {
		t.Fatal(err)
	}
	want := []TimeSegment{
		{Start: 100, End: 250, TxCount: 1, UniqueTxIDs: 1, MatchedBlocks: 1},
		{Start: 250, End: 300, TxCount: 1, UniqueTxIDs: 1, MatchedBlocks: 1},
	}
	if !reflect.DeepEqual(stats.Segments, want) {
		t.Fatalf("segments=%+v, want %+v", stats.Segments, want)
	}
}

func TestQueryTimeStatsReusesHeightRangeAndFilter(t *testing.T) {
	index := tsChain(t,
		tb{tsp(100), []string{"a"}},
		tb{tsp(200), []string{"b"}},
		tb{tsp(300), []string{"a"}},
		tb{nil, []string{"a"}},
	)
	// Only heights 2..3 are considered: h1 and the untimestamped h4 drop out.
	stats, err := index.QueryTimeStats(TimeStatsQuery{From: 2, To: 3, Start: 100, End: 400, Segment: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := []TimeSegment{
		{Start: 100, End: 200},
		{Start: 200, End: 300, TxCount: 1, UniqueTxIDs: 1, MatchedBlocks: 1},
		{Start: 300, End: 400, TxCount: 1, UniqueTxIDs: 1, MatchedBlocks: 1},
	}
	if !reflect.DeepEqual(stats.Segments, want) {
		t.Fatalf("segments=%+v, want %+v", stats.Segments, want)
	}
	if stats.Untimestamped != 0 {
		t.Fatalf("untimestamped=%d, want 0", stats.Untimestamped)
	}

	// The filter applies to occurrences and blocks, but never to the
	// untimestamped count.
	stats, err = index.QueryTimeStats(TimeStatsQuery{TxIDs: []string{"a"}, Start: 100, End: 400, Segment: 100})
	if err != nil {
		t.Fatal(err)
	}
	want = []TimeSegment{
		{Start: 100, End: 200, TxCount: 1, UniqueTxIDs: 1, MatchedBlocks: 1},
		{Start: 200, End: 300},
		{Start: 300, End: 400, TxCount: 1, UniqueTxIDs: 1, MatchedBlocks: 1},
	}
	if !reflect.DeepEqual(stats.Segments, want) {
		t.Fatalf("filtered segments=%+v, want %+v", stats.Segments, want)
	}
	if stats.Summary.TxCount != 2 || stats.Summary.UniqueTxIDs != 1 || stats.Summary.MatchedBlocks != 2 {
		t.Fatalf("filtered summary=%+v", stats.Summary)
	}
	if stats.Untimestamped != 1 {
		t.Fatalf("untimestamped=%d, want 1 even with a filter", stats.Untimestamped)
	}
}

func TestQueryTimeStatsEmptyIdentifierParticipates(t *testing.T) {
	index := tsChain(t,
		tb{tsp(100), []string{"", "a", ""}},
	)
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 100, End: 200, Segment: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := []TimeSegment{{Start: 100, End: 200, TxCount: 3, UniqueTxIDs: 2, MatchedBlocks: 1}}
	if !reflect.DeepEqual(stats.Segments, want) {
		t.Fatalf("segments=%+v, want %+v", stats.Segments, want)
	}
}

func TestQueryTimeStatsEmptyChainAndOutOfRange(t *testing.T) {
	stats, err := New().QueryTimeStats(TimeStatsQuery{Start: 0, End: 300, Segment: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := []TimeSegment{
		{Start: 0, End: 100},
		{Start: 100, End: 200},
		{Start: 200, End: 300},
	}
	if !reflect.DeepEqual(stats.Segments, want) {
		t.Fatalf("empty segments=%+v, want %+v", stats.Segments, want)
	}
	if stats.Summary != (TimeSegment{Start: 0, End: 300}) || stats.Untimestamped != 0 {
		t.Fatalf("empty summary=%+v untimestamped=%d", stats.Summary, stats.Untimestamped)
	}

	index := tsChain(t, tb{tsp(100), []string{"a"}})
	stats, err = index.QueryTimeStats(TimeStatsQuery{From: 5, Start: 0, End: 300, Segment: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stats.Segments, want) {
		t.Fatalf("out-of-range segments=%+v, want %+v", stats.Segments, want)
	}
}

func TestQueryTimeStatsRejectsInvalidArguments(t *testing.T) {
	index := tsChain(t, tb{tsp(100), []string{"a"}})
	cases := map[string]TimeStatsQuery{
		"negative start":    {Start: -1, End: 100, Segment: 10},
		"negative end":      {Start: 0, End: -1, Segment: 10},
		"start equals end":  {Start: 10, End: 10, Segment: 10},
		"start after end":   {Start: 20, End: 10, Segment: 10},
		"zero segment":      {Start: 0, End: 100, Segment: 0},
		"negative segment":  {Start: 0, End: 100, Segment: -1},
		"negative from":     {From: -1, Start: 0, End: 100, Segment: 10},
		"negative to":       {To: -1, Start: 0, End: 100, Segment: 10},
		"to below from":     {From: 3, To: 2, Start: 0, End: 100, Segment: 10},
		"too many segments": {Start: 0, End: MaxTimeSegments + 1, Segment: 1},
	}
	for name, query := range cases {
		if _, err := index.QueryTimeStats(query); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err=%v, want ErrInvalidArgument", name, err)
		}
	}
	// Exactly the segment cap is accepted, as are boundary heights.
	if _, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: MaxTimeSegments, Segment: 1}); err != nil {
		t.Errorf("segment cap refused: %v", err)
	}
	if _, err := index.QueryTimeStats(TimeStatsQuery{From: 0, To: 0, Start: 0, End: 10, Segment: 10}); err != nil {
		t.Errorf("zero heights refused: %v", err)
	}
}

func TestQueryTimeStatsNearInt64Max(t *testing.T) {
	index := tsChain(t,
		tb{tsp(math.MaxInt64 - 5), []string{"a"}},
		tb{tsp(math.MaxInt64), []string{"b"}},
	)
	// A window hugging the top of the int64 range must segment without
	// overflow: the last segment is truncated at the window end.
	stats, err := index.QueryTimeStats(TimeStatsQuery{
		Start:   math.MaxInt64 - 10,
		End:     math.MaxInt64,
		Segment: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []TimeSegment{
		{Start: math.MaxInt64 - 10, End: math.MaxInt64, TxCount: 1, UniqueTxIDs: 1, MatchedBlocks: 1},
	}
	if !reflect.DeepEqual(stats.Segments, want) {
		t.Fatalf("segments=%+v, want %+v", stats.Segments, want)
	}
	if stats.Summary.TxCount != 1 {
		t.Fatalf("summary=%+v", stats.Summary)
	}

	// A segment larger than the window: one segment spanning the whole range.
	// The block at MaxInt64 is excluded by the end-exclusive window.
	stats, err = index.QueryTimeStats(TimeStatsQuery{Start: 0, End: math.MaxInt64, Segment: math.MaxInt64})
	if err != nil {
		t.Fatal(err)
	}
	want = []TimeSegment{{Start: 0, End: math.MaxInt64, TxCount: 1, UniqueTxIDs: 1, MatchedBlocks: 1}}
	if !reflect.DeepEqual(stats.Segments, want) {
		t.Fatalf("full-range segments=%+v, want %+v", stats.Segments, want)
	}

	// The top of the range can host the maximum number of segments.
	stats, err = index.QueryTimeStats(TimeStatsQuery{
		Start:   math.MaxInt64 - MaxTimeSegments,
		End:     math.MaxInt64,
		Segment: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(stats.Segments) != MaxTimeSegments {
		t.Fatalf("segments=%d, want %d", len(stats.Segments), MaxTimeSegments)
	}
	if stats.Segments[0].Start != math.MaxInt64-MaxTimeSegments ||
		stats.Segments[len(stats.Segments)-1].End != math.MaxInt64 {
		t.Fatalf("segment bounds wrong: first=%+v last=%+v", stats.Segments[0], stats.Segments[len(stats.Segments)-1])
	}
}

func TestQueryTimeStatsRejectsNegativeTimestamps(t *testing.T) {
	index := New()
	blocks, byHash, tip := snapshot(index)
	if err := index.Append(Block{Height: 1, Hash: "h1", Parent: "genesis", Timestamp: tsp(-1)}); err == nil {
		t.Fatal("expected refusal of negative timestamp")
	}
	requireUnchanged(t, index, blocks, byHash, tip)

	index = tsChain(t, tb{tsp(100), []string{"a"}})
	blocks, byHash, tip = snapshot(index)
	if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1", Timestamp: tsp(-5)}}); err == nil {
		t.Fatal("expected refusal of negative timestamp in reorg branch")
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

func TestQueryTimeStatsTimestampIsContent(t *testing.T) {
	index := tsChain(t, tb{tsp(100), []string{"a"}})
	// Same block content except the timestamp: re-ingestion must be rejected.
	blocks, byHash, tip := snapshot(index)
	if err := index.Append(Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a"}, Timestamp: tsp(200)}); err == nil {
		t.Fatal("expected refusal of timestamp-only difference")
	}
	requireUnchanged(t, index, blocks, byHash, tip)
	// A missing timestamp also differs from a present zero.
	if err := index.Append(Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a"}}); err == nil {
		t.Fatal("expected refusal of missing vs present timestamp")
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

func TestQueryTimeStatsStoredTimestampIsACopy(t *testing.T) {
	index := New()
	ts := int64(100)
	if err := index.Append(Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a"}, Timestamp: &ts}); err != nil {
		t.Fatal(err)
	}
	ts = 999
	stats, err := index.QueryTimeStats(TimeStatsQuery{Start: 100, End: 200, Segment: 100})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Summary.TxCount != 1 {
		t.Fatalf("stored timestamp changed by caller mutation: %+v", stats.Summary)
	}
	stats, err = index.QueryTimeStats(TimeStatsQuery{Start: 900, End: 1000, Segment: 100})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Summary.TxCount != 0 {
		t.Fatalf("stored timestamp changed by caller mutation: %+v", stats.Summary)
	}
}

func TestQueryTimeStatsObservesCurrentChain(t *testing.T) {
	index := tsChain(t,
		tb{tsp(100), []string{"a"}},
		tb{tsp(200), []string{"b"}},
	)
	query := TimeStatsQuery{Start: 0, End: 300, Segment: 100}
	before, err := index.QueryTimeStats(query)
	if err != nil {
		t.Fatal(err)
	}
	if before.Summary.TxCount != 2 {
		t.Fatalf("before reorg: %+v", before.Summary)
	}
	// Replace and shorten the chain: only the current main chain counts.
	if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"c"}, Timestamp: tsp(150)}}); err != nil {
		t.Fatal(err)
	}
	after, err := index.QueryTimeStats(query)
	if err != nil {
		t.Fatal(err)
	}
	want := []TimeSegment{
		{Start: 0, End: 100},
		{Start: 100, End: 200, TxCount: 2, UniqueTxIDs: 2, MatchedBlocks: 2},
		{Start: 200, End: 300},
	}
	if !reflect.DeepEqual(after.Segments, want) {
		t.Fatalf("after reorg segments=%+v, want %+v", after.Segments, want)
	}
}

func TestQueryTimeStatsConcurrentWithChainOps(t *testing.T) {
	index := tsChain(t, tb{tsp(100), []string{"seed"}})
	stop := make(chan struct{})

	var writer sync.WaitGroup
	writer.Add(1)
	go func() {
		defer writer.Done()
		for height := int64(2); height <= 200; height++ {
			select {
			case <-stop:
				return
			default:
			}
			block := Block{
				Height:    height,
				Hash:      fmt.Sprintf("h%d", height),
				Parent:    fmt.Sprintf("h%d", height-1),
				Txs:       []string{fmt.Sprintf("tx-%d", height)},
				Timestamp: tsp(100 + height),
			}
			if err := index.Append(block); err != nil {
				return
			}
		}
	}()

	var readers sync.WaitGroup
	for reader := 0; reader < 4; reader++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for round := 0; round < 20; round++ {
				stats, err := index.QueryTimeStats(TimeStatsQuery{
					Start:   100,
					End:     400,
					Segment: 50,
				})
				if err != nil {
					t.Errorf("stats query failed: %v", err)
					return
				}
				// The result must describe one complete chain state.
				var txTotal, blockTotal int64
				var lastEnd int64 = 100
				for i, seg := range stats.Segments {
					if i == 0 && seg.Start != 100 {
						t.Errorf("first segment starts at %d, want 100", seg.Start)
						return
					}
					if seg.Start != lastEnd {
						t.Errorf("gap between segments: %d after %d", seg.Start, lastEnd)
						return
					}
					if seg.End <= seg.Start || seg.End > 400 {
						t.Errorf("bad segment bounds: %+v", seg)
						return
					}
					if seg.UniqueTxIDs > seg.TxCount {
						t.Errorf("unique %d exceeds occurrences %d", seg.UniqueTxIDs, seg.TxCount)
						return
					}
					txTotal += seg.TxCount
					blockTotal += seg.MatchedBlocks
					lastEnd = seg.End
				}
				if lastEnd != 400 {
					t.Errorf("last segment ends at %d, want 400", lastEnd)
					return
				}
				if txTotal != stats.Summary.TxCount || blockTotal != stats.Summary.MatchedBlocks {
					t.Errorf("summary mismatch: summed tx=%d blocks=%d, summary=%+v",
						txTotal, blockTotal, stats.Summary)
					return
				}
				if stats.Summary.UniqueTxIDs < stats.Segments[0].UniqueTxIDs {
					t.Errorf("summary unique %d below segment unique", stats.Summary.UniqueTxIDs)
					return
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	writer.Wait()
}
