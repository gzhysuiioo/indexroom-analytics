package indexroom

import (
	"reflect"
	"testing"
)

// This file guards the MinBlocks threshold combined with a timestamp window
// against the reorg shape that is easiest to miss: the chain keeps the SAME
// height span, the SAME block hashes, the SAME parent links and the SAME
// ordered transaction lists at every height — only the timestamps of SOME
// blocks move. Nothing about the blocks' identity changes, yet the set of
// identifiers reaching MinBlocks flips, because qualification is counted over
// in-window blocks only. A fresh query (empty cursor) issued after such a
// reorg must re-judge qualification entirely by the NEW times: identifiers
// that lost their second in-window block must vanish completely, identifiers
// that gained one must appear with every in-window occurrence, and the
// whole-range statistics on every page must describe the new main chain —
// never a leftover of the pre-reorg qualification or counts.
//
// The window is [0, 100): start included, end excluded, MinBlocks = 2.
// Starting the window at zero is deliberate: h1's real timestamp of zero sits
// exactly at the included start and must count as a qualifying block on both
// chains, while h4's missing timestamp must never count (a missing time is
// not zero) and h6's timestamp equal to the window end must neither count
// toward qualification nor have its occurrences returned.
//
//	                   old time   new time   in window old -> new
//	h1 parent g  [old, old]   0          0     yes -> yes  (real zero at the included start, unchanged)
//	h2 parent h1 [old, old]  50        150     yes -> OUT  (old loses its second qualifying block)
//	h3 parent h2 [new, new] 150         40     OUT -> in   (new gains a qualifying block)
//	h4 parent h3 [new]      nil        nil     OUT -> OUT  (missing stays missing; never counts)
//	h5 parent h4 [new]       90         90     yes -> yes  (new's one in-window block on both chains)
//	h6 parent h5 [new]      100        100     OUT -> OUT  (equal to the excluded end; never counts)
//
// Qualification (threshold 2, in-window distinct blocks):
//
//	old: before {h1, h2} -> QUALIFIED;  after {h1} -> not qualified.
//	new: before {h5} -> not qualified;  after {h3, h5} -> QUALIFIED.
//	  (h4's nil and h6's end-equal 100 never help on either chain, and the
//	  duplicate inside h3 still counts as one block.)
//
// Old answer (range 1..6): h1 old@0, h1 old@1, h2 old@0, h2 old@1 = 4
// occurrences over 2 matched blocks (h1, h2), upper height 6.
// New answer (range 1..6): h3 new@0, h3 new@1, h5 new@0 = 3 occurrences over
// 2 matched blocks (h3, h5), upper height 6.
//
// The two answers share not a single record, and TotalMatches differs (4 vs
// 3), so any page that keeps pre-reorg qualification, pre-reorg counts, or
// stitches the two states together is observable. The pinned upper height
// stays 6 on both chains: the time filter shrinking the result set must never
// shrink ToHeight.

const (
	mbTwWindowStart = int64(0)
	mbTwWindowEnd   = int64(100)
	mbTwPageSize    = 2
	mbTwMinBlocks   = 2
)

func mbTwBlock(height int64, hash, parent string, txs []string, time *int64) Block {
	return Block{Height: height, Hash: hash, Parent: parent, Txs: txs, Time: time}
}

// mbTwOldChain is the main chain before the time-only reorg.
func mbTwOldChain() []Block {
	return []Block{
		mbTwBlock(1, "h1", "g", []string{"old", "old"}, intptr(0)),
		mbTwBlock(2, "h2", "h1", []string{"old", "old"}, intptr(50)),
		mbTwBlock(3, "h3", "h2", []string{"new", "new"}, intptr(150)),
		mbTwBlock(4, "h4", "h3", []string{"new"}, nil),
		mbTwBlock(5, "h5", "h4", []string{"new"}, intptr(90)),
		mbTwBlock(6, "h6", "h5", []string{"new"}, intptr(100)),
	}
}

// mbTwBranch is the alternate branch handed to Reorg. It starts at height 2
// rooted at the retained h1 and keeps every hash, parent link and transaction
// list; only the h2 and h3 timestamps move (h2 50->150 leaves the window,
// h3 150->40 enters it).
func mbTwBranch() []Block {
	return []Block{
		mbTwBlock(2, "h2", "h1", []string{"old", "old"}, intptr(150)),
		mbTwBlock(3, "h3", "h2", []string{"new", "new"}, intptr(40)),
		mbTwBlock(4, "h4", "h3", []string{"new"}, nil),
		mbTwBlock(5, "h5", "h4", []string{"new"}, intptr(90)),
		mbTwBlock(6, "h6", "h5", []string{"new"}, intptr(100)),
	}
}

// mbTwNewChain is the complete main chain after the reorg: retained h1 plus
// the re-timed branch.
func mbTwNewChain() []Block {
	chain := append([]Block{}, mbTwOldChain()[:1]...)
	return append(chain, mbTwBranch()...)
}

func buildMbTwOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range mbTwOldChain() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

func mbTwQuery() TxQuery {
	return TxQuery{
		TimeStart: intptr(mbTwWindowStart),
		TimeEnd:   intptr(mbTwWindowEnd),
		MinBlocks: mbTwMinBlocks,
		PageSize:  mbTwPageSize,
	}
}

// Hand-derived complete answers; the guarantee rests on these literal
// records and counts, not on recomputation by the code under test.
func mbTwOldHits() []TxHit {
	return []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "old", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "old", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "old", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "old", Position: 1},
	}
}

func mbTwNewHits() []TxHit {
	return []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "new", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "new", Position: 1},
		{Height: 5, BlockHash: "h5", TxID: "new", Position: 0},
	}
}

// The structural invariant the regression rests on: above the retained h1,
// the reorg changes timestamps only. Every height, hash, parent link and
// ordered transaction list is identical on the two chains, the tip stays 6,
// and exactly the two re-timed heights are reported as dropped.
func TestMinBlocksTimeWindowReorgFixtureMovesTimesOnly(t *testing.T) {
	oldChain, newChain := mbTwOldChain(), mbTwNewChain()
	if len(oldChain) != len(newChain) || len(oldChain) != 6 {
		t.Fatalf("chain lengths old=%d new=%d, want 6/6", len(oldChain), len(newChain))
	}
	for i := range oldChain {
		o, n := oldChain[i], newChain[i]
		if o.Height != n.Height || o.Hash != n.Hash || o.Parent != n.Parent {
			t.Fatalf("height %d identity changed: old={%d %s %s} new={%d %s %s}",
				i+1, o.Height, o.Hash, o.Parent, n.Height, n.Hash, n.Parent)
		}
		if !reflect.DeepEqual(o.Txs, n.Txs) {
			t.Fatalf("height %d transaction list changed: old=%v new=%v", o.Height, o.Txs, n.Txs)
		}
	}
	// Per-height transition table, including the unchanged real-zero h1, the
	// missing-on-both-chains h4 and the end-equal h6.
	window := timeWindow{enabled: true, start: mbTwWindowStart, end: mbTwWindowEnd}
	flips := map[int64]struct {
		oldIn, newIn bool
		oldTime      *int64
		newTime      *int64
	}{
		1: {true, true, intptr(0), intptr(0)},
		2: {true, false, intptr(50), intptr(150)},
		3: {false, true, intptr(150), intptr(40)},
		4: {false, false, nil, nil},
		5: {true, true, intptr(90), intptr(90)},
		6: {false, false, intptr(100), intptr(100)},
	}
	for height, want := range flips {
		o, n := oldChain[height-1].Time, newChain[height-1].Time
		if !sameIntptr(o, want.oldTime) || !sameIntptr(n, want.newTime) {
			t.Fatalf("height %d times old=%v new=%v, want %v/%v", height, o, n, want.oldTime, want.newTime)
		}
		if got := window.contains(o); got != want.oldIn {
			t.Fatalf("height %d old window membership=%v, want %v", height, got, want.oldIn)
		}
		if got := window.contains(n); got != want.newIn {
			t.Fatalf("height %d new window membership=%v, want %v", height, got, want.newIn)
		}
	}

	index := buildMbTwOldChain(t)
	dropped, err := index.Reorg(mbTwBranch())
	if err != nil {
		t.Fatalf("time-only reorg refused: %v", err)
	}
	if want := []int64{2, 3}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped=%v, want only the two re-timed heights %v", dropped, want)
	}
	if index.Tip != 6 {
		t.Fatalf("tip=%d, want unchanged 6", index.Tip)
	}
	for _, want := range newChain {
		got, ok := index.Blocks[want.Height]
		if !ok {
			t.Fatalf("height %d missing after reorg", want.Height)
		}
		if !sameBlock(got, want) {
			t.Fatalf("height %d stored block=%+v, want %+v", want.Height, got, want)
		}
	}
}

// The core guarantee: a fresh query after the time-only reorg re-judges
// MinBlocks qualification entirely by the new times. "old" (down to one
// in-window block) must vanish completely; "new" (up to two in-window
// blocks) must appear with every in-window occurrence — both h3 duplicates at
// their original positions, then h5 — and every page must carry the
// whole-range statistics of the NEW chain with the upper height still pinned
// at the unchanged tip.
func TestQueryTxsMinBlocksReJudgedAfterTimeOnlyReorg(t *testing.T) {
	index := buildMbTwOldChain(t)
	query := mbTwQuery()

	// Before the reorg: "old" qualifies through h1 (real zero at the included
	// window start) and h2; "new" has only h5, because h3 sits outside the
	// window, h4's missing timestamp is not a zero, and h6's timestamp equals
	// the excluded end. The duplicate inside h1 still counts as one block, so
	// "old" qualifies with exactly two blocks — and once qualified, both
	// occurrences per block are returned at their original positions.
	before := collectPages(t, index, query)
	if len(before) != 2 {
		t.Fatalf("old-chain pages=%d, want 2 (2/2 over four hits)", len(before))
	}
	var beforeHits []TxHit
	for _, page := range before {
		if page.TotalMatches != 4 || page.MatchedBlocks != 2 || page.ToHeight != 6 {
			t.Fatalf("old-chain page stats=%d/%d to=%d, want 4/2 to=6",
				page.TotalMatches, page.MatchedBlocks, page.ToHeight)
		}
		beforeHits = append(beforeHits, page.Hits...)
	}
	if !reflect.DeepEqual(beforeHits, mbTwOldHits()) {
		t.Fatalf("old-chain hits=%v, want %v", beforeHits, mbTwOldHits())
	}

	if _, err := index.Reorg(mbTwBranch()); err != nil {
		t.Fatalf("time-only reorg refused: %v", err)
	}

	// After the reorg, from an empty cursor: qualification is decided by the
	// new times. "old" is down to h1 alone — the duplicate inside h1 must not
	// count as a second block — so not a single "old" occurrence survives.
	// "new" qualifies through h3 and h5; h4 (missing time) and h6 (time equal
	// to the excluded end) still contribute nothing and are never returned.
	after := collectPages(t, index, query)
	if len(after) != 2 {
		t.Fatalf("new-chain pages=%d, want 2 (2/1 over three hits)", len(after))
	}
	var afterHits []TxHit
	for i, page := range after {
		// Whole-range statistics of the NEW chain on every page: not the
		// pre-reorg 4/2, and not recomputed from the current page's slice
		// (page 2 holds a single hit from a single block).
		if page.TotalMatches != 3 || page.MatchedBlocks != 2 {
			t.Fatalf("new-chain page %d stats=%d/%d, want the whole-range 3/2 of the new chain",
				i+1, page.TotalMatches, page.MatchedBlocks)
		}
		// The upper bound stays the pinned tip 6: fewer time-filtered results
		// must not shrink it.
		if page.ToHeight != 6 {
			t.Fatalf("new-chain page %d toHeight=%d, want the unchanged pinned tip 6", i+1, page.ToHeight)
		}
		afterHits = append(afterHits, page.Hits...)
	}
	if !reflect.DeepEqual(afterHits, mbTwNewHits()) {
		t.Fatalf("new-chain hits=%v, want %v", afterHits, mbTwNewHits())
	}
	for _, hit := range afterHits {
		if hit.TxID == "old" {
			t.Fatalf("identifier that lost qualification leaked into the new-chain result: %+v", hit)
		}
		if hit.Height == 4 || hit.Height == 6 {
			t.Fatalf("occurrence from a missing-time or end-equal block leaked into the result: %+v", hit)
		}
	}
	// Ordering is by height and in-block position alone; the moved timestamps
	// (h3 at 40 below h5 at 90) must not reorder anything.
	requireOrdered(t, afterHits)
}

// A continuation cursor minted before the reorg pins the old timestamp state
// through the range fingerprint. After the reorg — even though every block
// hash, parent link, transaction list and the tip height are unchanged — the
// old cursor must fail with ErrQueryChanged and a fully zeroed page: the two
// chain states' results may never be stitched together.
func TestQueryTxsMinBlocksPreReorgCursorDiesAfterTimeOnlyReorg(t *testing.T) {
	index := buildMbTwOldChain(t)
	first, err := index.QueryTxs(mbTwQuery())
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("first page over 4 matches (page size 2) must carry a cursor")
	}
	if !reflect.DeepEqual(first.Hits, mbTwOldHits()[:2]) {
		t.Fatalf("old first page hits=%v, want %v", first.Hits, mbTwOldHits()[:2])
	}

	if _, err := index.Reorg(mbTwBranch()); err != nil {
		t.Fatalf("time-only reorg refused: %v", err)
	}

	query := mbTwQuery()
	query.Cursor = first.NextCursor
	page, err := index.QueryTxs(query)
	requireChangedZeroed(t, page, err)

	// Restarting from an empty cursor works and describes only the new chain.
	fresh, err := index.QueryTxs(mbTwQuery())
	if err != nil {
		t.Fatal(err)
	}
	if fresh.TotalMatches != 3 || fresh.MatchedBlocks != 2 || fresh.ToHeight != 6 {
		t.Fatalf("fresh page after reorg stats=%d/%d to=%d, want 3/2 to=6",
			fresh.TotalMatches, fresh.MatchedBlocks, fresh.ToHeight)
	}
}

// A branch whose LAST block carries a wrong parent link must reject the whole
// reorg: no height is dropped, the pre-reorg hits and statistics are
// unchanged, and a cursor minted before the failed reorg keeps paginating the
// original chain.
func TestQueryTxsMinBlocksReorgWithBadTailParentFailsAndKeepsOldState(t *testing.T) {
	index := buildMbTwOldChain(t)
	first, err := index.QueryTxs(mbTwQuery())
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("first page over 4 matches (page size 2) must carry a cursor")
	}

	broken := mbTwBranch()
	broken[len(broken)-1].Parent = "not-h5" // the tail no longer links to h5
	dropped, err := index.Reorg(broken)
	if err == nil {
		t.Fatal("branch with a broken tail parent link was accepted")
	}
	if dropped != nil {
		t.Fatalf("rejected reorg reported dropped heights %v, want none", dropped)
	}
	if index.Tip != 6 {
		t.Fatalf("tip=%d after rejected reorg, want 6", index.Tip)
	}

	// The original chain still answers exactly as before the failed reorg.
	fresh := collectPages(t, index, mbTwQuery())
	var freshHits []TxHit
	for _, page := range fresh {
		if page.TotalMatches != 4 || page.MatchedBlocks != 2 || page.ToHeight != 6 {
			t.Fatalf("page after rejected reorg stats=%d/%d to=%d, want the original 4/2 to=6",
				page.TotalMatches, page.MatchedBlocks, page.ToHeight)
		}
		freshHits = append(freshHits, page.Hits...)
	}
	if !reflect.DeepEqual(freshHits, mbTwOldHits()) {
		t.Fatalf("hits after rejected reorg=%v, want the original %v", freshHits, mbTwOldHits())
	}

	// The cursor minted before the failed reorg is still valid and continues
	// the original chain with the original statistics.
	query := mbTwQuery()
	query.Cursor = first.NextCursor
	rest := collectPages(t, index, query)
	all := append([]TxHit{}, first.Hits...)
	for _, page := range rest {
		if page.TotalMatches != 4 || page.MatchedBlocks != 2 || page.ToHeight != 6 {
			t.Fatalf("continuation after rejected reorg stats=%d/%d to=%d, want the pinned 4/2 to=6",
				page.TotalMatches, page.MatchedBlocks, page.ToHeight)
		}
		all = append(all, page.Hits...)
	}
	if !reflect.DeepEqual(all, mbTwOldHits()) {
		t.Fatalf("paginated hits across rejected reorg=%v, want %v", all, mbTwOldHits())
	}
}

// Qualification is pinned to the whole range, never re-decided per page, and
// never left over from the pre-reorg chain: on the new chain the second page
// holds a single hit from a single block, yet it must still report the
// whole-range 3/2 of the new chain, and a query interleaved with the reorg
// must never mix the old qualification (4/2, "old" records) with the new one.
func TestQueryTxsMinBlocksNoStaleQualificationAcrossReorg(t *testing.T) {
	index := buildMbTwOldChain(t)

	// Drain the old chain fully so its qualification state would be the one
	// most recently computed, then reorg and re-query from an empty cursor.
	oldPages := collectPages(t, index, mbTwQuery())
	var oldHits []TxHit
	for _, page := range oldPages {
		oldHits = append(oldHits, page.Hits...)
	}
	if !reflect.DeepEqual(oldHits, mbTwOldHits()) {
		t.Fatalf("old-chain hits=%v, want %v", oldHits, mbTwOldHits())
	}

	if _, err := index.Reorg(mbTwBranch()); err != nil {
		t.Fatal(err)
	}

	pages := collectPages(t, index, mbTwQuery())
	var all []TxHit
	for _, page := range pages {
		if page.TotalMatches != 3 || page.MatchedBlocks != 2 || page.ToHeight != 6 {
			t.Fatalf("stale pre-reorg statistics survived the reorg: %+v", page)
		}
		all = append(all, page.Hits...)
	}
	if !reflect.DeepEqual(all, mbTwNewHits()) {
		t.Fatalf("post-reorg hits=%v, want %v", all, mbTwNewHits())
	}
}
