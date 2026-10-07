package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// descHits walks a descending query to the end and collects every hit.
func collectDescPages(t *testing.T, index *Index, query TxQuery) []TxPage {
	t.Helper()
	query.Order = OrderDesc
	return collectPages(t, index, query)
}

// TestQueryTxsDescSpecExample pins the contract from the product brief:
// heights 1..3 hold [a,b,a], [a,c], [b,a]; filtering "a" with two hits per
// page must page (3,1),(2,0) then (1,2),(1,0), and both pages report the
// whole fixed range: four occurrences across three blocks.
func TestQueryTxsDescSpecExample(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b", "a"},
		[]string{"a", "c"},
		[]string{"b", "a"},
	)
	query := TxQuery{TxIDs: []string{"a"}, PageSize: 2, Order: OrderDesc}

	page1, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	want1 := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(page1.Hits, want1) {
		t.Fatalf("page1=%v, want %v", page1.Hits, want1)
	}
	if page1.TotalMatches != 4 || page1.MatchedBlocks != 3 || page1.ToHeight != 3 {
		t.Fatalf("page1 stats=%+v, want total=4 blocks=3 to=3", page1)
	}
	if page1.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}

	query.Cursor = page1.NextCursor
	page2, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	want2 := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 2},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(page2.Hits, want2) {
		t.Fatalf("page2=%v, want %v", page2.Hits, want2)
	}
	if page2.TotalMatches != 4 || page2.MatchedBlocks != 3 || page2.NextCursor != "" {
		t.Fatalf("page2 stats=%+v, want total=4 blocks=3 no cursor", page2)
	}
}

// TestQueryTxsDescIsReverseOfAscending checks every page size: the
// concatenated descending occurrences are exactly the ascending ones
// reversed, statistics and the pinned bound are direction-independent, and
// positions are the original zero-based block positions.
func TestQueryTxsDescIsReverseOfAscending(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b", "c"},
		[]string{"a", "a"},
		[]string{},
		[]string{"b", "c", "a", "a"},
		[]string{"c", "a"},
	)
	asc, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	desc, err := index.QueryTxs(TxQuery{Order: OrderDesc})
	if err != nil {
		t.Fatal(err)
	}
	if desc.TotalMatches != asc.TotalMatches || desc.MatchedBlocks != asc.MatchedBlocks ||
		desc.ToHeight != asc.ToHeight {
		t.Fatalf("desc stats %+v differ from asc %+v", desc, asc)
	}
	reversed := make([]TxHit, len(asc.Hits))
	for i, hit := range asc.Hits {
		reversed[len(asc.Hits)-1-i] = hit
	}
	if !reflect.DeepEqual(desc.Hits, reversed) {
		t.Fatalf("desc hits are not ascending hits reversed:\n%v\n%v", desc.Hits, reversed)
	}

	for _, pageSize := range []int{1, 2, 3, 7, MaxPageSize} {
		pages := collectDescPages(t, index, TxQuery{PageSize: pageSize})
		var all []TxHit
		for i, page := range pages {
			if page.TotalMatches != asc.TotalMatches || page.MatchedBlocks != asc.MatchedBlocks {
				t.Fatalf("pageSize=%d page %d stats drifted: %+v", pageSize, i, page)
			}
			// Every page is itself strictly ordered: height then position down.
			for j := 1; j < len(page.Hits); j++ {
				prev, cur := page.Hits[j-1], page.Hits[j]
				if cur.Height > prev.Height ||
					(cur.Height == prev.Height && cur.Position >= prev.Position) {
					t.Fatalf("pageSize=%d page %d not descending at %d: %+v after %+v",
						pageSize, i, j, cur, prev)
				}
			}
			all = append(all, page.Hits...)
		}
		if !reflect.DeepEqual(all, reversed) {
			t.Fatalf("pageSize=%d: paginated desc walk lost or duplicated an occurrence", pageSize)
		}
	}
}

// TestQueryTxsDescPositionsAreNotRenumbered verifies that descending output
// keeps each occurrence's real zero-based position inside its block even
// though positions arrive from large to small.
func TestQueryTxsDescPositionsAreNotRenumbered(t *testing.T) {
	index := txChain(t,
		[]string{"x", "a", "y", "a", "z"},
	)
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, Order: OrderDesc})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 3},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits=%v, want original positions 3 then 1", page.Hits)
	}
}

// TestQueryTxsDescRepeatedOccurrencesAreNotMerged checks the same identifier
// appearing twice in one block and across blocks stays separate in desc order.
func TestQueryTxsDescRepeatedOccurrencesAreNotMerged(t *testing.T) {
	index := txChain(t,
		[]string{"a", "a"},
		[]string{"b"},
		[]string{"a", "a", "a"},
	)
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, Order: OrderDesc})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 2},
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 1},
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits=%v, want %v", page.Hits, want)
	}
	if page.TotalMatches != 5 || page.MatchedBlocks != 2 {
		t.Fatalf("stats=%+v, want 5 matches in 2 blocks", page)
	}
}

// TestQueryTxsDescEmptyRangesSucceed: empty chain, a start above the tip and
// a filter without hits all return a successful empty page with no cursor.
func TestQueryTxsDescEmptyRangesSucceed(t *testing.T) {
	empty := New()
	page, err := empty.QueryTxs(TxQuery{Order: OrderDesc})
	if err != nil {
		t.Fatalf("empty index desc query failed: %v", err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 ||
		page.ToHeight != 0 || page.NextCursor != "" {
		t.Fatalf("unexpected empty-index desc page: %+v", page)
	}

	index := txChain(t, []string{"a"})
	page, err = index.QueryTxs(TxQuery{From: 5, Order: OrderDesc})
	if err != nil {
		t.Fatalf("start above tip failed: %v", err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.ToHeight != 1 || page.NextCursor != "" {
		t.Fatalf("unexpected out-of-range desc page: %+v", page)
	}

	page, err = index.QueryTxs(TxQuery{TxIDs: []string{"zzz"}, Order: OrderDesc})
	if err != nil {
		t.Fatalf("no-match filter failed: %v", err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 || page.NextCursor != "" {
		t.Fatalf("unexpected no-match desc page: %+v", page)
	}
}

// TestQueryTxsDescMayResizePageBetweenPages changes the page size on every
// continuation; the walk must still resume exactly where the previous page
// ended, neither repeating nor skipping an occurrence.
func TestQueryTxsDescMayResizePageBetweenPages(t *testing.T) {
	var blocks [][]string
	for h := 0; h < 10; h++ {
		blocks = append(blocks, []string{"a", "b", "a"})
	}
	index := txChain(t, blocks...)

	first, err := index.QueryTxs(TxQuery{PageSize: 1, Order: OrderDesc})
	if err != nil {
		t.Fatal(err)
	}
	rest, err := index.QueryTxs(TxQuery{PageSize: MaxPageSize, Order: OrderDesc, Cursor: first.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Hits) != 29 || rest.NextCursor != "" || rest.TotalMatches != 30 {
		t.Fatalf("unexpected resized desc page: %+v", rest)
	}

	sizes := []int{1, 4, 2, 7, 3, MaxPageSize}
	query := TxQuery{PageSize: 1, Order: OrderDesc}
	var all []TxHit
	for {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Hits...)
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
		query.PageSize = sizes[len(all)%len(sizes)]
	}
	full, err := index.QueryTxs(TxQuery{Order: OrderDesc, PageSize: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(all, full.Hits) {
		t.Fatalf("variably-sized desc walk differs from the full answer")
	}
}

// TestQueryTxsContinuationRejectsMismatchedOrder: an ascending cursor may
// not drive a descending continuation or vice versa; both are argument
// errors returning no usable page, and the genuine cursor still works.
func TestQueryTxsContinuationRejectsMismatchedOrder(t *testing.T) {
	index := txChain(t,
		[]string{"a"},
		[]string{"a"},
		[]string{"a"},
		[]string{"a"},
	)
	asc, err := index.QueryTxs(TxQuery{PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	desc, err := index.QueryTxs(TxQuery{PageSize: 2, Order: OrderDesc})
	if err != nil {
		t.Fatal(err)
	}

	mismatched := []TxQuery{
		{PageSize: 2, Order: OrderDesc, Cursor: asc.NextCursor},
		{PageSize: 2, Order: OrderAsc, Cursor: desc.NextCursor},
		// The zero value explicitly chosen is still a mismatch for a desc cursor.
		{PageSize: 2, Cursor: desc.NextCursor},
	}
	for i, query := range mismatched {
		page, err := index.QueryTxs(query)
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("case %d: err=%v, want ErrInvalidArgument", i, err)
		}
		if len(page.Hits) != 0 || page.NextCursor != "" {
			t.Errorf("case %d: mismatched-order call returned a usable page: %+v", i, page)
		}
	}

	// Direction-consistent continuations still work, as does omitting Order
	// on an ascending cursor (OrderAsc is the zero value).
	ascNext, err := index.QueryTxs(TxQuery{PageSize: 2, Cursor: asc.NextCursor})
	if err != nil {
		t.Fatalf("ascending continuation refused: %v", err)
	}
	if len(ascNext.Hits) != 2 {
		t.Fatalf("unexpected asc page: %+v", ascNext)
	}
	descNext, err := index.QueryTxs(TxQuery{PageSize: 2, Order: OrderDesc, Cursor: desc.NextCursor})
	if err != nil {
		t.Fatalf("descending continuation refused: %v", err)
	}
	if len(descNext.Hits) != 2 {
		t.Fatalf("unexpected desc page: %+v", descNext)
	}
}

// TestQueryTxsDescContinuationPinsTheRange: blocks appended after the first
// descending page never enter the pagination; an empty-cursor query is the
// only way to read them.
func TestQueryTxsDescContinuationPinsTheRange(t *testing.T) {
	index := txChain(t,
		[]string{"a"},
		[]string{"a"},
		[]string{"a"},
	)
	first, err := index.QueryTxs(TxQuery{PageSize: 2, Order: OrderDesc})
	if err != nil {
		t.Fatal(err)
	}
	if first.ToHeight != 3 {
		t.Fatalf("pinned to=%d, want 3", first.ToHeight)
	}
	want1 := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(first.Hits, want1) {
		t.Fatalf("first desc page=%v, want %v", first.Hits, want1)
	}

	for _, height := range []int64{4, 5} {
		block := Block{Height: height, Hash: fmt.Sprintf("h%d", height), Parent: fmt.Sprintf("h%d", height-1), Txs: []string{"a"}}
		if err := index.Append(block); err != nil {
			t.Fatal(err)
		}
	}

	pages := collectDescPages(t, index, TxQuery{PageSize: 2, Cursor: first.NextCursor})
	var all []TxHit
	for _, page := range pages {
		if page.TotalMatches != 3 || page.MatchedBlocks != 3 || page.ToHeight != 3 {
			t.Fatalf("desc continuation saw appended blocks: %+v", page)
		}
		all = append(all, page.Hits...)
	}
	want := []TxHit{{Height: 1, BlockHash: "h1", TxID: "a", Position: 0}}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("remaining desc hits=%v, want %v", all, want)
	}

	// A fresh first-page descending query now starts at the new tip.
	fresh, err := index.QueryTxs(TxQuery{PageSize: 2, Order: OrderDesc})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ToHeight != 5 || fresh.TotalMatches != 5 || fresh.Hits[0].Height != 5 {
		t.Fatalf("fresh desc query did not pick up appended blocks: %+v", fresh)
	}
}

// TestQueryTxsDescCursorDiesWithRange keeps the existing change-detection
// semantics in the descending direction.
func TestQueryTxsDescCursorDiesWithRange(t *testing.T) {
	index := txChain(t,
		[]string{"a"},
		[]string{"a"},
		[]string{"a"},
	)
	first, err := index.QueryTxs(TxQuery{PageSize: 1, Order: OrderDesc})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}

	if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"a"}}}); err != nil {
		t.Fatal(err)
	}
	page, err := index.QueryTxs(TxQuery{PageSize: 1, Order: OrderDesc, Cursor: first.NextCursor})
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("err=%v, want ErrQueryChanged", err)
	}
	if len(page.Hits) != 0 || page.NextCursor != "" {
		t.Fatalf("changed-range desc call returned a usable page: %+v", page)
	}
}

// TestQueryTxsDescEquivalentFilterAndRange applies the same
// equivalence rules as ascending: reordered/duplicate ids and verbatim
// request bounds describe the same continuation.
func TestQueryTxsDescEquivalentFilterAndRange(t *testing.T) {
	index := txChain(t,
		[]string{"a", "b"},
		[]string{"a", "b"},
	)
	first, err := index.QueryTxs(TxQuery{From: 1, To: 10, TxIDs: []string{"a", "b", "b"}, PageSize: 2, Order: OrderDesc})
	if err != nil {
		t.Fatal(err)
	}
	// Same set, reordered and deduplicated; To still the verbatim request.
	second, err := index.QueryTxs(TxQuery{From: 1, To: 10, TxIDs: []string{"b", "a"}, PageSize: 2, Order: OrderDesc, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("equivalent desc continuation refused: %v", err)
	}
	if len(second.Hits) != 2 || second.NextCursor != "" || second.ToHeight != 2 {
		t.Fatalf("unexpected desc continuation: %+v", second)
	}

	// Changed range remains an argument error in the descending direction.
	if _, err := index.QueryTxs(TxQuery{From: 2, To: 10, TxIDs: []string{"a", "b"}, PageSize: 2, Order: OrderDesc, Cursor: first.NextCursor}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("changed range on desc continuation: err=%v, want ErrInvalidArgument", err)
	}
}

// TestQueryTxsDescIgnoresTimestamps: ordering depends only on height and
// position, whether block times are missing, equal, or decrease with height.
func TestQueryTxsDescIgnoresTimestamps(t *testing.T) {
	index := New()
	times := []int64{100, 100, 50} // equal then decreasing with height
	blocks := []Block{
		{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a"}, Time: nil},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a", "a"}, Time: &times[0]},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a"}, Time: &times[1]},
	}
	for i, block := range blocks {
		if i > 0 {
			block.Time = &times[i]
		}
		if err := index.Append(block); err != nil {
			t.Fatal(err)
		}
	}
	page, err := index.QueryTxs(TxQuery{Order: OrderDesc})
	if err != nil {
		t.Fatal(err)
	}
	want := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("desc order drifted with timestamps: %v, want %v", page.Hits, want)
	}
}

// TestQueryTxsRejectsInvalidOrder: unknown order values are argument errors.
func TestQueryTxsRejectsInvalidOrder(t *testing.T) {
	index := txChain(t, []string{"a"})
	if _, err := index.QueryTxs(TxQuery{Order: TxOrder(2)}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("invalid order: err=%v, want ErrInvalidArgument", err)
	}
	if _, err := index.QueryTxs(TxQuery{Order: TxOrder(-1)}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("negative order: err=%v, want ErrInvalidArgument", err)
	}
}

// TestScanPageLockedDescWindow is the descending twin of the ascending
// window contract: each offset slices the oracle's fully-reversed occurrence
// list, at most pageSize records retained, totals unchanged.
func TestScanPageLockedDescWindow(t *testing.T) {
	var blocks [][]string
	for h := 0; h < 40; h++ {
		blocks = append(blocks, []string{
			fmt.Sprintf("t%d", h%5),
			"keep",
			"keep",
		})
	}
	index := txChain(t, blocks...)
	const from, to = int64(1), int64(40)

	ascAll, matchedBlocks := referenceScan(index, from, to, nil)
	wantAll := make([]TxHit, len(ascAll))
	for i, hit := range ascAll {
		wantAll[len(ascAll)-1-i] = hit
	}

	for _, pageSize := range []int{1, 3, DefaultPageSize, MaxPageSize} {
		var windowed []TxHit
		for offset := int64(0); ; {
			hits, total, gotBlocks := index.scanPageLocked(from, to, newTxFilter(nil), timeWindow{}, OrderDesc, offset, pageSize, 0)
			if total != int64(len(wantAll)) || gotBlocks != matchedBlocks {
				t.Fatalf("pageSize=%d offset=%d: totals=%d/%d want %d/%d",
					pageSize, offset, total, gotBlocks, len(wantAll), matchedBlocks)
			}
			if cap(hits) > pageSize {
				t.Fatalf("pageSize=%d retained cap=%d", pageSize, cap(hits))
			}
			end := offset + int64(len(hits))
			if end > int64(len(wantAll)) || !reflect.DeepEqual(hits, wantAll[offset:end]) {
				t.Fatalf("pageSize=%d offset=%d: desc window mismatch", pageSize, offset)
			}
			windowed = append(windowed, hits...)
			if len(hits) < pageSize {
				break
			}
			offset = end
		}
		if !reflect.DeepEqual(windowed, wantAll) {
			t.Fatalf("pageSize=%d: desc windows do not reconstruct the reversed list", pageSize)
		}
	}
}

// TestQueryTxsDescConcurrentWithAppends mirrors the ascending concurrency
// test: descending readers always see one consistent pinned range and a
// strictly decreasing occurrence sequence.
func TestQueryTxsDescConcurrentWithAppends(t *testing.T) {
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
				query := TxQuery{PageSize: 7, Order: OrderDesc}
				var lastHeight int64 = 1 << 62
				lastPosition := 1 << 30
				for {
					page, err := index.QueryTxs(query)
					if err != nil {
						t.Errorf("desc query failed during appends: %v", err)
						return
					}
					if total == -1 {
						total, matched, to = page.TotalMatches, page.MatchedBlocks, page.ToHeight
					} else if page.TotalMatches != total || page.MatchedBlocks != matched || page.ToHeight != to {
						t.Errorf("desc stats changed mid-pagination: %+v", page)
						return
					}
					for _, hit := range page.Hits {
						if hit.Height > lastHeight || (hit.Height == lastHeight && hit.Position >= lastPosition) {
							t.Errorf("desc hits out of order after (%d,%d): %+v", lastHeight, lastPosition, hit)
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
					t.Errorf("desc paginated %d hits over %d pages, total says %d", seen, pages, total)
					return
				}
			}
		}()
	}
	readers.Wait()
	close(stop)
	writer.Wait()
}
