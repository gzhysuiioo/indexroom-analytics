package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// minBlocksChain builds the chain from the MinBlocks specification:
// heights 1..3 hold transactions [a,a,b], [b,c], [a,d].
func minBlocksChain(t *testing.T) *Index {
	t.Helper()
	return txChain(t,
		[]string{"a", "a", "b"},
		[]string{"b", "c"},
		[]string{"a", "d"},
	)
}

func TestQueryTxsMinBlocksSpecExample(t *testing.T) {
	// a occurs in blocks 1 and 3, b in blocks 1 and 2: both reach the
	// threshold of two distinct blocks. c and d occur in one block each and
	// are excluded. Qualifying identifiers keep every occurrence, including
	// the duplicated a inside block 1.
	index := minBlocksChain(t)
	page, err := index.QueryTxs(TxQuery{MinBlocks: 2})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
		{Height: 1, BlockHash: "h1", TxID: "b", Position: 2},
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits=%v, want %v", page.Hits, want)
	}
	if page.TotalMatches != 5 || page.MatchedBlocks != 3 || page.ToHeight != 3 {
		t.Fatalf("stats=%+v, want total 5 blocks 3 to 3", page)
	}
	if page.NextCursor != "" {
		t.Fatalf("unexpected cursor %q", page.NextCursor)
	}
}

func TestQueryTxsMinBlocksRestrictedRange(t *testing.T) {
	// Over heights 1..2 only, a occurs in a single block and no longer
	// qualifies; b still occurs in blocks 1 and 2 and keeps both
	// occurrences. Eligibility is decided by the whole selected range.
	index := minBlocksChain(t)
	page, err := index.QueryTxs(TxQuery{From: 1, To: 2, MinBlocks: 2})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "b", Position: 2},
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits=%v, want %v", page.Hits, want)
	}
	if page.TotalMatches != 2 || page.MatchedBlocks != 2 || page.ToHeight != 2 {
		t.Fatalf("stats=%+v, want total 2 blocks 2 to 2", page)
	}
}

func TestQueryTxsMinBlocksZeroKeepsEverything(t *testing.T) {
	// The zero value disables the filter: every occurrence survives, exactly
	// as a query that never heard of MinBlocks.
	index := minBlocksChain(t)
	page, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalMatches != 7 || page.MatchedBlocks != 3 || len(page.Hits) != 7 {
		t.Fatalf("disabled filter page=%+v, want all 7 occurrences in 3 blocks", page)
	}
}

func TestQueryTxsMinBlocksSameBlockCountsOnce(t *testing.T) {
	// Three a occurrences inside one block contribute a single block toward
	// the threshold, so a does not qualify at MinBlocks 2.
	index := txChain(t, []string{"a", "a", "a"})
	page, err := index.QueryTxs(TxQuery{MinBlocks: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 {
		t.Fatalf("page=%+v, want an empty result", page)
	}
	// At MinBlocks 1 the same block qualifies a and keeps all duplicates.
	page, err = index.QueryTxs(TxQuery{MinBlocks: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalMatches != 3 || page.MatchedBlocks != 1 || len(page.Hits) != 3 {
		t.Fatalf("page=%+v, want all 3 occurrences in 1 block", page)
	}
}

func TestQueryTxsMinBlocksNegativeRejected(t *testing.T) {
	index := minBlocksChain(t)
	page, err := index.QueryTxs(TxQuery{MinBlocks: -1})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err=%v, want ErrInvalidArgument", err)
	}
	if len(page.Hits) != 0 || page.NextCursor != "" {
		t.Fatalf("rejected query must not return a usable page: %+v", page)
	}
}

func TestQueryTxsMinBlocksNobodyQualifies(t *testing.T) {
	// A legal threshold no identifier reaches: success with an empty page,
	// zero statistics, and no continuation cursor.
	index := minBlocksChain(t)
	page, err := index.QueryTxs(TxQuery{MinBlocks: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 {
		t.Fatalf("unexpected matches: %+v", page)
	}
	if page.ToHeight != 3 || page.NextCursor != "" {
		t.Fatalf("empty page should pin the tip and carry no cursor: %+v", page)
	}
}

func TestQueryTxsMinBlocksIntersectsTxIDs(t *testing.T) {
	index := minBlocksChain(t)
	// Restricting to b keeps its two occurrences; restricting to c (one
	// block) keeps nothing at threshold 2 but one occurrence at threshold 1.
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"b"}, MinBlocks: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalMatches != 2 || page.MatchedBlocks != 2 {
		t.Fatalf("b stats=%+v, want total 2 blocks 2", page)
	}
	page, err = index.QueryTxs(TxQuery{TxIDs: []string{"c"}, MinBlocks: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalMatches != 0 || len(page.Hits) != 0 {
		t.Fatalf("c at threshold 2: %+v, want empty", page)
	}
	page, err = index.QueryTxs(TxQuery{TxIDs: []string{"c"}, MinBlocks: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalMatches != 1 || page.MatchedBlocks != 1 {
		t.Fatalf("c at threshold 1: %+v, want total 1 blocks 1", page)
	}
}

func TestQueryTxsMinBlocksIntersectsTimeWindow(t *testing.T) {
	// Heights 1..3 hold [a], [a], [a] with times 100, 200, 300. Inside the
	// window [100, 150) only one block carries a, so a misses the threshold
	// of two; without the window it qualifies.
	index := New()
	for i, when := range []int64{100, 200, 300} {
		height := int64(i + 1)
		parent := "genesis"
		if height > 1 {
			parent = fmt.Sprintf("h%d", height-1)
		}
		block := Block{
			Height: height,
			Hash:   fmt.Sprintf("h%d", height),
			Parent: parent,
			Txs:    []string{"a"},
			Time:   intptr(when),
		}
		if err := index.Append(block); err != nil {
			t.Fatalf("setup append at %d: %v", height, err)
		}
	}

	windowed, err := index.QueryTxs(TxQuery{
		MinBlocks: 2, TimeStart: intptr(100), TimeEnd: intptr(150),
	})
	if err != nil {
		t.Fatal(err)
	}
	if windowed.TotalMatches != 0 || len(windowed.Hits) != 0 {
		t.Fatalf("windowed page=%+v, want empty: only one in-window block carries a", windowed)
	}

	plain, err := index.QueryTxs(TxQuery{MinBlocks: 2})
	if err != nil {
		t.Fatal(err)
	}
	if plain.TotalMatches != 3 || plain.MatchedBlocks != 3 {
		t.Fatalf("unwindowed page=%+v, want total 3 blocks 3", plain)
	}
}

func TestQueryTxsMinBlocksStatsStableAcrossPages(t *testing.T) {
	// The spec example paged two at a time: eligibility comes from the whole
	// pinned range, never from the current page, and every page reports the
	// same full-range statistics.
	index := minBlocksChain(t)
	pages := collectPages(t, index, TxQuery{MinBlocks: 2, PageSize: 2})
	if len(pages) != 3 {
		t.Fatalf("pages=%d, want 3", len(pages))
	}
	var all []TxHit
	for i, page := range pages {
		if page.TotalMatches != 5 || page.MatchedBlocks != 3 || page.ToHeight != 3 {
			t.Fatalf("page %d stats drifted: %+v", i, page)
		}
		all = append(all, page.Hits...)
	}
	single, err := index.QueryTxs(TxQuery{MinBlocks: 2, PageSize: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(all, single.Hits) {
		t.Fatalf("paged hits %v do not reconstruct the single-page list %v", all, single.Hits)
	}
}

func TestQueryTxsMinBlocksDescending(t *testing.T) {
	// Descending order walks the same retained occurrences the other way,
	// positions inside a block decreasing; statistics are unchanged.
	index := minBlocksChain(t)
	page, err := index.QueryTxs(TxQuery{MinBlocks: 2, Order: OrderDesc})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "b", Position: 2},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("desc hits=%v, want %v", page.Hits, want)
	}
	if page.TotalMatches != 5 || page.MatchedBlocks != 3 {
		t.Fatalf("desc stats=%+v, want total 5 blocks 3", page)
	}
}

func TestQueryTxsMinBlocksPinnedAcrossPages(t *testing.T) {
	index := minBlocksChain(t)
	base := TxQuery{MinBlocks: 2, PageSize: 2}
	first, err := index.QueryTxs(base)
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}

	t.Run("threshold may resize the page", func(t *testing.T) {
		query := base
		query.Cursor = first.NextCursor
		query.PageSize = 10
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("resized continuation failed: %v", err)
		}
		if len(page.Hits) != 3 || page.TotalMatches != 5 || page.MatchedBlocks != 3 {
			t.Fatalf("unexpected resized page: %+v", page)
		}
	})

	for name, minBlocks := range map[string]int64{
		"threshold raised":   3,
		"threshold lowered":  1,
		"threshold disabled": 0,
	} {
		t.Run(name, func(t *testing.T) {
			query := base
			query.Cursor = first.NextCursor
			query.MinBlocks = minBlocks
			page, err := index.QueryTxs(query)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err=%v, want ErrInvalidArgument", err)
			}
			if len(page.Hits) != 0 || page.NextCursor != "" {
				t.Fatalf("rejected continuation must not return a usable page: %+v", page)
			}
		})
	}

	t.Run("plain cursor rejects an added threshold", func(t *testing.T) {
		plain, err := index.QueryTxs(TxQuery{PageSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		query := TxQuery{PageSize: 2, MinBlocks: 2, Cursor: plain.NextCursor}
		page, err := index.QueryTxs(query)
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("adding a threshold: err=%v, want ErrInvalidArgument", err)
		}
		if len(page.Hits) != 0 || page.NextCursor != "" {
			t.Fatalf("rejected continuation must not return a usable page: %+v", page)
		}
	})

	// The original cursor remains usable after the rejected attempts.
	again := base
	again.Cursor = first.NextCursor
	if _, err := index.QueryTxs(again); err != nil {
		t.Fatalf("valid continuation refused after rejected ones: %v", err)
	}
}

func TestQueryTxsMinBlocksIgnoresBlocksAppendedAfterFirstPage(t *testing.T) {
	// The first page pins heights 1..3. A block appended afterwards holds
	// two more c occurrences, yet c must not qualify retroactively and the
	// new block must not enter the ongoing pagination.
	index := minBlocksChain(t)
	query := TxQuery{MinBlocks: 2, PageSize: 2}
	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	if err := index.Append(Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"c", "c"}}); err != nil {
		t.Fatal(err)
	}
	query.Cursor = first.NextCursor
	second, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	if second.TotalMatches != 5 || second.MatchedBlocks != 3 || second.ToHeight != 3 {
		t.Fatalf("stats changed after append: %+v", second)
	}
	for _, hit := range second.Hits {
		if hit.TxID == "c" || hit.Height > 3 {
			t.Fatalf("appended block leaked into the pinned query: %+v", hit)
		}
	}
}

func TestQueryTxsMinBlocksDiesWhenRangeChanges(t *testing.T) {
	// A reorg replacing a block inside the pinned range invalidates the
	// cursor exactly as without the threshold filter.
	index := minBlocksChain(t)
	first, err := index.QueryTxs(TxQuery{MinBlocks: 2, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := index.Reorg([]Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"b", "c"}, Time: intptr(7)},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a", "d"}},
	}); err != nil {
		t.Fatal(err)
	}
	page, err := index.QueryTxs(TxQuery{MinBlocks: 2, PageSize: 2, Cursor: first.NextCursor})
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("err=%v, want ErrQueryChanged", err)
	}
	if len(page.Hits) != 0 {
		t.Fatalf("changed-range continuation must not return hits: %+v", page)
	}
}
