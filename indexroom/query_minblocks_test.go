package indexroom

import (
	"errors"
	"reflect"
	"testing"
)

// minBlocksChain builds the spec example: heights 1..3 hold [a,a,b], [b,c],
// [a,d]. Identifier a appears in blocks {1,3}, b in {1,2}, c and d in one
// block each.
func minBlocksChain(t *testing.T) *Index {
	t.Helper()
	return txChain(t,
		[]string{"a", "a", "b"},
		[]string{"b", "c"},
		[]string{"a", "d"},
	)
}

func TestQueryTxsMinBlocksSpecExample(t *testing.T) {
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
	if page.TotalMatches != 5 || page.MatchedBlocks != 3 {
		t.Fatalf("stats=%d/%d, want 5/3", page.TotalMatches, page.MatchedBlocks)
	}
	if page.NextCursor != "" {
		t.Fatalf("unexpected cursor %q", page.NextCursor)
	}
}

func TestQueryTxsMinBlocksRestrictedRange(t *testing.T) {
	// Only the first two blocks are pinned: a now appears in one block, so
	// just b's two occurrences survive.
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
	if page.TotalMatches != 2 || page.MatchedBlocks != 2 {
		t.Fatalf("stats=%d/%d, want 2/2", page.TotalMatches, page.MatchedBlocks)
	}
	if page.ToHeight != 2 {
		t.Fatalf("toHeight=%d, want 2", page.ToHeight)
	}
}

func TestQueryTxsMinBlocksDescendingAndPaging(t *testing.T) {
	// Page size and read direction must not change the whole-range stats;
	// descending pages walk the same kept occurrences backwards.
	index := minBlocksChain(t)
	pages := collectPages(t, index, TxQuery{MinBlocks: 2, PageSize: 2, Order: OrderDesc})
	if len(pages) != 3 {
		t.Fatalf("pages=%d, want 3", len(pages))
	}
	var hits []TxHit
	for _, page := range pages {
		if page.TotalMatches != 5 || page.MatchedBlocks != 3 {
			t.Fatalf("stats=%d/%d, want 5/3", page.TotalMatches, page.MatchedBlocks)
		}
		hits = append(hits, page.Hits...)
	}
	want := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "b", Position: 2},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(hits, want) {
		t.Fatalf("hits=%v, want %v", hits, want)
	}
}

func TestQueryTxsMinBlocksCombinesWithTxIDsAndTimeWindow(t *testing.T) {
	// Heights 1..4 hold x each; height 2 has no timestamp and height 4 sits
	// at the excluded window end, so x reaches the threshold of two only
	// through heights 1 and 3. y appears in three in-window blocks but is
	// filtered out by TxIDs; z passes the filter yet appears only once.
	index := New()
	blocks := []struct {
		txs  []string
		time *int64
	}{
		{[]string{"x", "y"}, intptr(100)},
		{[]string{"x", "y"}, nil},
		{[]string{"x", "y", "z"}, intptr(105)},
		{[]string{"x"}, intptr(110)},
	}
	for i, entry := range blocks {
		height := int64(i + 1)
		parent := "genesis"
		if height > 1 {
			parent = "h" + string(rune('0'+height-1))
		}
		if err := index.Append(Block{Height: height, Hash: "h" + string(rune('0'+height)), Parent: parent, Txs: entry.txs, Time: entry.time}); err != nil {
			t.Fatalf("setup append at %d: %v", height, err)
		}
	}
	page, err := index.QueryTxs(TxQuery{
		TxIDs:     []string{"x", "z"},
		TimeStart: intptr(100),
		TimeEnd:   intptr(110),
		MinBlocks: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "x", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "x", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits=%v, want %v", page.Hits, want)
	}
	if page.TotalMatches != 2 || page.MatchedBlocks != 2 {
		t.Fatalf("stats=%d/%d, want 2/2", page.TotalMatches, page.MatchedBlocks)
	}
}

func TestQueryTxsMinBlocksNegativeRejected(t *testing.T) {
	index := minBlocksChain(t)
	if _, err := index.QueryTxs(TxQuery{MinBlocks: -1}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("err=%v, want ErrInvalidArgument", err)
	}
}

func TestQueryTxsMinBlocksNoneQualified(t *testing.T) {
	// A legal threshold nobody reaches: a successful empty page with zero
	// statistics and no continuation cursor.
	index := minBlocksChain(t)
	page, err := index.QueryTxs(TxQuery{MinBlocks: 4})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 || page.NextCursor != "" {
		t.Fatalf("page=%+v, want empty with zero stats", page)
	}
}

func TestQueryTxsMinBlocksPinnedAcrossPages(t *testing.T) {
	index := minBlocksChain(t)
	first, err := index.QueryTxs(TxQuery{MinBlocks: 2, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}

	// A changed threshold rejects the continuation as an argument error.
	changed := TxQuery{MinBlocks: 1, PageSize: 2, Cursor: first.NextCursor}
	if _, err := index.QueryTxs(changed); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("changed minBlocks err=%v, want ErrInvalidArgument", err)
	}

	// Blocks appended after the first page must not change qualification or
	// enter the pagination: d still never qualifies, and the remaining hits
	// are exactly the rest of the original five.
	if err := index.Append(Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"d", "d", "c"}}); err != nil {
		t.Fatal(err)
	}
	rest := collectPages(t, index, TxQuery{MinBlocks: 2, PageSize: 2, Cursor: first.NextCursor})
	var hits []TxHit
	hits = append(hits, first.Hits...)
	for _, page := range rest {
		if page.TotalMatches != 5 || page.MatchedBlocks != 3 {
			t.Fatalf("stats=%d/%d, want 5/3", page.TotalMatches, page.MatchedBlocks)
		}
		hits = append(hits, page.Hits...)
	}
	if len(hits) != 5 {
		t.Fatalf("total hits across pages=%d, want 5", len(hits))
	}
	for _, hit := range hits {
		if hit.Height > 3 {
			t.Fatalf("hit from appended block leaked into the page: %+v", hit)
		}
	}
}

func TestQueryTxsMinBlocksZeroKeepsExistingBehavior(t *testing.T) {
	// An explicit zero disables the filter, and a cursor minted without the
	// threshold continues a zero-threshold query.
	index := minBlocksChain(t)
	pages := collectPages(t, index, TxQuery{PageSize: 2})
	var hits []TxHit
	for _, page := range pages {
		if page.TotalMatches != 7 || page.MatchedBlocks != 3 {
			t.Fatalf("stats=%d/%d, want 7/3", page.TotalMatches, page.MatchedBlocks)
		}
		hits = append(hits, page.Hits...)
	}
	if len(hits) != 7 {
		t.Fatalf("hits=%d, want 7", len(hits))
	}
}
