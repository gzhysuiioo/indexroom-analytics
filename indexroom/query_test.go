package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// txChain builds a chain with one block per tx list, hashes h1..hN.
func txChain(t *testing.T, txs ...[]string) *Index {
	t.Helper()
	index := New()
	for i, blockTxs := range txs {
		height := int64(i + 1)
		parent := "genesis"
		if height > 1 {
			parent = fmt.Sprintf("h%d", height-1)
		}
		block := Block{Height: height, Hash: fmt.Sprintf("h%d", height), Parent: parent, Txs: blockTxs}
		if err := index.Append(block); err != nil {
			t.Fatalf("setup append at height %d: %v", height, err)
		}
	}
	return index
}

// collectPages follows the cursor chain to the end and returns every page.
func collectPages(t *testing.T, index *Index, query TxQuery) []TxPage {
	t.Helper()
	var pages []TxPage
	for {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		pages = append(pages, page)
		if page.NextCursor == "" {
			return pages
		}
		query.Cursor = page.NextCursor
	}
}

func TestQueryTxsReturnsOrderedOccurrences(t *testing.T) {
	index := txChain(t,
		[]string{"t1", "t2", "t1"},
		[]string{},
		[]string{"t3", "t1"},
	)
	page, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "t1", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "t2", Position: 1},
		{Height: 1, BlockHash: "h1", TxID: "t1", Position: 2},
		{Height: 3, BlockHash: "h3", TxID: "t3", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "t1", Position: 1},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits=%v, want %v", page.Hits, want)
	}
	if page.TotalMatches != 5 {
		t.Fatalf("total=%d, want 5", page.TotalMatches)
	}
	if page.MatchedBlocks != 2 {
		t.Fatalf("matched blocks=%d, want 2", page.MatchedBlocks)
	}
	if page.ToHeight != 3 {
		t.Fatalf("to=%d, want pinned tip 3", page.ToHeight)
	}
	if page.NextCursor != "" {
		t.Fatalf("unexpected cursor %q", page.NextCursor)
	}
}

func TestQueryTxsHeightRangeIsInclusive(t *testing.T) {
	index := txChain(t,
		[]string{"a"},
		[]string{"b"},
		[]string{"c"},
		[]string{"d"},
	)
	page, err := index.QueryTxs(TxQuery{From: 2, To: 3})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "c", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits=%v, want %v", page.Hits, want)
	}
	if page.TotalMatches != 2 || page.MatchedBlocks != 2 || page.ToHeight != 3 {
		t.Fatalf("unexpected stats: %+v", page)
	}
}

func TestQueryTxsIDSetMatchesAnyAndIgnoresOrder(t *testing.T) {
	index := txChain(t,
		[]string{"t1", "t2"},
		[]string{"t3", "t1"},
	)
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"t1", "t3"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "t1", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "t3", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "t1", Position: 1},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits=%v, want %v", page.Hits, want)
	}
	// Reordered and duplicated identifiers describe the same filter.
	again, err := index.QueryTxs(TxQuery{TxIDs: []string{"t3", "t1", "t1", "t3"}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again, page) {
		t.Fatalf("reordered set changed the result: %+v vs %+v", again, page)
	}
	// An identifier absent from the chain matches nothing.
	none, err := index.QueryTxs(TxQuery{TxIDs: []string{"nope"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Hits) != 0 || none.TotalMatches != 0 || none.MatchedBlocks != 0 {
		t.Fatalf("unexpected matches for unknown id: %+v", none)
	}
}

func TestQueryTxsDefaultsAndClamping(t *testing.T) {
	index := txChain(t,
		[]string{"a"},
		[]string{"b"},
	)
	// To above the tip is clamped to the tip seen by the first page.
	page, err := index.QueryTxs(TxQuery{From: 1, To: 100})
	if err != nil {
		t.Fatal(err)
	}
	if page.ToHeight != 2 || page.TotalMatches != 2 {
		t.Fatalf("unexpected page: %+v", page)
	}
	// Both bounds omitted: from height 1 to the tip.
	page, err = index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if page.ToHeight != 2 || page.TotalMatches != 2 {
		t.Fatalf("unexpected page: %+v", page)
	}
}

func TestQueryTxsRejectsInvalidArguments(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"})
	for name, query := range map[string]TxQuery{
		"negative from":  {From: -1},
		"negative to":    {To: -2},
		"to below from":  {From: 3, To: 2},
		"page too small": {PageSize: -1},
		"page too large": {PageSize: MaxPageSize + 1},
	} {
		if _, err := index.QueryTxs(query); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err=%v, want ErrInvalidArgument", name, err)
		}
	}
	// Boundary page sizes are accepted.
	for _, size := range []int{0, 1, MaxPageSize} {
		if _, err := index.QueryTxs(TxQuery{PageSize: size}); err != nil {
			t.Errorf("page size %d refused: %v", size, err)
		}
	}
}

func TestQueryTxsEmptyRangesSucceed(t *testing.T) {
	empty := New()
	page, err := empty.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatalf("empty index query failed: %v", err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.ToHeight != 0 || page.NextCursor != "" {
		t.Fatalf("unexpected empty-index page: %+v", page)
	}

	index := txChain(t, []string{"a"})
	page, err = index.QueryTxs(TxQuery{From: 5})
	if err != nil {
		t.Fatalf("start above tip failed: %v", err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.ToHeight != 1 || page.NextCursor != "" {
		t.Fatalf("unexpected out-of-range page: %+v", page)
	}
}

func TestQueryTxsPaginationCoversRange(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b", "c"},
		[]string{"a", "a"},
		[]string{},
		[]string{"b", "c", "a", "a"},
		[]string{"c"},
	)
	full, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if full.TotalMatches != 10 || full.MatchedBlocks != 4 {
		t.Fatalf("unexpected totals: %+v", full)
	}

	var all []TxHit
	pages := collectPages(t, index, TxQuery{PageSize: 4})
	if len(pages) != 3 {
		t.Fatalf("pages=%d, want 3", len(pages))
	}
	for i, page := range pages {
		// Statistics describe the whole range and never change between pages.
		if page.TotalMatches != 10 || page.MatchedBlocks != 4 || page.ToHeight != 5 {
			t.Fatalf("page %d stats changed: %+v", i, page)
		}
		if i < len(pages)-1 && len(page.Hits) != 4 {
			t.Fatalf("page %d short: %d hits", i, len(page.Hits))
		}
		all = append(all, page.Hits...)
	}
	if !reflect.DeepEqual(all, full.Hits) {
		t.Fatalf("paginated hits differ from single-page hits:\n%v\n%v", all, full.Hits)
	}
}

func TestQueryTxsContinuationPinsTheRange(t *testing.T) {
	index := txChain(t,
		[]string{"a"},
		[]string{"a"},
		[]string{"a"},
	)
	first, err := index.QueryTxs(TxQuery{PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" || first.ToHeight != 3 {
		t.Fatalf("unexpected first page: %+v", first)
	}
	// New blocks appended after the first page must not enter this query.
	for _, height := range []int64{4, 5} {
		block := Block{Height: height, Hash: fmt.Sprintf("h%d", height), Parent: fmt.Sprintf("h%d", height-1), Txs: []string{"a"}}
		if err := index.Append(block); err != nil {
			t.Fatal(err)
		}
	}
	second, err := index.QueryTxs(TxQuery{PageSize: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{{Height: 3, BlockHash: "h3", TxID: "a", Position: 0}}
	if !reflect.DeepEqual(second.Hits, want) {
		t.Fatalf("hits=%v, want %v", second.Hits, want)
	}
	if second.TotalMatches != 3 || second.ToHeight != 3 || second.NextCursor != "" {
		t.Fatalf("continuation saw new blocks: %+v", second)
	}
}

func TestQueryTxsContinuationMayResizePage(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b"},
		[]string{"c"},
		[]string{"d", "e"},
	)
	first, err := index.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	rest, err := index.QueryTxs(TxQuery{PageSize: MaxPageSize, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Hits) != 4 || rest.NextCursor != "" || rest.TotalMatches != 5 {
		t.Fatalf("unexpected resized page: %+v", rest)
	}
}

func TestQueryTxsContinuationAcceptsEquivalentSet(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b"},
		[]string{"a", "b"},
	)
	first, err := index.QueryTxs(TxQuery{TxIDs: []string{"a", "b", "b"}, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	// Same set, reordered and deduplicated: still the same filter.
	second, err := index.QueryTxs(TxQuery{TxIDs: []string{"b", "a"}, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("equivalent set refused: %v", err)
	}
	if len(second.Hits) != 2 || second.NextCursor != "" {
		t.Fatalf("unexpected continuation: %+v", second)
	}
}

func TestQueryTxsContinuationRejectsChangedConditions(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b"},
		[]string{"a"},
		[]string{"a"},
	)
	first, err := index.QueryTxs(TxQuery{From: 2, TxIDs: []string{"a"}, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	for name, query := range map[string]TxQuery{
		"different from":   {From: 1, TxIDs: []string{"a"}, Cursor: first.NextCursor},
		"to now explicit":  {From: 2, To: 3, TxIDs: []string{"a"}, Cursor: first.NextCursor},
		"different set":    {From: 2, TxIDs: []string{"b"}, Cursor: first.NextCursor},
		"wider set":        {From: 2, TxIDs: []string{"a", "b"}, Cursor: first.NextCursor},
		"filter now empty": {From: 2, Cursor: first.NextCursor},
	} {
		if _, err := index.QueryTxs(query); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err=%v, want ErrInvalidArgument", name, err)
		}
	}
	// The valid cursor still works afterwards.
	if _, err := index.QueryTxs(TxQuery{From: 2, TxIDs: []string{"a"}, Cursor: first.NextCursor}); err != nil {
		t.Fatalf("valid continuation refused after rejected ones: %v", err)
	}
}

func TestQueryTxsCursorSurvivesChangesOutsideRange(t *testing.T) {
	index := txChain(t,
		[]string{"a", "a"},
		[]string{"a"},
		[]string{"b"},
		[]string{"b"},
	)
	first, err := index.QueryTxs(TxQuery{From: 1, To: 2, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if first.TotalMatches != 3 || first.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", first)
	}
	cursor := first.NextCursor

	// Duplicate append of the tip: a successful no-op.
	if err := index.Append(Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"b"}}); err != nil {
		t.Fatal(err)
	}
	// Append beyond the pinned range.
	if err := index.Append(Block{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"a"}}); err != nil {
		t.Fatal(err)
	}
	// Reorg confined to heights above the pinned range.
	if _, err := index.Reorg([]Block{
		{Height: 4, Hash: "h4b", Parent: "h3", Txs: []string{"x"}},
		{Height: 5, Hash: "h5b", Parent: "h4b"},
	}); err != nil {
		t.Fatal(err)
	}
	// Replay of the identical branch, including an in-range block.
	if _, err := index.Reorg([]Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a"}},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"b"}},
	}); err != nil {
		t.Fatal(err)
	}

	pages := collectPages(t, index, TxQuery{From: 1, To: 2, PageSize: 1, Cursor: cursor})
	var all []TxHit
	for _, page := range pages {
		if page.TotalMatches != 3 || page.ToHeight != 2 {
			t.Fatalf("continuation stats changed: %+v", page)
		}
		all = append(all, page.Hits...)
	}
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("remaining hits=%v, want %v", all, want)
	}
}

func TestQueryTxsCursorDiesWithRange(t *testing.T) {
	build := func(t *testing.T) (*Index, string) {
		t.Helper()
		index := txChain(t,
			[]string{"a"},
			[]string{"b"},
			[]string{"c"},
		)
		first, err := index.QueryTxs(TxQuery{PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		if first.NextCursor == "" {
			t.Fatal("expected a continuation cursor")
		}
		return index, first.NextCursor
	}

	t.Run("block replaced", func(t *testing.T) {
		index, cursor := build(t)
		if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"b"}}}); err != nil {
			t.Fatal(err)
		}
		if _, err := index.QueryTxs(TxQuery{Cursor: cursor}); !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})

	t.Run("same hash different txs", func(t *testing.T) {
		index, cursor := build(t)
		// The block keeps its hash but its transaction content changes.
		if _, err := index.Reorg([]Block{
			{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"z"}},
			{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"c"}},
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := index.QueryTxs(TxQuery{Cursor: cursor}); !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})

	t.Run("chain shortened", func(t *testing.T) {
		index, cursor := build(t)
		if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := index.QueryTxs(TxQuery{Cursor: cursor}); !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})

	t.Run("index stays usable", func(t *testing.T) {
		index, cursor := build(t)
		if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1"}}); err != nil {
			t.Fatal(err)
		}
		if _, err := index.QueryTxs(TxQuery{Cursor: cursor}); !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
		// A fresh first-page query against the new chain state works.
		page, err := index.QueryTxs(TxQuery{})
		if err != nil {
			t.Fatalf("fresh query after reorg failed: %v", err)
		}
		if page.ToHeight != 2 || page.TotalMatches != 1 {
			t.Fatalf("unexpected fresh page: %+v", page)
		}
	})
}

func TestQueryTxsRejectsBadCursors(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"})
	first, err := index.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	good := first.NextCursor
	if good == "" {
		t.Fatal("expected a continuation cursor")
	}

	tampered := []byte(good)
	mid := len(tampered) / 2
	if tampered[mid] == 'A' {
		tampered[mid] = 'B'
	} else {
		tampered[mid] = 'A'
	}
	for name, cursor := range map[string]string{
		"garbage":   "not-a-cursor",
		"truncated": good[:len(good)-2],
		"tampered":  string(tampered),
	} {
		if _, err := index.QueryTxs(TxQuery{Cursor: cursor}); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err=%v, want ErrInvalidArgument", name, err)
		}
	}

	// A cursor minted by another index instance is an argument error, even
	// when the other index holds identical data.
	other := txChain(t, []string{"a"}, []string{"b"})
	if _, err := other.QueryTxs(TxQuery{Cursor: good}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("foreign cursor: err=%v, want ErrInvalidArgument", err)
	}

	// None of the failures invalidated the genuine cursor.
	if _, err := index.QueryTxs(TxQuery{Cursor: good}); err != nil {
		t.Fatalf("valid cursor refused after rejected ones: %v", err)
	}
}

func TestQueryTxsSameCursorSamePage(t *testing.T) {
	index := txChain(t,
		[]string{"a"},
		[]string{"b"},
		[]string{"c"},
	)
	first, err := index.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	pageA, err := index.QueryTxs(TxQuery{PageSize: 1, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	pageB, err := index.QueryTxs(TxQuery{PageSize: 1, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pageA, pageB) {
		t.Fatalf("repeated cursor returned different pages:\n%+v\n%+v", pageA, pageB)
	}
}

func TestQueryTxsReturnedHitsAreCopies(t *testing.T) {
	index := txChain(t, []string{"a", "b"})
	page, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	page.Hits[0].TxID = "mutated"
	page.Hits[0].BlockHash = "mutated"
	page.Hits[0].Height = 99
	again, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	want := TxHit{Height: 1, BlockHash: "h1", TxID: "a", Position: 0}
	if again.Hits[0] != want {
		t.Fatalf("caller mutation leaked into the index: %+v", again.Hits[0])
	}
}

func TestQueryTxsDoesNotMutateIndex(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b"},
		[]string{"c"},
	)
	blocks, byHash, tip := snapshot(index)
	for _, page := range collectPages(t, index, TxQuery{PageSize: 1}) {
		_ = page
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

func TestQueryTxsConcurrentWithAppends(t *testing.T) {
	index := chain(t, Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"tx-1"}})
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
				Height: height,
				Hash:   fmt.Sprintf("h%d", height),
				Parent: fmt.Sprintf("h%d", height-1),
				Txs:    []string{fmt.Sprintf("tx-%d", height)},
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
				// Appends above the pinned range never invalidate a query,
				// so every pagination run must complete and stay consistent.
				pages := 0
				seen := 0
				var total, matched, to int64 = -1, -1, -1
				query := TxQuery{PageSize: 7}
				var lastHeight int64
				var lastPosition = -1
				for {
					page, err := index.QueryTxs(query)
					if err != nil {
						t.Errorf("query failed during appends: %v", err)
						return
					}
					if total == -1 {
						total, matched, to = page.TotalMatches, page.MatchedBlocks, page.ToHeight
					} else if page.TotalMatches != total || page.MatchedBlocks != matched || page.ToHeight != to {
						t.Errorf("stats changed mid-pagination: %+v", page)
						return
					}
					for _, hit := range page.Hits {
						if hit.Height < lastHeight || (hit.Height == lastHeight && hit.Position <= lastPosition) {
							t.Errorf("hits out of order after (%d,%d): %+v", lastHeight, lastPosition, hit)
							return
						}
						if hit.Height > page.ToHeight {
							t.Errorf("hit above pinned bound: %+v vs %d", hit, page.ToHeight)
							return
						}
						lastHeight, lastPosition = hit.Height, hit.Position
					}
					seen += len(page.Hits)
					pages++
					if page.NextCursor == "" {
						break
					}
					query.Cursor = page.NextCursor
				}
				if int64(seen) != total {
					t.Errorf("paginated %d hits over %d pages, total says %d", seen, pages, total)
					return
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	writer.Wait()
}
