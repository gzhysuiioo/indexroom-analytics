package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// reversedHits returns a copy of hits in reverse order.
func reversedHits(hits []TxHit) []TxHit {
	out := make([]TxHit, len(hits))
	for i, hit := range hits {
		out[len(hits)-1-i] = hit
	}
	return out
}

// TestQueryTxsReverseSpecExample is the worked example from the product
// request: heights 1..3 hold [a,b,a], [a,c], [b,a]; filtering a with two
// hits per page must page from the chain top downward, finishing in two
// pages, while every page reports the whole-range totals 4/3.
func TestQueryTxsReverseSpecExample(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b", "a"},
		[]string{"a", "c"},
		[]string{"b", "a"},
	)
	query := TxQuery{TxIDs: []string{"a"}, PageSize: 2, Reverse: true}

	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(first.Hits, wantFirst) {
		t.Fatalf("first page hits=%v, want %v", first.Hits, wantFirst)
	}
	if first.TotalMatches != 4 || first.MatchedBlocks != 3 || first.ToHeight != 3 || first.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", first)
	}

	query.Cursor = first.NextCursor
	second, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	wantSecond := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 2},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(second.Hits, wantSecond) {
		t.Fatalf("second page hits=%v, want %v", second.Hits, wantSecond)
	}
	if second.NextCursor != "" {
		t.Fatalf("second page must be the last, cursor=%q", second.NextCursor)
	}
	for i, page := range []TxPage{first, second} {
		if page.TotalMatches != 4 || page.MatchedBlocks != 3 || page.ToHeight != 3 {
			t.Fatalf("page %d statistics changed: %+v", i, page)
		}
	}
}

// TestQueryTxsReverseIsReversedAscendingOrder pins the core relation: the
// reverse answer over the same range/filter is exactly the ascending answer
// read backwards, with original positions intact and duplicates retained.
func TestQueryTxsReverseIsReversedAscendingOrder(t *testing.T) {
	index := txChain(t,
		[]string{"t1", "t2", "t1"},
		[]string{},
		[]string{"t3", "t1"},
	)
	asc, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := index.QueryTxs(TxQuery{Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(rev.Hits, reversedHits(asc.Hits)) {
		t.Fatalf("reverse hits=%v, want ascending backwards %v", rev.Hits, reversedHits(asc.Hits))
	}
	if rev.TotalMatches != asc.TotalMatches || rev.MatchedBlocks != asc.MatchedBlocks ||
		rev.ToHeight != asc.ToHeight || rev.NextCursor != "" {
		t.Fatalf("reverse page metadata differs: %+v vs %+v", rev, asc)
	}

	// The same must hold for a selective filter with repeats in one block.
	for _, txIDs := range [][]string{{"t1"}, {"t1", "t3"}, nil} {
		asc, err := index.QueryTxs(TxQuery{TxIDs: txIDs, PageSize: MaxPageSize})
		if err != nil {
			t.Fatal(err)
		}
		rev, err := index.QueryTxs(TxQuery{TxIDs: txIDs, PageSize: MaxPageSize, Reverse: true})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(rev.Hits, reversedHits(asc.Hits)) {
			t.Fatalf("filter %q: reverse=%v want %v", txIDs, rev.Hits, reversedHits(asc.Hits))
		}
		if rev.TotalMatches != asc.TotalMatches || rev.MatchedBlocks != asc.MatchedBlocks {
			t.Fatalf("filter %q: direction changed statistics", txIDs)
		}
	}
}

// TestQueryTxsReversePaginationCoversRange walks every sensible page size
// and proves the stitched pages equal one single-page reverse read: no
// occurrence is duplicated or skipped, and statistics never move.
func TestQueryTxsReversePaginationCoversRange(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b", "c"},
		[]string{"a", "a"},
		[]string{},
		[]string{"b", "c", "a", "a"},
		[]string{"c"},
	)
	full, err := index.QueryTxs(TxQuery{Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	if full.TotalMatches != 10 || full.MatchedBlocks != 4 {
		t.Fatalf("unexpected totals: %+v", full)
	}

	for _, pageSize := range []int{1, 2, 3, 4, 7, MaxPageSize} {
		var all []TxHit
		pages := collectPages(t, index, TxQuery{PageSize: pageSize, Reverse: true})
		for i, page := range pages {
			if page.TotalMatches != 10 || page.MatchedBlocks != 4 || page.ToHeight != 5 {
				t.Fatalf("pageSize=%d page %d statistics changed: %+v", pageSize, i, page)
			}
			if i < len(pages)-1 && len(page.Hits) != pageSize {
				t.Fatalf("pageSize=%d page %d short: %d hits", pageSize, i, len(page.Hits))
			}
			all = append(all, page.Hits...)
		}
		if !reflect.DeepEqual(all, full.Hits) {
			t.Fatalf("pageSize=%d stitched hits differ from one-page read:\n%v\n%v", pageSize, all, full.Hits)
		}
		// The stitched walk must be strictly height/position descending.
		for i := 1; i < len(all); i++ {
			prev, cur := all[i-1], all[i]
			if cur.Height > prev.Height || (cur.Height == prev.Height && cur.Position >= prev.Position) {
				t.Fatalf("pageSize=%d hits not in descending order at %d: %+v after %+v",
					pageSize, i, cur, prev)
			}
		}
	}
}

// TestQueryTxsReversePositionsAreOriginal checks that descending through a
// block never renumbers positions: a block [a,b,a] reports positions 2 and
// 0 for a, not 1 and 0.
func TestQueryTxsReversePositionsAreOriginal(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b", "a"},
		[]string{"a"},
	)
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 2},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits=%v, want original positions %v", page.Hits, want)
	}
}

// TestQueryTxsReverseRangeIsInclusive pins From/To handling in reverse.
func TestQueryTxsReverseRangeIsInclusive(t *testing.T) {
	index := txChain(t,
		[]string{"a"},
		[]string{"b"},
		[]string{"c"},
		[]string{"d"},
	)
	page, err := index.QueryTxs(TxQuery{From: 2, To: 3, Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "c", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits=%v, want %v", page.Hits, want)
	}
	if page.TotalMatches != 2 || page.MatchedBlocks != 2 || page.ToHeight != 3 {
		t.Fatalf("unexpected stats: %+v", page)
	}
}

// TestQueryTxsReverseEmptyCasesSucceed covers the empty chain, a start above
// the tip, and a filter that matches nothing: all are successful empty
// pages with an empty continuation cursor in reverse too.
func TestQueryTxsReverseEmptyCasesSucceed(t *testing.T) {
	empty := New()
	page, err := empty.QueryTxs(TxQuery{Reverse: true})
	if err != nil {
		t.Fatalf("empty index reverse query failed: %v", err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.ToHeight != 0 || page.NextCursor != "" {
		t.Fatalf("unexpected empty-index reverse page: %+v", page)
	}

	index := txChain(t, []string{"a"})
	page, err = index.QueryTxs(TxQuery{From: 5, Reverse: true})
	if err != nil {
		t.Fatalf("start above tip failed: %v", err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.ToHeight != 1 || page.NextCursor != "" {
		t.Fatalf("unexpected out-of-range reverse page: %+v", page)
	}

	page, err = index.QueryTxs(TxQuery{TxIDs: []string{"zzz"}, Reverse: true})
	if err != nil {
		t.Fatalf("no-match query failed: %v", err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 || page.NextCursor != "" {
		t.Fatalf("unexpected no-match reverse page: %+v", page)
	}
}

// TestQueryTxsReverseContinuationMayResizePage checks that page size may
// change between reverse pages without losing or repeating occurrences.
func TestQueryTxsReverseContinuationMayResizePage(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b"},
		[]string{"c"},
		[]string{"d", "e"},
	)
	first, err := index.QueryTxs(TxQuery{PageSize: 1, Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	rest, err := index.QueryTxs(TxQuery{PageSize: MaxPageSize, Reverse: true, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Hits) != 4 || rest.NextCursor != "" || rest.TotalMatches != 5 {
		t.Fatalf("unexpected resized reverse page: %+v", rest)
	}
	stitched := append(append([]TxHit{}, first.Hits...), rest.Hits...)
	full, err := index.QueryTxs(TxQuery{PageSize: MaxPageSize, Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stitched, full.Hits) {
		t.Fatalf("resized walk=%v, want %v", stitched, full.Hits)
	}
}

// TestQueryTxsReverseContinuationPinsTheRange ensures blocks appended after
// the first reverse page cannot enter the ongoing pagination, just like an
// ascending query; a fresh empty-cursor query is required to see them.
func TestQueryTxsReverseContinuationPinsTheRange(t *testing.T) {
	index := txChain(t,
		[]string{"a"},
		[]string{"a"},
		[]string{"a"},
	)
	first, err := index.QueryTxs(TxQuery{PageSize: 2, Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" || first.ToHeight != 3 {
		t.Fatalf("unexpected first page: %+v", first)
	}
	wantFirst := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(first.Hits, wantFirst) {
		t.Fatalf("first hits=%v, want %v", first.Hits, wantFirst)
	}
	for _, height := range []int64{4, 5} {
		block := Block{Height: height, Hash: fmt.Sprintf("h%d", height), Parent: fmt.Sprintf("h%d", height-1), Txs: []string{"a"}}
		if err := index.Append(block); err != nil {
			t.Fatal(err)
		}
	}
	second, err := index.QueryTxs(TxQuery{PageSize: 2, Reverse: true, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	wantSecond := []TxHit{{Height: 1, BlockHash: "h1", TxID: "a", Position: 0}}
	if !reflect.DeepEqual(second.Hits, wantSecond) {
		t.Fatalf("second hits=%v, want %v", second.Hits, wantSecond)
	}
	if second.TotalMatches != 3 || second.ToHeight != 3 || second.NextCursor != "" {
		t.Fatalf("continuation saw appended blocks: %+v", second)
	}

	// Restarting with an empty cursor re-pins at the new tip and reads it.
	fresh, err := index.QueryTxs(TxQuery{PageSize: 1, Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Hits[0].Height != 5 || fresh.ToHeight != 5 || fresh.TotalMatches != 5 {
		t.Fatalf("fresh reverse query did not see appended blocks: %+v", fresh)
	}
}

// TestQueryTxsReverseRejectsDirectionSwitch pins the direction rule in both
// directions: mismatching direction is ErrInvalidArgument with no page, and
// the untouched cursor continues to work afterwards.
func TestQueryTxsReverseRejectsDirectionSwitch(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b"},
		[]string{"a"},
		[]string{"a"},
	)
	asc, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 1, Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	if asc.NextCursor == "" || rev.NextCursor == "" {
		t.Fatal("expected continuation cursors")
	}

	// Ascending cursor presented with Reverse set.
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 1, Reverse: true, Cursor: asc.NextCursor})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("asc cursor + Reverse: err=%v, want ErrInvalidArgument", err)
	}
	if !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("mismatched direction returned page results: %+v", page)
	}
	// Reverse cursor presented as ascending.
	page, err = index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 1, Cursor: rev.NextCursor})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("reverse cursor without Reverse: err=%v, want ErrInvalidArgument", err)
	}
	if !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("mismatched direction returned page results: %+v", page)
	}

	// Neither rejection consumed the cursor: both continuations still work.
	if _, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 1, Cursor: asc.NextCursor}); err != nil {
		t.Fatalf("ascending cursor no longer valid: %v", err)
	}
	if _, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 1, Reverse: true, Cursor: rev.NextCursor}); err != nil {
		t.Fatalf("reverse cursor no longer valid: %v", err)
	}
}

// TestQueryTxsReverseCursorRecordsDirection inspects the signed payload: a
// reverse cursor carries Rev=true, while ascending cursors stay byte-shaped
// like the pre-reverse encoding (the rev field is omitted when false).
func TestQueryTxsReverseCursorRecordsDirection(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"a"}, []string{"a"})
	asc, err := index.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	rev, err := index.QueryTxs(TxQuery{PageSize: 1, Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	ascPayload := decodePayload(t, index, asc.NextCursor)
	revPayload := decodePayload(t, index, rev.NextCursor)
	if ascPayload.Rev {
		t.Fatalf("ascending cursor payload=%+v must not carry rev", ascPayload)
	}
	if !revPayload.Rev {
		t.Fatalf("reverse cursor payload=%+v must carry rev=true", revPayload)
	}
	if revPayload.Off != 1 || ascPayload.Off != 1 {
		t.Fatalf("unexpected offsets: asc=%d rev=%d", ascPayload.Off, revPayload.Off)
	}
}

// TestQueryTxsReverseEquivalentSetContinuation checks set-equivalence
// semantics on a reverse continuation as well.
func TestQueryTxsReverseEquivalentSetContinuation(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b"},
		[]string{"a", "b"},
	)
	first, err := index.QueryTxs(TxQuery{TxIDs: []string{"a", "b", "b"}, PageSize: 2, Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := index.QueryTxs(TxQuery{TxIDs: []string{"b", "a"}, PageSize: 2, Reverse: true, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("equivalent set refused on reverse continuation: %v", err)
	}
	if len(second.Hits) != 2 || second.NextCursor != "" {
		t.Fatalf("unexpected reverse continuation: %+v", second)
	}
	stitched := append(append([]TxHit{}, first.Hits...), second.Hits...)
	full, err := index.QueryTxs(TxQuery{PageSize: MaxPageSize, Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(stitched, full.Hits) {
		t.Fatalf("stitched=%v, want %v", stitched, full.Hits)
	}
}

// TestQueryTxsReverseCursorDiesWithRange applies the existing change
// semantics to reverse queries: in-range replacement (even same hash,
// timestamp-only change) or chain shortening gives ErrQueryChanged, while a
// change above the pinned range leaves the cursor valid.
func TestQueryTxsReverseCursorDiesWithRange(t *testing.T) {
	t.Run("block replaced", func(t *testing.T) {
		index := txChain(t, []string{"a"}, []string{"b"}, []string{"c"})
		first, _ := index.QueryTxs(TxQuery{PageSize: 1, Reverse: true})
		if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"b"}}}); err != nil {
			t.Fatal(err)
		}
		_, err := index.QueryTxs(TxQuery{PageSize: 1, Reverse: true, Cursor: first.NextCursor})
		if !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})

	t.Run("timestamp only changes", func(t *testing.T) {
		index := timeChain(t, intptr(10), intptr(20), intptr(30))
		first, _ := index.QueryTxs(TxQuery{PageSize: 1, Reverse: true})
		if _, err := index.Reorg([]Block{
			{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a"}, Time: intptr(999)},
			{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a"}, Time: intptr(30)},
		}); err != nil {
			t.Fatal(err)
		}
		_, err := index.QueryTxs(TxQuery{PageSize: 1, Reverse: true, Cursor: first.NextCursor})
		if !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})

	t.Run("chain shortened below bound", func(t *testing.T) {
		index := txChain(t, []string{"a"}, []string{"b"}, []string{"c"})
		first, _ := index.QueryTxs(TxQuery{PageSize: 1, Reverse: true})
		if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1"}}); err != nil {
			t.Fatal(err)
		}
		_, err := index.QueryTxs(TxQuery{PageSize: 1, Reverse: true, Cursor: first.NextCursor})
		if !errors.Is(err, ErrQueryChanged) {
			t.Fatalf("err=%v, want ErrQueryChanged", err)
		}
	})

	t.Run("change above pinned range survives", func(t *testing.T) {
		index := txChain(t,
			[]string{"a", "a"},
			[]string{"a"},
			[]string{"b"},
			[]string{"b"},
		)
		first, err := index.QueryTxs(TxQuery{From: 1, To: 2, PageSize: 1, Reverse: true})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := index.Reorg([]Block{
			{Height: 4, Hash: "h4b", Parent: "h3", Txs: []string{"x"}},
			{Height: 5, Hash: "h5b", Parent: "h4b"},
		}); err != nil {
			t.Fatal(err)
		}
		pages := collectPages(t, index, TxQuery{From: 1, To: 2, PageSize: 1, Reverse: true, Cursor: first.NextCursor})
		var all []TxHit
		for _, page := range pages {
			if page.TotalMatches != 3 || page.ToHeight != 2 {
				t.Fatalf("continuation stats changed: %+v", page)
			}
			all = append(all, page.Hits...)
		}
		// Full reverse answer over heights 1..2: h2 pos0, h1 pos1, h1 pos0;
		// the first page already delivered h2 pos0.
		want := []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
			{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		}
		if !reflect.DeepEqual(all, want) {
			t.Fatalf("remaining reverse hits=%v, want %v", all, want)
		}
	})
}

// TestQueryTxsReverseIgnoresTimestampShape pins the requirement that
// ordering comes only from height and in-block position: missing, equal, and
// height-decreasing timestamps produce the same reverse page as a chain
// carrying no timestamps at all.
func TestQueryTxsReverseIgnoresTimestampShape(t *testing.T) {
	noTimes := txChain(t, []string{"a", "b"}, []string{"a"}, []string{"b", "a"})
	want, err := noTimes.QueryTxs(TxQuery{Reverse: true, PageSize: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}

	cases := map[string][]*int64{
		"all missing":     {nil, nil, nil},
		"equal times":     {intptr(7), intptr(7), intptr(7)},
		"decreasing":      {intptr(300), intptr(200), intptr(100)},
		"mixed presence":  {nil, intptr(5), nil},
		"zero is present": {intptr(0), intptr(0), intptr(0)},
	}
	for name, times := range cases {
		t.Run(name, func(t *testing.T) {
			index := timeChainBlocks(t, [][]string{{"a", "b"}, {"a"}, {"b", "a"}}, times)
			page, err := index.QueryTxs(TxQuery{Reverse: true, PageSize: MaxPageSize})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(page.Hits, want.Hits) {
				t.Fatalf("%s: reverse hits=%v, want timestamp-free %v", name, page.Hits, want.Hits)
			}
			if page.TotalMatches != want.TotalMatches || page.MatchedBlocks != want.MatchedBlocks {
				t.Fatalf("%s: stats=%d/%d, want %d/%d", name,
					page.TotalMatches, page.MatchedBlocks, want.TotalMatches, want.MatchedBlocks)
			}
		})
	}
}

// timeChainBlocks builds a chain with explicit tx lists and per-height
// timestamps (nil entry means the block carries no timestamp).
func timeChainBlocks(t *testing.T, txs [][]string, times []*int64) *Index {
	t.Helper()
	index := New()
	for i := range txs {
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
			Time:   times[i],
		}
		if err := index.Append(block); err != nil {
			t.Fatalf("setup append at %d: %v", height, err)
		}
	}
	return index
}

// TestQueryTxsReverseSameCursorSamePage guarantees a repeated reverse cursor
// returns an identical page.
func TestQueryTxsReverseSameCursorSamePage(t *testing.T) {
	index := txChain(t, []string{"a"}, []string{"b"}, []string{"c"})
	first, err := index.QueryTxs(TxQuery{PageSize: 1, Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	pageA, err := index.QueryTxs(TxQuery{PageSize: 1, Reverse: true, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	pageB, err := index.QueryTxs(TxQuery{PageSize: 1, Reverse: true, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(pageA, pageB) {
		t.Fatalf("repeated reverse cursor returned different pages:\n%+v\n%+v", pageA, pageB)
	}
}

// TestQueryTxsReverseReturnedHitsAreCopies ensures caller mutation of a
// reverse page cannot leak back into the index.
func TestQueryTxsReverseReturnedHitsAreCopies(t *testing.T) {
	index := txChain(t, []string{"a", "b"})
	page, err := index.QueryTxs(TxQuery{Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	page.Hits[0].TxID = "mutated"
	page.Hits[0].BlockHash = "mutated"
	page.Hits[0].Height = 99
	again, err := index.QueryTxs(TxQuery{Reverse: true})
	if err != nil {
		t.Fatal(err)
	}
	want := TxHit{Height: 1, BlockHash: "h1", TxID: "b", Position: 1}
	if again.Hits[0] != want {
		t.Fatalf("caller mutation leaked into the index: %+v", again.Hits[0])
	}
}

// TestScanPageLockedReverseWindows checks the scan directly: direction-free
// totals, descending windows at top-anchored offsets, and the pageSize
// memory cap.
func TestScanPageLockedReverseWindows(t *testing.T) {
	var blocks [][]string
	for h := 0; h < 20; h++ {
		blocks = append(blocks, []string{
			fmt.Sprintf("t%d", h%4),
			"keep",
			"keep",
		})
	}
	index := txChain(t, blocks...) // 60 occurrences
	const from, to = int64(1), int64(20)

	allAsc, matchedBlocks := referenceScan(index, from, to, nil)
	if len(allAsc) != 60 || matchedBlocks != 20 {
		t.Fatalf("oracle setup: hits=%d blocks=%d, want 60/20", len(allAsc), matchedBlocks)
	}
	allRev := reversedHits(allAsc)

	for _, pageSize := range []int{1, 2, 7, MaxPageSize} {
		var windowed []TxHit
		for offset := int64(0); ; {
			hits, total, gotBlocks := index.scanPageLocked(from, to, newTxFilter(nil), offset, pageSize, true)
			if total != 60 || gotBlocks != 20 {
				t.Fatalf("pageSize=%d offset=%d: totals=%d/%d, want 60/20", pageSize, offset, total, gotBlocks)
			}
			if len(hits) > pageSize || cap(hits) > pageSize {
				t.Fatalf("pageSize=%d offset=%d retained len=%d cap=%d", pageSize, offset, len(hits), cap(hits))
			}
			end := offset + int64(len(hits))
			if end > int64(len(allRev)) || !reflect.DeepEqual(hits, allRev[offset:end]) {
				t.Fatalf("pageSize=%d offset=%d window mismatch", pageSize, offset)
			}
			windowed = append(windowed, hits...)
			if len(hits) < pageSize {
				break
			}
			offset = end
		}
		if !reflect.DeepEqual(windowed, allRev) {
			t.Fatalf("pageSize=%d reverse windows lost or duplicated occurrences", pageSize)
		}
	}

	// A selective filter keeps the same windowing contract.
	filteredAsc, filteredBlocks := referenceScan(index, from, to, []string{"keep", "t1"})
	filteredRev := reversedHits(filteredAsc)
	hits, total, gotBlocks := index.scanPageLocked(from, to, newTxFilter([]string{"t1", "keep"}), 2, 4, true)
	if total != int64(len(filteredAsc)) || gotBlocks != filteredBlocks {
		t.Fatalf("filtered totals=%d/%d want %d/%d", total, gotBlocks, len(filteredAsc), filteredBlocks)
	}
	if !reflect.DeepEqual(hits, filteredRev[2:6]) {
		t.Fatalf("filtered reverse window=%v, want %v", hits, filteredRev[2:6])
	}

	// An offset beyond the matches returns no records but keeps the totals.
	hits, total, gotBlocks = index.scanPageLocked(from, to, newTxFilter(nil), 70, 4, true)
	if len(hits) != 0 || total != 60 || gotBlocks != 20 {
		t.Fatalf("beyond-end reverse scan: hits=%d total=%d blocks=%d", len(hits), total, gotBlocks)
	}

	// An empty range retains nothing and reports zero statistics.
	hits, total, gotBlocks = index.scanPageLocked(100, 200, newTxFilter(nil), 0, 10, true)
	if len(hits) != 0 || cap(hits) > 10 || total != 0 || gotBlocks != 0 {
		t.Fatalf("empty-range reverse scan: hits=%v total=%d blocks=%d", hits, total, gotBlocks)
	}
}

// TestQueryTxsReverseConcurrentWithAppends mirrors the ascending concurrency
// regression for reverse pagination: every run completes against one chain
// view per page, statistics stay pinned, and the stitched hits are strictly
// descending and cover exactly TotalMatches occurrences.
func TestQueryTxsReverseConcurrentWithAppends(t *testing.T) {
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
				pages := 0
				seen := 0
				var total, matched, to int64 = -1, -1, -1
				query := TxQuery{PageSize: 7, Reverse: true}
				var lastHeight int64 = 1 << 62
				var lastPosition = 1 << 30
				started := false
				for {
					page, err := index.QueryTxs(query)
					if err != nil {
						t.Errorf("reverse query failed during appends: %v", err)
						return
					}
					if total == -1 {
						total, matched, to = page.TotalMatches, page.MatchedBlocks, page.ToHeight
					} else if page.TotalMatches != total || page.MatchedBlocks != matched || page.ToHeight != to {
						t.Errorf("reverse stats changed mid-pagination: %+v", page)
						return
					}
					for _, hit := range page.Hits {
						if started && (hit.Height > lastHeight || (hit.Height == lastHeight && hit.Position >= lastPosition)) {
							t.Errorf("reverse hits out of order after (%d,%d): %+v", lastHeight, lastPosition, hit)
							return
						}
						if hit.Height > to {
							t.Errorf("hit above pinned bound: %+v vs %d", hit, to)
							return
						}
						lastHeight, lastPosition, started = hit.Height, hit.Position, true
					}
					seen += len(page.Hits)
					pages++
					if page.NextCursor == "" {
						break
					}
					query.Cursor = page.NextCursor
				}
				if int64(seen) != total {
					t.Errorf("reverse paginated %d hits over %d pages, total says %d", seen, pages, total)
					return
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	writer.Wait()
}
