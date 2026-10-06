package indexroom

import (
	"errors"
	"fmt"
	"testing"
)

// windowChain builds a chain with the given per-height timestamps (nil entry
// means a missing timestamp) and per-height transaction lists.
func windowChain(t *testing.T, times []*int64, txs [][]string) *Index {
	t.Helper()
	index := New()
	for i, when := range times {
		height := int64(i + 1)
		parent := "genesis"
		if height > 1 {
			parent = fmt.Sprintf("h%d", height-1)
		}
		block := Block{
			Height: height,
			Hash:   fmt.Sprintf("h%d", height),
			Parent: parent,
			Txs:    txs[i],
			Time:   when,
		}
		if err := index.Append(block); err != nil {
			t.Fatalf("setup append at %d: %v", height, err)
		}
	}
	return index
}

// specChain is the chain from the feature specification: heights 1..4 with
// times 105, missing, 100, 110 and transactions [a,b,a], [a], [a], [a].
func specChain(t *testing.T) *Index {
	return windowChain(t,
		[]*int64{intptr(105), nil, intptr(100), intptr(110)},
		[][]string{{"a", "b", "a"}, {"a"}, {"a"}, {"a"}})
}

func windowQuery(start, end int64) TxQuery {
	return TxQuery{TxIDs: []string{"a"}, PageSize: 2, TimeStart: intptr(start), TimeEnd: intptr(end)}
}

func TestQueryTxsTimeWindowSpecExample(t *testing.T) {
	index := specChain(t)
	query := windowQuery(100, 110)

	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 2},
	}
	assertHits(t, first.Hits, wantFirst)
	if first.TotalMatches != 3 || first.MatchedBlocks != 2 || first.ToHeight != 4 {
		t.Fatalf("first page stats: %+v", first)
	}
	if first.NextCursor == "" {
		t.Fatal("first page should have a continuation cursor")
	}

	query.Cursor = first.NextCursor
	second, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	assertHits(t, second.Hits, []TxHit{{Height: 3, BlockHash: "h3", TxID: "a", Position: 0}})
	if second.TotalMatches != 3 || second.MatchedBlocks != 2 || second.ToHeight != 4 {
		t.Fatalf("second page stats: %+v", second)
	}
	if second.NextCursor != "" {
		t.Fatalf("second page should end the query, got cursor %q", second.NextCursor)
	}
}

func assertHits(t *testing.T, got, want []TxHit) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("hits=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("hit %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestQueryTxsTimeWindowBoundaries(t *testing.T) {
	// Height 1: time 0 (real zero); height 2: missing; height 3: time 99;
	// height 4: time 100; height 5: time 200.
	index := windowChain(t,
		[]*int64{intptr(0), nil, intptr(99), intptr(100), intptr(200)},
		[][]string{{"a"}, {"a"}, {"a"}, {"a"}, {"a"}})

	// Window [0, 100): zero is a real timestamp and the start is inclusive;
	// the missing timestamp never matches; 100 is excluded as the end.
	page, err := index.QueryTxs(TxQuery{TimeStart: intptr(0), TimeEnd: intptr(100)})
	if err != nil {
		t.Fatal(err)
	}
	assertHits(t, page.Hits, []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
	})
	if page.TotalMatches != 2 || page.MatchedBlocks != 2 {
		t.Fatalf("stats: %+v", page)
	}

	// A window starting at zero is not the same as a disabled filter.
	all, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if all.TotalMatches != 5 {
		t.Fatalf("disabled filter should match all five occurrences, got %d", all.TotalMatches)
	}
}

func TestQueryTxsTimeWindowInvalid(t *testing.T) {
	index := specChain(t)
	cases := []struct {
		name  string
		query TxQuery
	}{
		{"only start", TxQuery{TimeStart: intptr(100)}},
		{"only end", TxQuery{TimeEnd: intptr(110)}},
		{"negative start", TxQuery{TimeStart: intptr(-1), TimeEnd: intptr(110)}},
		{"negative end", TxQuery{TimeStart: intptr(100), TimeEnd: intptr(-1)}},
		{"start equals end", TxQuery{TimeStart: intptr(100), TimeEnd: intptr(100)}},
		{"start above end", TxQuery{TimeStart: intptr(110), TimeEnd: intptr(100)}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			page, err := index.QueryTxs(tc.query)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err=%v, want ErrInvalidArgument", err)
			}
			if len(page.Hits) != 0 || page.NextCursor != "" {
				t.Fatalf("invalid window must not yield a usable page: %+v", page)
			}
		})
	}
}

func TestQueryTxsTimeWindowNoMatchIsEmptyPage(t *testing.T) {
	index := specChain(t)
	// A valid window that covers no block timestamp.
	page, err := index.QueryTxs(TxQuery{TimeStart: intptr(200), TimeEnd: intptr(300)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 {
		t.Fatalf("want an empty page, got %+v", page)
	}
	if page.NextCursor != "" {
		t.Fatalf("empty result must not mint a cursor, got %q", page.NextCursor)
	}
	if page.ToHeight != 4 {
		t.Fatalf("ToHeight=%d, want the pinned upper bound 4", page.ToHeight)
	}
}

func TestQueryTxsTimeWindowKeepsHeightOrder(t *testing.T) {
	// Timestamps decrease with height; results must still come out by
	// height and position, never by time.
	index := windowChain(t,
		[]*int64{intptr(300), intptr(200), intptr(100)},
		[][]string{{"a", "a"}, {"a"}, {"a"}})
	query := TxQuery{PageSize: 2, TimeStart: intptr(50), TimeEnd: intptr(350)}

	asc := collectPages(t, index, query)
	var ascHits []TxHit
	for _, page := range asc {
		ascHits = append(ascHits, page.Hits...)
	}
	assertHits(t, ascHits, []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
	})

	query.Order = OrderDesc
	desc := collectPages(t, index, query)
	var descHits []TxHit
	for _, page := range desc {
		if page.TotalMatches != 4 || page.MatchedBlocks != 3 {
			t.Fatalf("desc page stats: %+v", page)
		}
		descHits = append(descHits, page.Hits...)
	}
	assertHits(t, descHits, []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
	})
}

func TestQueryTxsTimeWindowContinuation(t *testing.T) {
	index := specChain(t)
	query := windowQuery(100, 110)
	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}

	// Same window, adjusted page size: the continuation succeeds.
	next := query
	next.Cursor = first.NextCursor
	next.PageSize = 100
	page, err := index.QueryTxs(next)
	if err != nil {
		t.Fatalf("continuation with the same window failed: %v", err)
	}
	assertHits(t, page.Hits, []TxHit{{Height: 3, BlockHash: "h3", TxID: "a", Position: 0}})

	// Changing the window or the enabled state rejects the continuation
	// with ErrInvalidArgument and no usable page.
	mutations := []struct {
		name  string
		query TxQuery
	}{
		{"different start", TxQuery{TxIDs: []string{"a"}, TimeStart: intptr(101), TimeEnd: intptr(110), Cursor: first.NextCursor}},
		{"different end", TxQuery{TxIDs: []string{"a"}, TimeStart: intptr(100), TimeEnd: intptr(111), Cursor: first.NextCursor}},
		{"window dropped", TxQuery{TxIDs: []string{"a"}, Cursor: first.NextCursor}},
	}
	for _, tc := range mutations {
		t.Run(tc.name, func(t *testing.T) {
			page, err := index.QueryTxs(tc.query)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err=%v, want ErrInvalidArgument", err)
			}
			if len(page.Hits) != 0 {
				t.Fatalf("changed window must not yield a usable page: %+v", page)
			}
		})
	}

	// A windowless cursor likewise rejects a continuation that adds one.
	plain, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	_, err = index.QueryTxs(TxQuery{TxIDs: []string{"a"}, TimeStart: intptr(100), TimeEnd: intptr(110), Cursor: plain.NextCursor})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("adding a window on continuation: err=%v, want ErrInvalidArgument", err)
	}
}

func TestQueryTxsTimeWindowCursorDiesOnExcludedBlockChange(t *testing.T) {
	index := specChain(t)
	query := windowQuery(100, 110)
	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	// Height 4 (time 110) is excluded by the window; changing any of its
	// content still invalidates the cursor, and height 2's missing
	// timestamp becoming known is a change too.
	if _, err := index.Reorg([]Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a"}},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a"}, Time: intptr(100)},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"a"}, Time: intptr(111)},
	}); err != nil {
		t.Fatal(err)
	}
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, TimeStart: intptr(100), TimeEnd: intptr(110), Cursor: first.NextCursor})
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("err=%v, want ErrQueryChanged", err)
	}
	if len(page.Hits) != 0 {
		t.Fatalf("changed range must not yield a usable page: %+v", page)
	}
}
