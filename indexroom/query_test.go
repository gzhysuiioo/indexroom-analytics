package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"
)

// txChain builds an index from blocks, failing the test on setup errors.
func txChain(t *testing.T, blocks ...Block) *Index {
	t.Helper()
	index := New()
	for _, block := range blocks {
		if err := index.Append(block); err != nil {
			t.Fatalf("setup append at height %d: %v", block.Height, err)
		}
	}
	return index
}

// allPages drains a query to completion, returning every record in order.
func allPages(t *testing.T, index *Index, q TxQuery) []TxRecord {
	t.Helper()
	var out []TxRecord
	cursor := ""
	for {
		page, err := index.QueryTxs(q, cursor)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		out = append(out, page.Records...)
		if page.Cursor == "" {
			return out
		}
		cursor = page.Cursor
	}
}

func recordsEqual(a, b []TxRecord) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestQueryTxsBasicOrderingAndFields(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t2"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t3"}},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t4", "t5", "t6"}},
	)
	page, err := index.QueryTxs(TxQuery{StartHeight: 1, EndHeight: 3}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []TxRecord{
		{Height: 1, BlockHash: "h1", TxID: "t1", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "t2", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "t3", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "t4", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "t5", Position: 1},
		{Height: 3, BlockHash: "h3", TxID: "t6", Position: 2},
	}
	if !recordsEqual(page.Records, want) {
		t.Fatalf("records=%+v want %+v", page.Records, want)
	}
	if page.TotalMatches != 6 || page.MatchedBlocks != 3 {
		t.Fatalf("stats: total=%d blocks=%d, want 6/3", page.TotalMatches, page.MatchedBlocks)
	}
	if page.UpperBound != 3 {
		t.Fatalf("upper bound=%d, want 3", page.UpperBound)
	}
	if page.Cursor != "" {
		t.Fatalf("cursor=%q, want empty for a single page", page.Cursor)
	}
}

func TestQueryTxsDuplicatesPreserved(t *testing.T) {
	// The same identifier in the same block or different blocks must appear
	// at every position; it is never deduplicated.
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t1", "t2"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t1"}},
	)
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"t1"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	want := []TxRecord{
		{Height: 1, BlockHash: "h1", TxID: "t1", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "t1", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "t1", Position: 0},
	}
	if !recordsEqual(page.Records, want) {
		t.Fatalf("records=%+v want %+v", page.Records, want)
	}
	if page.TotalMatches != 3 {
		t.Fatalf("total=%d, want 3", page.TotalMatches)
	}
	if page.MatchedBlocks != 2 {
		t.Fatalf("matched blocks=%d, want 2", page.MatchedBlocks)
	}
}

func TestQueryTxsFilterExactMatchAndSetNormalization(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t2", "t3"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2", "t4"}},
	)
	// Empty set matches everything.
	page, err := index.QueryTxs(TxQuery{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalMatches != 5 {
		t.Fatalf("empty filter total=%d, want 5", page.TotalMatches)
	}
	// Exact string comparison: "t1" does not match "t10".
	index2 := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t10", "t1"}},
	)
	page, err = index2.QueryTxs(TxQuery{TxIDs: []string{"t1"}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].TxID != "t1" {
		t.Fatalf("exact match failed: %+v", page.Records)
	}
	// Duplicates and order in the filter set do not matter.
	for _, ids := range [][]string{
		{"t2", "t4"},
		{"t4", "t2"},
		{"t2", "t2", "t4", "t4"},
	} {
		p, err := index.QueryTxs(TxQuery{TxIDs: ids}, "")
		if err != nil {
			t.Fatal(err)
		}
		want := []TxRecord{
			{Height: 1, BlockHash: "h1", TxID: "t2", Position: 1},
			{Height: 2, BlockHash: "h2", TxID: "t2", Position: 0},
			{Height: 2, BlockHash: "h2", TxID: "t4", Position: 1},
		}
		if !recordsEqual(p.Records, want) {
			t.Fatalf("ids=%v records=%+v want %+v", ids, p.Records, want)
		}
	}
}

func TestQueryTxsHeightRangeDefaultsAndCap(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3"}},
	)
	// Omitted start means height 1; omitted end means the tip.
	page, err := index.QueryTxs(TxQuery{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if page.UpperBound != 3 || page.TotalMatches != 3 {
		t.Fatalf("defaults: upper=%d total=%d", page.UpperBound, page.TotalMatches)
	}
	// An end above the tip is capped to the tip.
	page, err = index.QueryTxs(TxQuery{StartHeight: 2, EndHeight: 100}, "")
	if err != nil {
		t.Fatal(err)
	}
	if page.UpperBound != 3 {
		t.Fatalf("upper bound=%d, want capped 3", page.UpperBound)
	}
	want := []TxRecord{
		{Height: 2, BlockHash: "h2", TxID: "t2", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "t3", Position: 0},
	}
	if !recordsEqual(page.Records, want) {
		t.Fatalf("records=%+v want %+v", page.Records, want)
	}
}

func TestQueryTxsEmptyAndStartAboveTip(t *testing.T) {
	// Empty index: successful empty result with bound 0.
	empty := New()
	page, err := empty.QueryTxs(TxQuery{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 || page.Cursor != "" {
		t.Fatalf("empty index result: %+v", page)
	}
	if page.UpperBound != 0 {
		t.Fatalf("empty index upper=%d, want 0", page.UpperBound)
	}
	// Start above the tip: successful empty result.
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
	)
	page, err = index.QueryTxs(TxQuery{StartHeight: 5, EndHeight: 10}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 0 || page.Cursor != "" || page.UpperBound != 1 {
		t.Fatalf("start above tip result: %+v", page)
	}
}

func TestQueryTxsInvalidConditions(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis"},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
	)
	for name, q := range map[string]TxQuery{
		"negative start":      {StartHeight: -1},
		"negative end":        {EndHeight: -1},
		"end below start":     {StartHeight: 3, EndHeight: 2},
		"zero page size":      {PageSize: 0}, // 0 is the default, not invalid
		"page size too large": {PageSize: 1001},
		"negative page size":  {PageSize: -1},
	} {
		_, err := index.QueryTxs(q, "")
		switch name {
		case "zero page size":
			if err != nil {
				t.Fatalf("%s: unexpected error: %v", name, err)
			}
		default:
			if err == nil {
				t.Fatalf("%s: expected parameter error", name)
			}
		}
	}
	// Page size 1000 is the largest allowed.
	if _, err := index.QueryTxs(TxQuery{PageSize: 1000}, ""); err != nil {
		t.Fatalf("page size 1000 rejected: %v", err)
	}
}

func TestQueryTxsPaginationNoDuplicatesNoGaps(t *testing.T) {
	blocks := make([]Block, 0, 12)
	for h := int64(1); h <= 3; h++ {
		txs := make([]string, 0, 4)
		for p := 0; p < 4; p++ {
			txs = append(txs, fmt.Sprintf("h%dp%d", h, p))
		}
		blocks = append(blocks, Block{Height: h, Hash: fmt.Sprintf("h%d", h), Parent: fmt.Sprintf("h%d", h-1), Txs: txs})
	}
	index := txChain(t, blocks...)

	for _, pageSize := range []int{1, 2, 3, 5, 100} {
		q := TxQuery{StartHeight: 1, EndHeight: 3, PageSize: pageSize}
		all := allPages(t, index, q)
		if len(all) != 12 {
			t.Fatalf("pageSize=%d: got %d records, want 12", pageSize, len(all))
		}
		// Verify strict ordering and no duplicates.
		seen := make(map[TxRecord]bool)
		for i, r := range all {
			if seen[r] {
				t.Fatalf("pageSize=%d: duplicate record %+v", pageSize, r)
			}
			seen[r] = true
			if i > 0 {
				prev := all[i-1]
				if r.Height < prev.Height || (r.Height == prev.Height && r.Position <= prev.Position) {
					t.Fatalf("pageSize=%d: not ordered at %d: %+v after %+v", pageSize, i, r, prev)
				}
			}
		}
	}
}

func TestQueryTxsCursorEmptyAtEnd(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t2", "t3"}},
	)
	// Page size larger than the result set: no cursor.
	page, err := index.QueryTxs(TxQuery{PageSize: 100}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 3 || page.Cursor != "" {
		t.Fatalf("single page: records=%d cursor=%q", len(page.Records), page.Cursor)
	}
	// Exactly pageSize records: no cursor.
	page, err = index.QueryTxs(TxQuery{PageSize: 3}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 3 || page.Cursor != "" {
		t.Fatalf("exact page: records=%d cursor=%q", len(page.Records), page.Cursor)
	}
	// One more than pageSize: cursor present.
	page, err = index.QueryTxs(TxQuery{PageSize: 2}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 2 || page.Cursor == "" {
		t.Fatalf("full page: records=%d cursor=%q", len(page.Records), page.Cursor)
	}
}

func TestQueryTxsPageSizeCanChange(t *testing.T) {
	blocks := make([]Block, 0, 10)
	for h := int64(1); h <= 10; h++ {
		blocks = append(blocks, Block{Height: h, Hash: fmt.Sprintf("h%d", h), Parent: fmt.Sprintf("h%d", h-1), Txs: []string{fmt.Sprintf("t%d", h)}})
	}
	index := txChain(t, blocks...)

	// First page with size 3.
	page1, err := index.QueryTxs(TxQuery{StartHeight: 1, EndHeight: 10, PageSize: 3}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page1.Records) != 3 || page1.Cursor == "" {
		t.Fatalf("page1: records=%d cursor=%q", len(page1.Records), page1.Cursor)
	}
	// Continue with a larger page size: the next records follow on directly.
	page2, err := index.QueryTxs(TxQuery{StartHeight: 1, EndHeight: 10, PageSize: 5}, page1.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	want := []TxRecord{
		{Height: 4, BlockHash: "h4", TxID: "t4", Position: 0},
		{Height: 5, BlockHash: "h5", TxID: "t5", Position: 0},
		{Height: 6, BlockHash: "h6", TxID: "t6", Position: 0},
		{Height: 7, BlockHash: "h7", TxID: "t7", Position: 0},
		{Height: 8, BlockHash: "h8", TxID: "t8", Position: 0},
	}
	if !recordsEqual(page2.Records, want) {
		t.Fatalf("page2 records=%+v want %+v", page2.Records, want)
	}
	// Stats are identical on both pages.
	if page2.TotalMatches != page1.TotalMatches || page2.MatchedBlocks != page1.MatchedBlocks || page2.UpperBound != page1.UpperBound {
		t.Fatalf("stats changed: page1=%+v page2=%+v", page1, page2)
	}
}

func TestQueryTxsResubmitCursorSamePage(t *testing.T) {
	blocks := make([]Block, 0, 6)
	for h := int64(1); h <= 6; h++ {
		blocks = append(blocks, Block{Height: h, Hash: fmt.Sprintf("h%d", h), Parent: fmt.Sprintf("h%d", h-1), Txs: []string{fmt.Sprintf("t%d", h)}})
	}
	index := txChain(t, blocks...)
	q := TxQuery{StartHeight: 1, EndHeight: 6, PageSize: 2}
	page1, err := index.QueryTxs(q, "")
	if err != nil {
		t.Fatal(err)
	}
	// The page after page1 is reached by re-submitting page1's cursor.
	page2, err := index.QueryTxs(q, page1.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	// Re-submitting the same cursor with the same page size returns the same
	// page (the page after the cursor), not a different one.
	page2Again, err := index.QueryTxs(q, page1.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	if !recordsEqual(page2.Records, page2Again.Records) {
		t.Fatalf("page changed on resubmit: %+v vs %+v", page2.Records, page2Again.Records)
	}
	if page2.Cursor != page2Again.Cursor {
		t.Fatalf("next cursor changed on resubmit: %q vs %q", page2.Cursor, page2Again.Cursor)
	}
	// The two pages must be distinct and cover the range without overlap.
	if recordsEqual(page1.Records, page2.Records) {
		t.Fatalf("page1 and page2 should differ: %+v", page1.Records)
	}
}

func TestQueryTxsStatsCoverFullRange(t *testing.T) {
	// Range [2,4] with a filter: stats count all matches in the range,
	// including blocks beyond the first page.
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"x"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t1", "x", "t2"}},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"x"}},
		Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"t3", "t4"}},
		Block{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"t5"}},
	)
	q := TxQuery{StartHeight: 2, EndHeight: 4, TxIDs: []string{"t1", "t2", "t3", "t4", "t5"}, PageSize: 2}
	page1, err := index.QueryTxs(q, "")
	if err != nil {
		t.Fatal(err)
	}
	// t5 is at height 5, outside the range, so it must not be counted.
	if page1.TotalMatches != 4 || page1.MatchedBlocks != 2 {
		t.Fatalf("stats: total=%d blocks=%d, want 4/2", page1.TotalMatches, page1.MatchedBlocks)
	}
	if page1.UpperBound != 4 {
		t.Fatalf("upper=%d, want 4", page1.UpperBound)
	}
	all := allPages(t, index, q)
	if len(all) != 4 {
		t.Fatalf("drained records=%d, want 4", len(all))
	}
}

func TestQueryTxsReturnedRecordsAreIndependent(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t2"}},
	)
	page, err := index.QueryTxs(TxQuery{}, "")
	if err != nil {
		t.Fatal(err)
	}
	// Mutating a returned record must not affect the index or later queries.
	page.Records[0].TxID = "mutated"
	page.Records[0].Height = 999
	page2, err := index.QueryTxs(TxQuery{}, "")
	if err != nil {
		t.Fatal(err)
	}
	if page2.Records[0].TxID != "t1" || page2.Records[0].Height != 1 {
		t.Fatalf("returned record mutation leaked: %+v", page2.Records[0])
	}
	// The stored block is untouched.
	if index.Blocks[1].Txs[0] != "t1" {
		t.Fatalf("index tx mutated: %v", index.Blocks[1].Txs)
	}
}

func TestQueryTxsContinuationConditionsMustMatch(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3"}},
	)
	q := TxQuery{StartHeight: 1, EndHeight: 3, TxIDs: []string{"t1", "t2", "t3"}, PageSize: 1}
	page1, err := index.QueryTxs(q, "")
	if err != nil {
		t.Fatal(err)
	}
	cursor := page1.Cursor

	for name, q2 := range map[string]TxQuery{
		"different start":      {StartHeight: 2, EndHeight: 3, TxIDs: []string{"t1", "t2", "t3"}, PageSize: 1},
		"different end":        {StartHeight: 1, EndHeight: 2, TxIDs: []string{"t1", "t2", "t3"}, PageSize: 1},
		"different filter":     {StartHeight: 1, EndHeight: 3, TxIDs: []string{"t1", "t2"}, PageSize: 1},
		"extra filter id":      {StartHeight: 1, EndHeight: 3, TxIDs: []string{"t1", "t2", "t3", "t4"}, PageSize: 1},
		"missing filter id":    {StartHeight: 1, EndHeight: 3, TxIDs: []string{"t1", "t2"}, PageSize: 1},
		"no filter vs filter":  {StartHeight: 1, EndHeight: 3, TxIDs: nil, PageSize: 1},
	} {
		if _, err := index.QueryTxs(q2, cursor); err == nil {
			t.Fatalf("%s: expected parameter error", name)
		}
	}
	// Reordering or deduping the filter set is still the same condition.
	for _, ids := range [][]string{
		{"t3", "t2", "t1"},
		{"t1", "t1", "t2", "t3", "t3"},
	} {
		q2 := TxQuery{StartHeight: 1, EndHeight: 3, TxIDs: ids, PageSize: 1}
		if _, err := index.QueryTxs(q2, cursor); err != nil {
			t.Fatalf("ids=%v: unexpected error: %v", ids, err)
		}
	}
}

func TestQueryTxsCursorCorruptAndForeign(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t2"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t3"}},
	)
	page1, err := index.QueryTxs(TxQuery{PageSize: 1}, "")
	if err != nil {
		t.Fatal(err)
	}
	cursor := page1.Cursor

	// Corrupt cursor: flip characters.
	corrupt := cursor[:len(cursor)-2] + "xx"
	if _, err := index.QueryTxs(TxQuery{PageSize: 1}, corrupt); err == nil {
		t.Fatal("corrupt cursor accepted")
	}
	// Truncated cursor.
	if _, err := index.QueryTxs(TxQuery{PageSize: 1}, cursor[:10]); err == nil {
		t.Fatal("truncated cursor accepted")
	}
	// Garbage.
	if _, err := index.QueryTxs(TxQuery{PageSize: 1}, "not-a-cursor"); err == nil {
		t.Fatal("garbage cursor accepted")
	}

	// Cursor from another index instance: HMAC verification fails.
	other := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1", "t2"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t3"}},
	)
	if _, err := other.QueryTxs(TxQuery{PageSize: 1}, cursor); err == nil {
		t.Fatal("foreign cursor accepted")
	}
	// The foreign cursor failure must not disturb the other index.
	if other.Tip != 2 {
		t.Fatalf("other index tip disturbed: %d", other.Tip)
	}
}

func TestQueryTxsDataChangedOnReorgInRange(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3"}},
		Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"t4"}},
	)
	q := TxQuery{StartHeight: 1, EndHeight: 3, PageSize: 1}
	page1, err := index.QueryTxs(q, "")
	if err != nil {
		t.Fatal(err)
	}
	cursor := page1.Cursor

	// Reorg inside the fixed range: replace block 2 with a different hash.
	if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"t2"}}}); err != nil {
		t.Fatal(err)
	}
	_, err = index.QueryTxs(q, cursor)
	if !errors.Is(err, ErrTxQueryDataChanged) {
		t.Fatalf("expected ErrTxQueryDataChanged, got %v", err)
	}
	// No results are returned with the error.
	page, err := index.QueryTxs(q, cursor)
	if page != nil {
		t.Fatalf("expected nil page on data changed, got %+v", page)
	}
}

func TestQueryTxsDataChangedWhenTxsChangeWithSameHash(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3"}},
	)
	q := TxQuery{StartHeight: 1, EndHeight: 3, PageSize: 1}
	page1, err := index.QueryTxs(q, "")
	if err != nil {
		t.Fatal(err)
	}
	cursor := page1.Cursor

	// Reorg that keeps the same block hash but changes the transactions.
	// The hash is in the dropped suffix, so reusing it is allowed.
	if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2x"}}}); err != nil {
		t.Fatal(err)
	}
	_, err = index.QueryTxs(q, cursor)
	if !errors.Is(err, ErrTxQueryDataChanged) {
		t.Fatalf("expected ErrTxQueryDataChanged for tx change with same hash, got %v", err)
	}
}

func TestQueryTxsDataChangedOnChainShortening(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3"}},
	)
	q := TxQuery{StartHeight: 1, EndHeight: 3, PageSize: 1}
	page1, err := index.QueryTxs(q, "")
	if err != nil {
		t.Fatal(err)
	}
	cursor := page1.Cursor

	// Shorten the chain: block 3 disappears from the fixed range.
	if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1"}}); err != nil {
		t.Fatal(err)
	}
	_, err = index.QueryTxs(q, cursor)
	if !errors.Is(err, ErrTxQueryDataChanged) {
		t.Fatalf("expected ErrTxQueryDataChanged on shortening, got %v", err)
	}
}

func TestQueryTxsContinuationSurvivesReplayAndOutsideReorg(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3"}},
		Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"t4"}},
		Block{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"t5"}},
		Block{Height: 6, Hash: "h6", Parent: "h5", Txs: []string{"t6"}},
	)
	// Range [1,4] with page size 1: four pages, cursor valid throughout.
	q := TxQuery{StartHeight: 1, EndHeight: 4, PageSize: 1}
	page1, err := index.QueryTxs(q, "")
	if err != nil {
		t.Fatal(err)
	}
	if page1.Records[0].TxID != "t1" || page1.Cursor == "" {
		t.Fatalf("page1: records=%+v cursor=%q", page1.Records, page1.Cursor)
	}

	// Duplicate append of an identical block is a no-op.
	if err := index.Append(Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}}); err != nil {
		t.Fatal(err)
	}
	page2, err := index.QueryTxs(q, page1.Cursor)
	if err != nil {
		t.Fatalf("continuation after duplicate append failed: %v", err)
	}
	if page2.Records[0].TxID != "t2" {
		t.Fatalf("page2 records=%+v, want t2", page2.Records)
	}

	// Reorg outside the fixed range (height 5): the range is untouched.
	if _, err := index.Reorg([]Block{{Height: 5, Hash: "h5b", Parent: "h4", Txs: []string{"t5"}}}); err != nil {
		t.Fatal(err)
	}
	page3, err := index.QueryTxs(q, page2.Cursor)
	if err != nil {
		t.Fatalf("continuation after outside reorg failed: %v", err)
	}
	if page3.Records[0].TxID != "t3" {
		t.Fatalf("page3 records=%+v, want t3", page3.Records)
	}

	// Re-submitting the same branch in effect is a no-op replay.
	if _, err := index.Reorg([]Block{{Height: 5, Hash: "h5b", Parent: "h4", Txs: []string{"t5"}}}); err != nil {
		t.Fatal(err)
	}
	page4, err := index.QueryTxs(q, page3.Cursor)
	if err != nil {
		t.Fatalf("continuation after replay failed: %v", err)
	}
	if page4.Records[0].TxID != "t4" {
		t.Fatalf("page4 records=%+v, want t4", page4.Records)
	}
	if page4.Cursor != "" {
		t.Fatalf("expected end of pages, cursor=%q", page4.Cursor)
	}
}

func TestQueryTxsUpperBoundFixedAcrossPages(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t2"}},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t3"}},
	)
	// First query asks for [1,10] but the tip is 3; the bound is fixed at 3.
	q := TxQuery{StartHeight: 1, EndHeight: 10, PageSize: 1}
	page1, err := index.QueryTxs(q, "")
	if err != nil {
		t.Fatal(err)
	}
	if page1.UpperBound != 3 {
		t.Fatalf("upper=%d, want 3", page1.UpperBound)
	}
	// Append a higher block after the first query.
	if err := index.Append(Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"t4"}}); err != nil {
		t.Fatal(err)
	}
	// Continuation still uses the fixed bound 3 and never sees height 4.
	// Drain the query from page1's cursor; no record may appear above the
	// fixed bound 3.
	cursor := page1.Cursor
	for cursor != "" {
		page, err := index.QueryTxs(q, cursor)
		if err != nil {
			t.Fatal(err)
		}
		if page.UpperBound != 3 {
			t.Fatalf("upper after append=%d, want fixed 3", page.UpperBound)
		}
		for _, r := range page.Records {
			if r.Height > 3 {
				t.Fatalf("record above fixed bound: %+v", r)
			}
		}
		cursor = page.Cursor
	}
}

func TestQueryTxsConcurrentWithIngestion(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
	)
	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Ingestion workers: append and reorg concurrently.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				h := fmt.Sprintf("w%d-i%d", w, i)
				if _, err := index.Reorg([]Block{{Height: 2, Hash: h, Parent: "h1", Txs: []string{h}}}); err == nil {
					_ = index.Append(Block{Height: 3, Hash: h + "-x", Parent: h, Txs: []string{h + "-x"}})
				}
			}
		}(w)
	}

	// Query workers: repeatedly page through the fixed range [1,1].
	var queryWG sync.WaitGroup
	for w := 0; w < 4; w++ {
		queryWG.Add(1)
		go func() {
			defer queryWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				q := TxQuery{StartHeight: 1, EndHeight: 1, PageSize: 10}
				page, err := index.QueryTxs(q, "")
				if err != nil {
					t.Errorf("query failed: %v", err)
					return
				}
				// Every record must be within the fixed range and ordered.
				for _, r := range page.Records {
					if r.Height != 1 {
						t.Errorf("record outside fixed range: %+v", r)
					}
				}
				// Stats must be consistent with the records.
				if page.TotalMatches < len(page.Records) {
					t.Errorf("total %d < records %d", page.TotalMatches, len(page.Records))
				}
				if page.MatchedBlocks > page.TotalMatches {
					t.Errorf("matched blocks %d > total %d", page.MatchedBlocks, page.TotalMatches)
				}
			}
		}()
	}

	// Exercise the race detector for a bounded window, then stop cleanly.
	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()
	queryWG.Wait()
}

func TestQueryTxsFilterWithPagination(t *testing.T) {
	// Only some transactions match; pagination must skip non-matching ones.
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"x", "t1", "x", "t2"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"x", "t3", "x"}},
		Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"t4", "x"}},
	)
	q := TxQuery{StartHeight: 1, EndHeight: 3, TxIDs: []string{"t1", "t2", "t3", "t4"}, PageSize: 2}
	page1, err := index.QueryTxs(q, "")
	if err != nil {
		t.Fatal(err)
	}
	want1 := []TxRecord{
		{Height: 1, BlockHash: "h1", TxID: "t1", Position: 1},
		{Height: 1, BlockHash: "h1", TxID: "t2", Position: 3},
	}
	if !recordsEqual(page1.Records, want1) {
		t.Fatalf("page1=%+v want %+v", page1.Records, want1)
	}
	page2, err := index.QueryTxs(q, page1.Cursor)
	if err != nil {
		t.Fatal(err)
	}
	want2 := []TxRecord{
		{Height: 2, BlockHash: "h2", TxID: "t3", Position: 1},
		{Height: 3, BlockHash: "h3", TxID: "t4", Position: 0},
	}
	if !recordsEqual(page2.Records, want2) {
		t.Fatalf("page2=%+v want %+v", page2.Records, want2)
	}
	if page2.Cursor != "" {
		t.Fatalf("expected end, cursor=%q", page2.Cursor)
	}
	if page1.TotalMatches != 4 || page1.MatchedBlocks != 3 {
		t.Fatalf("stats: total=%d blocks=%d, want 4/3", page1.TotalMatches, page1.MatchedBlocks)
	}
}

func TestQueryTxsEmptyStringIdentifier(t *testing.T) {
	// An empty-string identifier is matched exactly like any other.
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"", "t1"}},
	)
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{""}}, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Records) != 1 || page.Records[0].TxID != "" || page.Records[0].Position != 0 {
		t.Fatalf("empty id match: %+v", page.Records)
	}
}

func TestQueryTxsNoFilterMatchesAll(t *testing.T) {
	index := txChain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "b"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"c"}},
	)
	all := allPages(t, index, TxQuery{})
	if len(all) != 3 {
		t.Fatalf("got %d records, want 3", len(all))
	}
	// Sorted order.
	sorted := make([]TxRecord, len(all))
	copy(sorted, all)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Height != sorted[j].Height {
			return sorted[i].Height < sorted[j].Height
		}
		return sorted[i].Position < sorted[j].Position
	})
	if !reflect.DeepEqual(all, sorted) {
		t.Fatalf("records not sorted: %+v", all)
	}
}
