package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// windowedChain builds the chain from the time-window specification:
// heights 1..4 carry times 105, missing, 100, 110 and transactions
// [a,b,a], [a], [a], [a].
func windowedChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	blocks := []struct {
		txs  []string
		time *int64
	}{
		{[]string{"a", "b", "a"}, intptr(105)},
		{[]string{"a"}, nil},
		{[]string{"a"}, intptr(100)},
		{[]string{"a"}, intptr(110)},
	}
	for i, entry := range blocks {
		height := int64(i + 1)
		parent := "genesis"
		if height > 1 {
			parent = fmt.Sprintf("h%d", height-1)
		}
		block := Block{
			Height: height,
			Hash:   fmt.Sprintf("h%d", height),
			Parent: parent,
			Txs:    entry.txs,
			Time:   entry.time,
		}
		if err := index.Append(block); err != nil {
			t.Fatalf("setup append at %d: %v", height, err)
		}
	}
	return index
}

func TestQueryTxsTimeWindowSpecExample(t *testing.T) {
	// Filter a over [100, 110), ascending, two hits per page. Height 2 has
	// no timestamp and height 4 has time 110 (the excluded end), so only the
	// two a occurrences at height 1 and the one at height 3 survive.
	index := windowedChain(t)
	query := TxQuery{TxIDs: []string{"a"}, TimeStart: intptr(100), TimeEnd: intptr(110), PageSize: 2}

	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 2},
	}
	if !reflect.DeepEqual(first.Hits, wantFirst) {
		t.Fatalf("first page hits=%v, want %v", first.Hits, wantFirst)
	}
	if first.TotalMatches != 3 || first.MatchedBlocks != 2 || first.ToHeight != 4 {
		t.Fatalf("first page stats=%+v, want total 3 blocks 2 to 4", first)
	}
	if first.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}

	query.Cursor = first.NextCursor
	second, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	wantSecond := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(second.Hits, wantSecond) {
		t.Fatalf("second page hits=%v, want %v", second.Hits, wantSecond)
	}
	if second.TotalMatches != 3 || second.MatchedBlocks != 2 || second.ToHeight != 4 {
		t.Fatalf("second page stats=%+v, want total 3 blocks 2 to 4", second)
	}
	if second.NextCursor != "" {
		t.Fatalf("last page must not carry a cursor, got %q", second.NextCursor)
	}
}

func TestQueryTxsTimeWindowDescending(t *testing.T) {
	// Same filtered occurrences as the ascending spec example, read from the
	// pinned upper height downward with positions decreasing inside a block.
	index := windowedChain(t)
	query := TxQuery{TxIDs: []string{"a"}, TimeStart: intptr(100), TimeEnd: intptr(110), PageSize: 2, Order: OrderDesc}

	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 2},
	}
	if !reflect.DeepEqual(first.Hits, wantFirst) {
		t.Fatalf("desc first page=%v, want %v", first.Hits, wantFirst)
	}
	if first.TotalMatches != 3 || first.MatchedBlocks != 2 || first.ToHeight != 4 {
		t.Fatalf("desc stats=%+v", first)
	}

	query.Cursor = first.NextCursor
	second, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	wantSecond := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(second.Hits, wantSecond) {
		t.Fatalf("desc second page=%v, want %v", second.Hits, wantSecond)
	}
	if second.NextCursor != "" {
		t.Fatalf("desc last page carries a cursor: %q", second.NextCursor)
	}
}

func TestQueryTxsTimeWindowBoundaries(t *testing.T) {
	index := timeChain(t, intptr(100), intptr(105), intptr(110))
	query := TxQuery{TimeStart: intptr(100), TimeEnd: intptr(110)}

	page, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	// Start included, end excluded: heights 1 and 2 survive, height 3 does
	// not. Timestamps need not be ordered, but hits still come out by height.
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits=%v, want %v", page.Hits, want)
	}
	if page.TotalMatches != 2 || page.MatchedBlocks != 2 || page.NextCursor != "" {
		t.Fatalf("unexpected page: %+v", page)
	}
}

func TestQueryTxsTimeWindowMissingAndRealZero(t *testing.T) {
	// Height 1 has a real zero timestamp, height 2 none.
	index := timeChain(t, intptr(0), nil)

	t.Run("zero start keeps real zero, rejects missing", func(t *testing.T) {
		page, err := index.QueryTxs(TxQuery{TimeStart: intptr(0), TimeEnd: intptr(1)})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Hits) != 1 || page.Hits[0].Height != 1 {
			t.Fatalf("hits=%v, want only the real-zero height 1", page.Hits)
		}
		if page.TotalMatches != 1 || page.MatchedBlocks != 1 {
			t.Fatalf("stats=%+v, want 1/1", page)
		}
	})

	t.Run("disabled window keeps both", func(t *testing.T) {
		// No pointers: disabled, which must stay distinguishable from a
		// window starting at zero.
		page, err := index.QueryTxs(TxQuery{})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Hits) != 2 || page.TotalMatches != 2 || page.MatchedBlocks != 2 {
			t.Fatalf("disabled window page=%+v, want both occurrences", page)
		}
	})
}

func TestQueryTxsTimeWindowNoMatchesIsEmptyPage(t *testing.T) {
	index := windowedChain(t)
	// A legal window that no timestamp falls into: success, empty page, no
	// continuation cursor.
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, TimeStart: intptr(1000), TimeEnd: intptr(2000)})
	if err != nil {
		t.Fatalf("empty match window errored: %v", err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 {
		t.Fatalf("unexpected matches: %+v", page)
	}
	if page.ToHeight != 4 || page.NextCursor != "" {
		t.Fatalf("empty page should pin the tip and carry no cursor: %+v", page)
	}
}

func TestQueryTxsTimeWindowRejectsInvalidWindows(t *testing.T) {
	index := windowedChain(t)
	for name, query := range map[string]TxQuery{
		"only start":     {TimeStart: intptr(100)},
		"only end":       {TimeEnd: intptr(110)},
		"negative start": {TimeStart: intptr(-1), TimeEnd: intptr(110)},
		"negative end":   {TimeStart: intptr(0), TimeEnd: intptr(-1)},
		"zero width":     {TimeStart: intptr(100), TimeEnd: intptr(100)},
		"inverted width": {TimeStart: intptr(110), TimeEnd: intptr(100)},
		"neg zero width": {TimeStart: intptr(-5), TimeEnd: intptr(-1)},
	} {
		if _, err := index.QueryTxs(query); !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("%s: err=%v, want ErrInvalidArgument", name, err)
		}
	}
}

func TestQueryTxsTimeWindowPinnedAcrossPages(t *testing.T) {
	index := windowedChain(t)
	base := TxQuery{TxIDs: []string{"a"}, TimeStart: intptr(100), TimeEnd: intptr(110), PageSize: 1}
	first, err := index.QueryTxs(base)
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}

	t.Run("window may resize the page", func(t *testing.T) {
		query := base
		query.Cursor = first.NextCursor
		query.PageSize = 10
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("resized continuation failed: %v", err)
		}
		if len(page.Hits) != 2 || page.TotalMatches != 3 || page.MatchedBlocks != 2 {
			t.Fatalf("unexpected resized page: %+v", page)
		}
	})

	for name, mutate := range map[string]func(*TxQuery){
		"window disabled":  func(q *TxQuery) { q.TimeStart, q.TimeEnd = nil, nil },
		"start shifted":    func(q *TxQuery) { q.TimeStart = intptr(101) },
		"end shifted":      func(q *TxQuery) { q.TimeEnd = intptr(111) },
		"start moved to 0": func(q *TxQuery) { q.TimeStart = intptr(0) },
	} {
		t.Run(name, func(t *testing.T) {
			query := base
			query.Cursor = first.NextCursor
			mutate(&query)
			page, err := index.QueryTxs(query)
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("err=%v, want ErrInvalidArgument", err)
			}
			if len(page.Hits) != 0 || page.NextCursor != "" {
				t.Fatalf("rejected continuation must not return a usable page: %+v", page)
			}
		})
	}

	t.Run("plain cursor rejects an added window", func(t *testing.T) {
		plain, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 1})
		if err != nil {
			t.Fatal(err)
		}
		_, err = index.QueryTxs(TxQuery{
			TxIDs: []string{"a"}, PageSize: 1,
			TimeStart: intptr(100), TimeEnd: intptr(110),
			Cursor: plain.NextCursor,
		})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("adding a window: err=%v, want ErrInvalidArgument", err)
		}
	})

	// The original cursor remains usable after the rejected attempts.
	again := base
	again.Cursor = first.NextCursor
	if _, err := index.QueryTxs(again); err != nil {
		t.Fatalf("valid continuation refused after rejected ones: %v", err)
	}
}

func TestQueryTxsTimeWindowDiesWhenAnyRangeBlockChanges(t *testing.T) {
	t.Run("block missed for time becomes another non-match", func(t *testing.T) {
		index := windowedChain(t)
		first, err := index.QueryTxs(TxQuery{
			TxIDs: []string{"a"}, TimeStart: intptr(100), TimeEnd: intptr(110), PageSize: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Height 2 currently has no timestamp and therefore never matches;
		// give it a timestamp that still lies outside the window. The
		// fingerprint changes even though the filtered answer would not.
		if _, err := index.Reorg([]Block{
			{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a"}, Time: intptr(120)},
			{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a"}, Time: intptr(100)},
			{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"a"}, Time: intptr(110)},
		}); err != nil {
			t.Fatal(err)
		}
		page, err := index.QueryTxs(TxQuery{
			TxIDs: []string{"a"}, PageSize: 1,
			TimeStart: intptr(100), TimeEnd: intptr(110),
			Cursor: first.NextCursor,
		})
		if !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
		if len(page.Hits) != 0 {
			t.Fatalf("changed-range continuation must not return hits: %+v", page)
		}
	})

	t.Run("end-excluded block changes transactions", func(t *testing.T) {
		index := windowedChain(t)
		first, err := index.QueryTxs(TxQuery{
			TxIDs: []string{"a"}, TimeStart: intptr(100), TimeEnd: intptr(110), PageSize: 1,
		})
		if err != nil {
			t.Fatal(err)
		}
		// Height 4 sits at the excluded end time 110; rewrite its txs. It
		// stays outside the window, yet the range content changed.
		if _, err := index.Reorg([]Block{
			{Height: 4, Hash: "h4b", Parent: "h3", Txs: []string{"z"}, Time: intptr(110)},
		}); err != nil {
			t.Fatal(err)
		}
		_, err = index.QueryTxs(TxQuery{
			TxIDs: []string{"a"}, PageSize: 1,
			TimeStart: intptr(100), TimeEnd: intptr(110),
			Cursor: first.NextCursor,
		})
		if !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})
}

func TestQueryTxsTimeWindowIntersectsHeightRange(t *testing.T) {
	index := windowedChain(t)
	// Restrict heights to 3..4: only height 3 (time 100) matches; height 4
	// at the excluded end does not.
	page, err := index.QueryTxs(TxQuery{
		From: 3, To: 4, TxIDs: []string{"a"},
		TimeStart: intptr(100), TimeEnd: intptr(110),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 1 || page.Hits[0].Height != 3 {
		t.Fatalf("hits=%v, want only height 3", page.Hits)
	}
	if page.TotalMatches != 1 || page.MatchedBlocks != 1 || page.ToHeight != 4 {
		t.Fatalf("stats=%+v", page)
	}
}

func TestQueryTxsTimeWindowStatsStableAcrossPages(t *testing.T) {
	index := windowedChain(t)
	pages := collectPages(t, index, TxQuery{
		TxIDs:     []string{"a"},
		TimeStart: intptr(100),
		TimeEnd:   intptr(110),
		PageSize:  1,
	})
	if len(pages) != 3 {
		t.Fatalf("pages=%d, want 3", len(pages))
	}
	for i, page := range pages {
		if page.TotalMatches != 3 || page.MatchedBlocks != 2 || page.ToHeight != 4 {
			t.Fatalf("page %d stats drifted: %+v", i, page)
		}
	}
}
