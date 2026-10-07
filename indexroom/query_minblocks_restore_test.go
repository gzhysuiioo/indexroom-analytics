package indexroom

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// This file is the automated regression guard for the MinBlocks threshold of a
// paginated QueryTxs across a snapshot Restore. Restore replaces the whole main
// chain with one complete snapshot, so when a caller later issues a FIRST page
// (empty cursor) the distinct-block qualification must be decided entirely
// from the restored blocks: an identifier that reached the threshold on the old
// chain only through heights the snapshot deleted must disappear completely,
// while an identifier that reaches it on the restored chain must return every
// one of its matching occurrences inside the query range, at its original
// in-block positions.
//
// The main fixture restores a SHORTER chain whose blocks hold different
// content under different hashes:
//
//	Old main chain, tip 5 (ingested):
//	  h1 parent g txs [old, q, old]  t=0    // "old" twice: one distinct block, both occurrences kept once qualified
//	  h2 parent h1 txs [old]         t=50
//	  h3 parent h2 txs [win, new]    t=nil  // missing time
//	  h4 parent h3 txs [old, new, drop] t=100 // t == window end (excluded); deleted by the restore
//	  h5 parent h4 txs [drop, win]   t=90   // deleted by the shorter restore
//	Version-2 snapshot replacing it, tip 3:
//	  j1 parent g  txs [new, z, new] t=nil  // missing time
//	  j2 parent j1 txs [win, new]    t=0    // real zero at the included window start
//	  j3 parent j2 txs [win, drop]   t=90   // "drop" survives here but in one block only
//
// Queries use the filter {old, new, win, drop} and MinBlocks = 2.
//
// Without a time window, over the whole range:
//
//	old chain: old{h1,h2,h4}=3, new{h3,h4}=2, win{h3,h5}=2, drop{h4,h5}=2;
//	  kept = 4+2+2+2 = 10 occurrences over 5 matched blocks, ToHeight 5.
//	new chain: new{j1,j2}=2, win{j2,j3}=2, drop{j3}=1 (old is absent);
//	  kept = 3+2 = 5 occurrences over 3 matched blocks, ToHeight 3.
//
// "drop" is the identifier the title singles out: it qualifies on the old
// chain only because the deleted heights h4/h5 give it two blocks, it still
// exists on the restored chain (j3), yet every "drop" occurrence must vanish
// from the new result because one block is below the threshold. "old" is an
// old-chain-only identifier. The duplicate inside h1 and inside j1 contributes
// one distinct block but two kept occurrences. The two answers share no
// records (every hash differs), totals differ (10 vs 5), and tips differ
// (5 vs 3).
//
// With the window [0,100) (start included, end excluded) and the same filter:
//
//	old chain: h3 (missing time) and h4 (t == excluded end) cannot help anyone;
//	  old{h1,h2}=2 QUALIFIED, win{h5}=1, new=0, drop{h5}=1 -> "old" only,
//	  3 occurrences (h1#0,h1#2,h2#0) over 2 matched blocks, ToHeight 5.
//	new chain: j1 has no time and never counts; j2's real zero sits exactly at
//	  the included start, so new{j2}=1, win{j2,j3}=2 QUALIFIED, drop{j3}=1;
//	  "win" only, 2 occurrences (j2#0,j3#0) over 2 matched blocks, ToHeight 3.
//
// The pinned upper bound follows the restored tip 3 even though the threshold
// filters identifiers out: shrinking the result set must never shrink
// ToHeight.

const mbRestoreMinBlocks = int64(2)

func mbRestoreOldChainBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"old", "q", "old"}, Time: intptr(0)},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"old"}, Time: intptr(50)},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"win", "new"}},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"old", "new", "drop"}, Time: intptr(100)},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"drop", "win"}, Time: intptr(90)},
	}
}

func mbRestoreNewChainBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "j1", Parent: "g", Txs: []string{"new", "z", "new"}},
		{Height: 2, Hash: "j2", Parent: "j1", Txs: []string{"win", "new"}, Time: intptr(0)},
		{Height: 3, Hash: "j3", Parent: "j2", Txs: []string{"win", "drop"}, Time: intptr(90)},
	}
}

// mbRestoreNewSnapshot is the version-2 wire form of mbRestoreNewChainBlocks:
// every block carries a timestamp, null for j1's missing time.
const mbRestoreNewSnapshot = `{"version":2,"tip":3,"blocks":[` +
	`{"height":1,"hash":"j1","parent":"g","txs":["new","z","new"],"timestamp":null},` +
	`{"height":2,"hash":"j2","parent":"j1","txs":["win","new"],"timestamp":0},` +
	`{"height":3,"hash":"j3","parent":"j2","txs":["win","drop"],"timestamp":90}` +
	`]}`

// mbRestoreBrokenSnapshot is identical to mbRestoreNewSnapshot except that the
// LAST block carries a wrong parent link, discovered only at the document's
// end; such a restore must reject wholesale as ErrInvalidSnapshot.
const mbRestoreBrokenSnapshot = `{"version":2,"tip":3,"blocks":[` +
	`{"height":1,"hash":"j1","parent":"g","txs":["new","z","new"],"timestamp":null},` +
	`{"height":2,"hash":"j2","parent":"j1","txs":["win","new"],"timestamp":0},` +
	`{"height":3,"hash":"j3","parent":"j1-broken","txs":["win","drop"],"timestamp":90}` +
	`]}`

func mbRestoreIDs() []string { return []string{"old", "new", "win", "drop"} }

func buildMbRestoreOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range mbRestoreOldChainBlocks() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at height %d: %v", b.Height, err)
		}
	}
	return index
}

// Hand-derived complete answers; the guarantee rests on these literal records
// and counts, not on a recomputation by the code under test.
func mbRestoreOldHits() []TxHit {
	return []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "old", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "old", Position: 2},
		{Height: 2, BlockHash: "h2", TxID: "old", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "win", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "new", Position: 1},
		{Height: 4, BlockHash: "h4", TxID: "old", Position: 0},
		{Height: 4, BlockHash: "h4", TxID: "new", Position: 1},
		{Height: 4, BlockHash: "h4", TxID: "drop", Position: 2},
		{Height: 5, BlockHash: "h5", TxID: "drop", Position: 0},
		{Height: 5, BlockHash: "h5", TxID: "win", Position: 1},
	}
}

func mbRestoreNewHits() []TxHit {
	return []TxHit{
		{Height: 1, BlockHash: "j1", TxID: "new", Position: 0},
		{Height: 1, BlockHash: "j1", TxID: "new", Position: 2},
		{Height: 2, BlockHash: "j2", TxID: "win", Position: 0},
		{Height: 2, BlockHash: "j2", TxID: "new", Position: 1},
		{Height: 3, BlockHash: "j3", TxID: "win", Position: 0},
	}
}

// mbRestoreWindowHits are the window-filtered answers ([0,100)).
func mbRestoreOldWindowHits() []TxHit {
	return []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "old", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "old", Position: 2},
		{Height: 2, BlockHash: "h2", TxID: "old", Position: 0},
	}
}

func mbRestoreNewWindowHits() []TxHit {
	return []TxHit{
		{Height: 2, BlockHash: "j2", TxID: "win", Position: 0},
		{Height: 3, BlockHash: "j3", TxID: "win", Position: 0},
	}
}

// mbRestoreQualifiedHits is the MinBlocks oracle over an arbitrary chain: it
// counts distinct blocks per identifier under the same filter/window, keeps
// the identifiers reaching the threshold, and returns every surviving
// occurrence in height/position order, plus the whole-range statistics.
func mbRestoreQualifiedHits(chain []Block, ids []string, window timeWindow, minBlocks int64) (hits []TxHit, total, blocks int64) {
	filter := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		filter[id] = struct{}{}
	}
	counts := make(map[string]int64)
	for _, block := range chain {
		if !window.contains(block.Time) {
			continue
		}
		seen := make(map[string]struct{})
		for _, tx := range block.Txs {
			if _, ok := filter[tx]; !ok {
				continue
			}
			if _, dup := seen[tx]; dup {
				continue
			}
			seen[tx] = struct{}{}
			counts[tx]++
		}
	}
	qualified := make(map[string]struct{}, len(counts))
	for id, n := range counts {
		if n >= minBlocks {
			qualified[id] = struct{}{}
		}
	}
	hits = []TxHit{}
	for _, block := range chain {
		if !window.contains(block.Time) {
			continue
		}
		matched := false
		for position, tx := range block.Txs {
			if _, ok := filter[tx]; !ok {
				continue
			}
			if _, ok := qualified[tx]; !ok {
				continue
			}
			hits = append(hits, TxHit{Height: block.Height, BlockHash: block.Hash, TxID: tx, Position: position})
			matched = true
		}
		if matched {
			blocks++
		}
	}
	return hits, int64(len(hits)), blocks
}

// requireMbRestorePageStats checks the whole-filtered-range statistics that
// every page of one query must carry.
func requireMbRestorePageStats(t *testing.T, page TxPage, total, blocks, toHeight int64) {
	t.Helper()
	if page.TotalMatches != total || page.MatchedBlocks != blocks || page.ToHeight != toHeight {
		t.Fatalf("page stats=%d/%d to=%d, want %d/%d to=%d",
			page.TotalMatches, page.MatchedBlocks, page.ToHeight, total, blocks, toHeight)
	}
}

// The fixture itself must say what the hand-derived answers claim, under both
// the unrestricted and the windowed query, and the two chains must be
// distinguishable record by record.
func TestQueryTxsMinBlocksRestoreFixtureSemantics(t *testing.T) {
	oldChain, newChain := mbRestoreOldChainBlocks(), mbRestoreNewChainBlocks()
	ids := mbRestoreIDs()
	noWindow := timeWindow{}
	window := timeWindow{enabled: true, start: 0, end: 100}

	gotOld, oldTotal, oldBlocks := mbRestoreQualifiedHits(oldChain, ids, noWindow, mbRestoreMinBlocks)
	if !reflect.DeepEqual(gotOld, mbRestoreOldHits()) {
		t.Fatalf("oracle old hits=%v, want %v", gotOld, mbRestoreOldHits())
	}
	if oldTotal != 10 || oldBlocks != 5 {
		t.Fatalf("oracle old stats=%d/%d, want 10/5", oldTotal, oldBlocks)
	}
	gotNew, newTotal, newBlocks := mbRestoreQualifiedHits(newChain, ids, noWindow, mbRestoreMinBlocks)
	if !reflect.DeepEqual(gotNew, mbRestoreNewHits()) {
		t.Fatalf("oracle new hits=%v, want %v", gotNew, mbRestoreNewHits())
	}
	if newTotal != 5 || newBlocks != 3 {
		t.Fatalf("oracle new stats=%d/%d, want 5/3", newTotal, newBlocks)
	}

	gotOldW, oldWTotal, oldWBlocks := mbRestoreQualifiedHits(oldChain, ids, window, mbRestoreMinBlocks)
	if !reflect.DeepEqual(gotOldW, mbRestoreOldWindowHits()) {
		t.Fatalf("oracle old window hits=%v, want %v", gotOldW, mbRestoreOldWindowHits())
	}
	if oldWTotal != 3 || oldWBlocks != 2 {
		t.Fatalf("oracle old window stats=%d/%d, want 3/2", oldWTotal, oldWBlocks)
	}
	gotNewW, newWTotal, newWBlocks := mbRestoreQualifiedHits(newChain, ids, window, mbRestoreMinBlocks)
	if !reflect.DeepEqual(gotNewW, mbRestoreNewWindowHits()) {
		t.Fatalf("oracle new window hits=%v, want %v", gotNewW, mbRestoreNewWindowHits())
	}
	if newWTotal != 2 || newWBlocks != 2 {
		t.Fatalf("oracle new window stats=%d/%d, want 2/2", newWTotal, newWBlocks)
	}

	// Discriminators: "drop" qualifies on the old chain only through deleted
	// heights, still exists on the restored chain but below the threshold;
	// "old" exists on the old chain alone; the chains share no block hash; h4's
	// time sits exactly on the excluded end while j2's real zero sits exactly
	// on the included start.
	for _, hit := range gotNew {
		if hit.TxID == "old" || hit.TxID == "drop" {
			t.Fatalf("identifier that lost qualification leaked into the new-chain oracle: %+v", hit)
		}
		if strings.HasPrefix(hit.BlockHash, "h") {
			t.Fatalf("new-chain oracle kept an old hash: %+v", hit)
		}
	}
	if !containsHit(gotOld, TxHit{Height: 5, BlockHash: "h5", TxID: "drop", Position: 0}) ||
		!containsHit(gotOld, TxHit{Height: 4, BlockHash: "h4", TxID: "drop", Position: 2}) {
		t.Fatal("\"drop\" must reach the old threshold through the deleted heights h4/h5")
	}
	if containsHit(gotNew, TxHit{Height: 3, BlockHash: "j3", TxID: "drop", Position: 1}) {
		t.Fatal("\"drop\" at j3 must not qualify on the new chain (one block only)")
	}
	if !window.contains(intptr(0)) || window.contains(nil) || window.contains(intptr(100)) {
		t.Fatal("window oracle must include zero, reject a missing time, and reject the end")
	}
}

// The core guarantee: after restoring the shorter, different chain, a fresh
// QueryTxs re-judges MinBlocks entirely from the restored blocks. Identifiers
// that qualified only through deleted heights vanish (even when the identifier
// still exists below threshold on the new chain); identifiers qualifying on
// the new chain return every matching occurrence in range, including repeats
// inside one block at their original positions; and the tip-bound ToHeight
// follows the restored tip rather than shrinking when an identifier is
// filtered out.
func TestQueryTxsMinBlocksReJudgedAfterShorterRestore(t *testing.T) {
	index := buildMbRestoreOldChain(t)

	before, err := index.QueryTxs(TxQuery{TxIDs: mbRestoreIDs(), MinBlocks: mbRestoreMinBlocks})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before.Hits, mbRestoreOldHits()) {
		t.Fatalf("pre-restore hits=%v, want the old-chain answer %v", before.Hits, mbRestoreOldHits())
	}
	requireMbRestorePageStats(t, before, 10, 5, 5)
	if before.NextCursor != "" {
		t.Fatalf("pre-restore answer fits one page, got cursor %q", before.NextCursor)
	}

	if err := index.Restore(strings.NewReader(mbRestoreNewSnapshot)); err != nil {
		t.Fatalf("valid restore refused: %v", err)
	}
	if index.Tip != 3 {
		t.Fatalf("tip after restore=%d, want 3", index.Tip)
	}
	for _, gone := range []int64{4, 5} {
		if _, ok := index.Blocks[gone]; ok {
			t.Fatalf("deleted height %d survived the restore", gone)
		}
	}
	// "drop" still exists on the restored chain at j3; the threshold is what
	// removes it, not an absence of data.
	if got := index.Blocks[3].Txs; !reflect.DeepEqual(got, []string{"win", "drop"}) {
		t.Fatalf("j3 txs=%v, want [win drop] still present on the restored chain", got)
	}

	after, err := index.QueryTxs(TxQuery{TxIDs: mbRestoreIDs(), MinBlocks: mbRestoreMinBlocks})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Hits, mbRestoreNewHits()) {
		t.Fatalf("post-restore hits=%v, want %v", after.Hits, mbRestoreNewHits())
	}
	// Whole-range statistics of the restored chain on the page, and the upper
	// bound follows the restored tip even though "old" and "drop" were removed.
	requireMbRestorePageStats(t, after, 5, 3, 3)
	if after.NextCursor != "" {
		t.Fatalf("post-restore answer fits one page, got cursor %q", after.NextCursor)
	}

	// Every surviving record belongs to the restored chain, with its hash and
	// original in-block position; no old-chain identifier or height lingers.
	for _, hit := range after.Hits {
		if hit.Height > 3 || strings.HasPrefix(hit.BlockHash, "h") {
			t.Fatalf("old-chain record leaked into the post-restore answer: %+v", hit)
		}
		if hit.TxID == "old" || hit.TxID == "drop" {
			t.Fatalf("identifier that lost qualification on the new chain survived: %+v", hit)
		}
	}
	// The j1 duplicate contributes one distinct block but, once "new"
	// qualifies, BOTH occurrences survive with un-renumbered positions.
	if !containsHit(after.Hits, TxHit{Height: 1, BlockHash: "j1", TxID: "new", Position: 0}) ||
		!containsHit(after.Hits, TxHit{Height: 1, BlockHash: "j1", TxID: "new", Position: 2}) {
		t.Fatal("qualifying identifier must keep every in-block occurrence at its original position")
	}
	requireOrdered(t, after.Hits)
}

// Reading the post-restore answer page by page (small page size) must walk the
// filtered whole range exactly once: every page reports the whole-range
// TotalMatches/MatchedBlocks of the restored chain and the restored tip, and
// the pages concatenate to the full answer with no duplicate or missing
// occurrence, in both ascending and descending order.
func TestQueryTxsMinBlocksPaginationAfterRestore(t *testing.T) {
	for _, order := range []TxOrder{OrderAsc, OrderDesc} {
		t.Run(orderName(order), func(t *testing.T) {
			index := buildMbRestoreOldChain(t)
			if err := index.Restore(strings.NewReader(mbRestoreNewSnapshot)); err != nil {
				t.Fatalf("restore refused: %v", err)
			}

			query := TxQuery{TxIDs: mbRestoreIDs(), MinBlocks: mbRestoreMinBlocks, PageSize: 2, Order: order}
			pages := collectPages(t, index, query)
			if len(pages) != 3 {
				t.Fatalf("pages=%d, want 3 over five hits at page size 2", len(pages))
			}
			var all []TxHit
			for i, page := range pages {
				requireMbRestorePageStats(t, page, 5, 3, 3)
				if i < len(pages)-1 && page.NextCursor == "" {
					t.Fatalf("page %d ended the walk early", i+1)
				}
				all = append(all, page.Hits...)
			}
			want := append([]TxHit{}, mbRestoreNewHits()...)
			if order == OrderDesc {
				for i, j := 0, len(want)-1; i < j; i, j = i+1, j-1 {
					want[i], want[j] = want[j], want[i]
				}
			}
			if !reflect.DeepEqual(all, want) {
				t.Fatalf("paginated hits=%v, want %v", all, want)
			}
			// No occurrence read twice across pages.
			seen := map[TxHit]bool{}
			for _, hit := range all {
				if seen[hit] {
					t.Fatalf("occurrence returned twice: %+v", hit)
				}
				seen[hit] = true
			}
		})
	}
}

func orderName(order TxOrder) string {
	if order == OrderDesc {
		return "desc"
	}
	return "asc"
}

// A MinBlocks query with an explicit upper bound above the restored tip clamps
// ToHeight to the restored tip: qualification and statistics must not draw on
// the old chain's removed heights. A cursor minted before the restore pinned
// the old, taller range, so after the shrink the continuation must fail with
// ErrQueryChanged rather than page mixed data; a fresh query re-pins to the
// restored tip.
func TestQueryTxsMinBlocksExplicitHighToClampsToRestoredTip(t *testing.T) {
	index := buildMbRestoreOldChain(t)
	first, err := index.QueryTxs(TxQuery{To: 99, TxIDs: mbRestoreIDs(), MinBlocks: mbRestoreMinBlocks, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	requireMbRestorePageStats(t, first, 10, 5, 5)

	if err := index.Restore(strings.NewReader(mbRestoreNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}

	stale := TxQuery{To: 99, TxIDs: mbRestoreIDs(), MinBlocks: mbRestoreMinBlocks, PageSize: 2, Cursor: first.NextCursor}
	page, err := index.QueryTxs(stale)
	requireChangedZeroed(t, page, err)

	fresh, err := index.QueryTxs(TxQuery{To: 99, TxIDs: mbRestoreIDs(), MinBlocks: mbRestoreMinBlocks})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Hits, mbRestoreNewHits()) {
		t.Fatalf("fresh hits=%v, want %v", fresh.Hits, mbRestoreNewHits())
	}
	requireMbRestorePageStats(t, fresh, 5, 3, 3)
}

// With both a transaction filter and a time window, only in-window occurrences
// participate in qualification: out-of-window and missing-time blocks cannot
// help an identifier qualify, and their occurrences are not returned. After
// the restore the qualification is re-judged from the new blocks — the real
// Unix zero at j2 is a valid timestamp at the included start, while j1's
// missing time never counts and h4's end-equal time was deleted with the old
// chain. The half-open [start, end) rule is unchanged by the restore.
func TestQueryTxsMinBlocksTimeWindowReJudgedAfterRestore(t *testing.T) {
	index := buildMbRestoreOldChain(t)
	query := TxQuery{
		TxIDs:     mbRestoreIDs(),
		TimeStart: intptr(0),
		TimeEnd:   intptr(100),
		MinBlocks: mbRestoreMinBlocks,
		PageSize:  2,
	}

	before := collectPages(t, index, query)
	var beforeHits []TxHit
	for _, page := range before {
		requireMbRestorePageStats(t, page, 3, 2, 5)
		beforeHits = append(beforeHits, page.Hits...)
	}
	if !reflect.DeepEqual(beforeHits, mbRestoreOldWindowHits()) {
		t.Fatalf("old window hits=%v, want %v", beforeHits, mbRestoreOldWindowHits())
	}

	if err := index.Restore(strings.NewReader(mbRestoreNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}

	after := collectPages(t, index, query)
	var afterHits []TxHit
	for _, page := range after {
		// New-chain whole-range statistics on every page, and the bound is the
		// restored tip 3 even though the window removed j1 entirely.
		requireMbRestorePageStats(t, page, 2, 2, 3)
		afterHits = append(afterHits, page.Hits...)
	}
	if !reflect.DeepEqual(afterHits, mbRestoreNewWindowHits()) {
		t.Fatalf("new window hits=%v, want %v", afterHits, mbRestoreNewWindowHits())
	}
	for _, hit := range afterHits {
		if hit.TxID != "win" {
			t.Fatalf("only \"win\" qualifies under the window on the new chain, got %+v", hit)
		}
		if hit.Height == 1 {
			t.Fatalf("missing-time block j1 must not help qualification nor return occurrences: %+v", hit)
		}
	}

	// The real zero is a genuine timestamp, never confused with a missing time:
	// a window starting at zero (and ending before j3's 90) keeps j2's "win" at
	// t=0 even at a threshold of one, while moving the start to one excludes
	// the zero and leaves nobody qualifying at two — a successful empty page
	// with zero statistics and the restored tip as its bound.
	zeroWin, err := index.QueryTxs(TxQuery{
		TxIDs: []string{"win"}, TimeStart: intptr(0), TimeEnd: intptr(90), MinBlocks: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(zeroWin.Hits, []TxHit{
		{Height: 2, BlockHash: "j2", TxID: "win", Position: 0},
	}) || zeroWin.TotalMatches != 1 || zeroWin.MatchedBlocks != 1 || zeroWin.ToHeight != 3 {
		t.Fatalf("window [0,90) must keep only the real-zero j2: %+v", zeroWin)
	}
	none, err := index.QueryTxs(TxQuery{
		TxIDs: mbRestoreIDs(), TimeStart: intptr(1), TimeEnd: intptr(100), MinBlocks: mbRestoreMinBlocks,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(none.Hits) != 0 || none.TotalMatches != 0 || none.MatchedBlocks != 0 || none.ToHeight != 3 || none.NextCursor != "" {
		t.Fatalf("window excluding the real zero must be a zeroed empty page, got %+v", none)
	}
}

// Distinct BLOCKS are the threshold unit after a restore: an identifier that
// appears twice inside ONE restored block and nowhere else must not reach a
// threshold of two, while an identifier spread once each across two blocks
// qualifies and keeps both occurrences at their original positions. This pins
// the rule against counting in-block repetitions as extra blocks.
func TestQueryTxsMinBlocksInBlockRepetitionCountsAsOneBlockAfterRestore(t *testing.T) {
	// tip 2, version 1: "dup" twice in j1 alone, "win" once in each block.
	const snapshot = `{"version":1,"tip":2,"blocks":[` +
		`{"height":1,"hash":"j1","parent":"g","txs":["dup","x","dup"]},` +
		`{"height":2,"hash":"j2","parent":"j1","txs":["x"]}` +
		`]}`
	index := New()
	if err := index.Restore(strings.NewReader(snapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"dup", "x"}, MinBlocks: 2})
	if err != nil {
		t.Fatal(err)
	}
	// "dup" contributes one block and vanishes; "x" spans two blocks and both
	// occurrences survive.
	want := []TxHit{
		{Height: 1, BlockHash: "j1", TxID: "x", Position: 1},
		{Height: 2, BlockHash: "j2", TxID: "x", Position: 0},
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits=%v, want %v (dup repeated in one block must not qualify)", page.Hits, want)
	}
	if page.TotalMatches != 2 || page.MatchedBlocks != 2 || page.ToHeight != 2 || page.NextCursor != "" {
		t.Fatalf("stats=%+v, want 2/2, ToHeight 2, no cursor", page)
	}
}

// A legal threshold nobody reaches after the restore is a successful empty
// page with zero statistics, an empty cursor, and the restored tip as the
// bound — not an error.
func TestQueryTxsMinBlocksNobodyQualifiesAfterRestore(t *testing.T) {
	index := buildMbRestoreOldChain(t)
	if err := index.Restore(strings.NewReader(mbRestoreNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	page, err := index.QueryTxs(TxQuery{TxIDs: mbRestoreIDs(), MinBlocks: 3})
	if err != nil {
		t.Fatalf("a threshold nobody reaches must not be an error: %v", err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 || page.ToHeight != 3 || page.NextCursor != "" {
		t.Fatalf("want a successful zeroed empty page pinned at tip 3, got %+v", page)
	}
}

// A chain obtained through a snapshot Restore and the same chain ingested
// block by block must return identical MinBlocks results and statistics,
// including under the time window and across descending pagination.
func TestQueryTxsMinBlocksRestoredChainEqualsIngestedChain(t *testing.T) {
	restored := New()
	if err := restored.Restore(strings.NewReader(mbRestoreNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	ingested := New()
	for _, b := range mbRestoreNewChainBlocks() {
		if err := ingested.Append(b); err != nil {
			t.Fatalf("ingest append at %d: %v", b.Height, err)
		}
	}

	cases := []struct {
		name  string
		query TxQuery
	}{
		{"plain", TxQuery{TxIDs: mbRestoreIDs(), MinBlocks: mbRestoreMinBlocks}},
		{"window", TxQuery{
			TxIDs: mbRestoreIDs(), TimeStart: intptr(0), TimeEnd: intptr(100), MinBlocks: mbRestoreMinBlocks,
		}},
		{"paged-desc", TxQuery{TxIDs: mbRestoreIDs(), MinBlocks: mbRestoreMinBlocks, PageSize: 2, Order: OrderDesc}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restoredPages := collectPages(t, restored, tc.query)
			ingestedPages := collectPages(t, ingested, tc.query)
			if len(restoredPages) != len(ingestedPages) {
				t.Fatalf("page counts restored=%d ingested=%d", len(restoredPages), len(ingestedPages))
			}
			var restoredHits, ingestedHits []TxHit
			for i := range restoredPages {
				r, g := restoredPages[i], ingestedPages[i]
				if r.TotalMatches != g.TotalMatches || r.MatchedBlocks != g.MatchedBlocks || r.ToHeight != g.ToHeight {
					t.Fatalf("page %d stats differ: restored=%+v ingested=%+v", i+1, r, g)
				}
				restoredHits = append(restoredHits, r.Hits...)
				ingestedHits = append(ingestedHits, g.Hits...)
			}
			if !reflect.DeepEqual(restoredHits, ingestedHits) {
				t.Fatalf("restored hits=%v, ingested hits=%v", restoredHits, ingestedHits)
			}
		})
	}
}

// A cursor minted before a SUCCESSFUL whole-chain restore pins the old range
// [1,5]; the shorter restored chain no longer covers it and every in-range
// block was replaced anyway, so the continuation must fail with
// ErrQueryChanged and return nothing — the two qualification states may never
// be stitched. A fresh empty-cursor query then reads the restored chain.
func TestQueryTxsMinBlocksOldCursorDiesAfterSuccessfulRestore(t *testing.T) {
	index := buildMbRestoreOldChain(t)
	query := TxQuery{TxIDs: mbRestoreIDs(), MinBlocks: mbRestoreMinBlocks, PageSize: 2}
	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Hits, mbRestoreOldHits()[:2]) {
		t.Fatalf("first page hits=%v, want %v", first.Hits, mbRestoreOldHits()[:2])
	}
	requireMbRestorePageStats(t, first, 10, 5, 5)
	if first.NextCursor == "" {
		t.Fatal("first page over ten hits at page size 2 must carry a cursor")
	}

	if err := index.Restore(strings.NewReader(mbRestoreNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	query.Cursor = first.NextCursor
	page, err := index.QueryTxs(query)
	requireChangedZeroed(t, page, err)

	fresh, err := index.QueryTxs(TxQuery{TxIDs: mbRestoreIDs(), MinBlocks: mbRestoreMinBlocks})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Hits, mbRestoreNewHits()) {
		t.Fatalf("fresh post-restore hits=%v, want %v", fresh.Hits, mbRestoreNewHits())
	}
	requireMbRestorePageStats(t, fresh, 5, 3, 3)
}

// A snapshot whose LAST block has a wrong parent link must be rejected
// wholesale as ErrInvalidSnapshot: the old chain's MinBlocks answer and
// statistics stay exactly as they were, and a cursor minted before the failed
// restore keeps paginating the old chain through all remaining pages.
func TestQueryTxsMinBlocksRejectedRestoreKeepsOldChainAndCursor(t *testing.T) {
	index := buildMbRestoreOldChain(t)
	query := TxQuery{TxIDs: mbRestoreIDs(), MinBlocks: mbRestoreMinBlocks, PageSize: 2}
	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("first page over ten hits at page size 2 must carry a cursor")
	}

	err = index.Restore(strings.NewReader(mbRestoreBrokenSnapshot))
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("broken-tail restore err=%v, want ErrInvalidSnapshot", err)
	}
	if !strings.Contains(err.Error(), "does not link to its parent") {
		t.Fatalf("restore must fail on the last block's parent link: %v", err)
	}
	// Whole rejection: tip, heights, hashes and timestamps are the old chain's.
	if index.Tip != 5 {
		t.Fatalf("tip after rejected restore=%d, want 5", index.Tip)
	}
	for _, want := range mbRestoreOldChainBlocks() {
		got, ok := index.Blocks[want.Height]
		if !ok || !equalBlockContent(got, want) {
			t.Fatalf("height %d changed after rejected restore: got=%+v want=%+v", want.Height, got, want)
		}
	}
	for _, leaked := range []string{"j1", "j2", "j3"} {
		if _, ok := index.ByHash[leaked]; ok {
			t.Fatalf("snapshot hash %s from the rejected restore was left indexed", leaked)
		}
	}

	// A fresh query still returns the complete old-chain answer and stats.
	fresh, err := index.QueryTxs(TxQuery{TxIDs: mbRestoreIDs(), MinBlocks: mbRestoreMinBlocks})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Hits, mbRestoreOldHits()) {
		t.Fatalf("fresh query after rejected restore=%v, want %v", fresh.Hits, mbRestoreOldHits())
	}
	requireMbRestorePageStats(t, fresh, 10, 5, 5)

	// The cursor minted before the failed restore continues the old chain: the
	// remaining pages carry the pinned stats and concatenate without gaps.
	query.Cursor = first.NextCursor
	rest := collectPages(t, index, query)
	all := append([]TxHit{}, first.Hits...)
	for _, page := range rest {
		requireMbRestorePageStats(t, page, 10, 5, 5)
		all = append(all, page.Hits...)
	}
	if !reflect.DeepEqual(all, mbRestoreOldHits()) {
		t.Fatalf("paginated across rejected restore=%v, want %v", all, mbRestoreOldHits())
	}
}
