package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// This file guards a paginated QueryTxs that combines the MinBlocks threshold
// with the half-open timestamp window against the reorg shape that is easiest
// to miss: the chain keeps the SAME height span, the SAME block hashes, the
// SAME parent links and the SAME ordered transaction lists at every height —
// only the timestamps of SOME blocks move — yet that time movement changes
// WHICH IDENTIFIERS reach the threshold.
//
// A MinBlocks qualification that is cached against block identity (hashes,
// parents, transaction lists), or that is counted without the timestamp
// window, would survive this reorg and keep serving the OLD qualification:
// the identifier the new times push below the threshold would still be
// returned, and the identifier the new times push up to it would still be
// absent. The guarantee is that a query restarted from an empty cursor after
// the reorg judges qualification entirely against the NEW times and the
// window — and that every page, its whole-range statistics and the validity
// of old cursors are consistent with exactly one chain state.
//
// Threshold is two; the window is the half-open [0, 110): start (a real zero)
// included, end excluded, a missing timestamp rejected. Timestamps are
// deliberately non-monotonic in height so the window can never be confused
// with a height range. Leaving and entering blocks are kept identifier-pure so
// each flip moves exactly one identifier's count:
//
//	                    txs       old time   new time   in window old -> new
//	h1 parent g  [a, a, z]   0          0        yes -> yes   (real zero at the included start; a repeats)
//	h2 parent h1 [a, x]     105        120       yes -> OUT   (a leaves; carries no b)
//	h3 parent h2 [b, b]     110        100       OUT -> in    (excluded end becomes included; b repeats)
//	h4 parent h3 [b, q]     108        102       yes -> yes   (b's persistent in-window block)
//	h5 parent h4 [a, b]      nil       nil       OUT -> OUT   (missing never matches on either chain)
//
// Filter ["a","b"], MinBlocks 2, window [0,110), ascending.
//
// Old chain: in-window blocks are h1 (time 0), h2 (105), h4 (108). a occupies
// {h1,h2} = 2 distinct blocks -> qualifies; b occupies only {h4} = 1 (h3 sits
// at the excluded end and h5 has no time) -> does not. Kept occurrences are
// a's three: the repeated h1 a@0,a@1 and h2 a@0 = TotalMatches 3 over
// MatchedBlocks {h1,h2} = 2, upper height 5.
//
// New chain: in-window blocks are h1 (0), h3 (100), h4 (102). a now occupies
// only {h1} = 1 (h2 left the window; its two appearances in h1 are still just
// ONE block) -> it loses qualification and EVERY a occurrence is excluded,
// including the in-window h1 pair. b occupies {h3,h4} = 2 -> it now qualifies,
// and all of its occurrences are kept: the repeated h3 b@0,b@1 and h4 b@0 =
// TotalMatches 3 over MatchedBlocks {h3,h4} = 2, upper height still 5.
//
// The two complete answers are deliberately symmetric in their counts
// (3 occurrences over 2 blocks, tip 5) and share NO record: the old answer is
// all "a", the new answer is all "b". Only the identity of the qualified set
// and the returned records distinguishes the states, which is exactly the
// behavior being protected. z/x/q are outside the TxIDs filter and never ride
// in on either identifier's qualification.

const (
	mbShiftWindowStart = int64(0)
	mbShiftWindowEnd   = int64(110)
	mbShiftThreshold   = int64(2)
	mbShiftPageSize    = 2
)

// mbShiftOldChain is the main chain before the time-only reorg.
func mbShiftOldChain() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"a", "a", "z"}, Time: intptr(0)},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a", "x"}, Time: intptr(105)},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"b", "b"}, Time: intptr(110)},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"b", "q"}, Time: intptr(108)},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"a", "b"}, Time: nil},
	}
}

// mbShiftBranch is the alternate branch handed to Reorg. It starts at height 2
// rooted at the retained h1 and keeps every hash, parent link and transaction
// list; only timestamps move (h2 105->120, h3 110->100, h4 108->102). h1 at
// real zero is the retained root and is untouched, and h5 is byte-identical
// (still missing its timestamp), so the reorg reports exactly heights 2..4.
func mbShiftBranch() []Block {
	return []Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a", "x"}, Time: intptr(120)},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"b", "b"}, Time: intptr(100)},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"b", "q"}, Time: intptr(102)},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"a", "b"}, Time: nil},
	}
}

// mbShiftNewChain is the complete main chain after the reorg: retained h1 plus
// the re-timed branch.
func mbShiftNewChain() []Block {
	chain := append([]Block{}, mbShiftOldChain()[:1]...)
	return append(chain, mbShiftBranch()...)
}

func buildMbShiftOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range mbShiftOldChain() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

func mbShiftQuery() TxQuery {
	return TxQuery{
		TxIDs:     []string{"a", "b"},
		TimeStart: intptr(mbShiftWindowStart),
		TimeEnd:   intptr(mbShiftWindowEnd),
		MinBlocks: mbShiftThreshold,
		PageSize:  mbShiftPageSize,
	}
}

// mbReferenceAnswer is an independent oracle: over an explicit chain it first
// counts, per filtered identifier, the distinct in-window blocks that hold it
// (repeated appearances in one block contribute one block), keeps the
// identifiers reaching minBlocks, then returns EVERY in-window occurrence of a
// qualified identifier in height/position order — together with the total
// occurrence count and the count of blocks that hold a kept occurrence. It
// never touches the Index under test, so a QueryTxs bug that cached or
// window-ignored qualification could not make an expectation match itself.
func mbReferenceAnswer(chain []Block, minBlocks int64) (hits []TxHit, total, blocks int64) {
	filtered := map[string]bool{"a": true, "b": true}
	inWindow := func(when *int64) bool {
		if when == nil {
			return false // a missing timestamp never matches, unlike a real zero
		}
		return *when >= mbShiftWindowStart && *when < mbShiftWindowEnd // start in, end out
	}
	counts := map[string]int64{}
	for _, block := range chain {
		if !inWindow(block.Time) {
			continue
		}
		seen := map[string]bool{}
		for _, tx := range block.Txs {
			if !filtered[tx] || seen[tx] {
				continue
			}
			seen[tx] = true
			counts[tx]++
		}
	}
	qualified := map[string]bool{}
	for id, n := range counts {
		if n >= minBlocks {
			qualified[id] = true
		}
	}
	matchedBlocks := map[int64]bool{}
	hits = []TxHit{}
	for _, block := range chain { // height order; timestamps are non-monotonic
		if !inWindow(block.Time) {
			continue
		}
		for position, tx := range block.Txs { // original in-block position order
			if !qualified[tx] {
				continue
			}
			hits = append(hits, TxHit{Height: block.Height, BlockHash: block.Hash, TxID: tx, Position: position})
			matchedBlocks[block.Height] = true
		}
	}
	return hits, int64(len(hits)), int64(len(matchedBlocks))
}

// Hand-derived complete answers; the guarantee rests on these literal records.
func mbLiteralOldHits() []TxHit {
	return []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
	}
}

func mbLiteralNewHits() []TxHit {
	return []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "b", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "b", Position: 1},
		{Height: 4, BlockHash: "h4", TxID: "b", Position: 0},
	}
}

// The structural invariant the regression rests on: above the retained h1 the
// reorg moves timestamps only (h5 is byte-identical), yet one identifier loses
// its threshold standing and the other gains it.
func TestReorgMinBlocksFixtureMovesTimesOnlyAndFlipsQualification(t *testing.T) {
	oldChain, newChain := mbShiftOldChain(), mbShiftNewChain()
	if len(oldChain) != len(newChain) || len(oldChain) != 5 {
		t.Fatalf("chain lengths old=%d new=%d, want 5/5", len(oldChain), len(newChain))
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
	// Per-height window-membership transition table.
	flips := map[int64]struct {
		oldIn, newIn bool
		oldTime      *int64
		newTime      *int64
	}{
		1: {true, true, intptr(0), intptr(0)},
		2: {true, false, intptr(105), intptr(120)},
		3: {false, true, intptr(110), intptr(100)},
		4: {true, true, intptr(108), intptr(102)},
		5: {false, false, nil, nil},
	}
	window := timeWindow{enabled: true, start: mbShiftWindowStart, end: mbShiftWindowEnd}
	movedOut, movedIn := 0, 0
	for height, want := range flips {
		o, n := oldChain[height-1].Time, newChain[height-1].Time
		if !sameIntptr(o, want.oldTime) || !sameIntptr(n, want.newTime) {
			t.Fatalf("height %d times old=%v new=%v, want %v/%v", height, o, n, want.oldTime, want.newTime)
		}
		if window.contains(o) != want.oldIn || window.contains(n) != want.newIn {
			t.Fatalf("height %d window membership old/new not %v/%v", height, want.oldIn, want.newIn)
		}
		if want.oldIn && !want.newIn {
			movedOut++
		}
		if !want.oldIn && want.newIn {
			movedIn++
		}
	}
	if movedOut != 1 || movedIn != 1 {
		t.Fatalf("boundary crossings: movedOut=%d movedIn=%d, want 1/1", movedOut, movedIn)
	}

	// Independent oracle on both chains.
	oldHits, oldTotal, oldBlocks := mbReferenceAnswer(oldChain, mbShiftThreshold)
	newHits, newTotal, newBlocks := mbReferenceAnswer(newChain, mbShiftThreshold)
	if !reflect.DeepEqual(oldHits, mbLiteralOldHits()) {
		t.Fatalf("old oracle hits=%v, want %v", oldHits, mbLiteralOldHits())
	}
	if !reflect.DeepEqual(newHits, mbLiteralNewHits()) {
		t.Fatalf("new oracle hits=%v, want %v", newHits, mbLiteralNewHits())
	}
	if oldTotal != 3 || oldBlocks != 2 {
		t.Fatalf("old oracle stats=%d/%d, want 3 occurrences over 2 blocks", oldTotal, oldBlocks)
	}
	if newTotal != 3 || newBlocks != 2 {
		t.Fatalf("new oracle stats=%d/%d, want 3 occurrences over 2 blocks", newTotal, newBlocks)
	}
	requireOrdered(t, oldHits)
	requireOrdered(t, newHits)

	// Identifier-level qualification, the exact behavior being protected:
	// old chain qualifies a only; new chain flips to b only. a's two h1
	// appearances still count as ONE distinct block, which is why a drops to a
	// single block and is fully excluded after the reorg. The filtered-out
	// z/x/q and the timestamp-less h5 never help either identifier.
	mbRequireQualified := func(chain []Block, want map[string]bool) {
		t.Helper()
		counts := map[string]int64{}
		for _, block := range chain {
			if !window.contains(block.Time) {
				continue
			}
			seen := map[string]bool{}
			for _, tx := range block.Txs {
				if (tx != "a" && tx != "b") || seen[tx] {
					continue
				}
				seen[tx] = true
				counts[tx]++
			}
		}
		got := map[string]bool{}
		for id, n := range counts {
			got[id] = n >= mbShiftThreshold
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("qualification=%v, want %v (counts=%v)", got, want, counts)
		}
	}
	mbRequireQualified(oldChain, map[string]bool{"a": true, "b": false})
	mbRequireQualified(newChain, map[string]bool{"a": false, "b": true})

	// A live time-only Reorg reports exactly the re-timed heights (h1 retained,
	// h5 byte-identical), keeps the tip at 5, and leaves identities in place.
	index := buildMbShiftOldChain(t)
	dropped, err := index.Reorg(mbShiftBranch())
	if err != nil {
		t.Fatalf("time-only reorg refused: %v", err)
	}
	if want := []int64{2, 3, 4}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped=%v, want the three re-timed heights %v", dropped, want)
	}
	if index.Tip != 5 {
		t.Fatalf("tip=%d, want unchanged 5", index.Tip)
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

// On the static old chain the threshold filter behaves as the baseline tests
// expect: a qualifies and keeps all three occurrences (the repeated h1 pair
// plus h2), while b — one in-window block, with another sitting at the
// excluded end and one in a timestamp-less block — is fully absent.
func TestReorgMinBlocksOldChainStaticFiltering(t *testing.T) {
	index := buildMbShiftOldChain(t)
	page, err := index.QueryTxs(mbShiftQuery())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page.Hits, mbLiteralOldHits()[:mbShiftPageSize]) {
		t.Fatalf("old first page hits=%v, want %v", page.Hits, mbLiteralOldHits()[:mbShiftPageSize])
	}
	if page.TotalMatches != 3 || page.MatchedBlocks != 2 || page.ToHeight != 5 {
		t.Fatalf("old first page stats=%+v, want 3/2/ToHeight 5", page)
	}
	all := collectAllMbPages(t, index, mbShiftQuery())
	if !reflect.DeepEqual(all, mbLiteralOldHits()) {
		t.Fatalf("old full walk=%v, want %v", all, mbLiteralOldHits())
	}
	for _, hit := range all {
		if hit.TxID != "a" {
			t.Fatalf("only a qualifies on the old chain, got %+v", hit)
		}
	}
}

// The core guarantee: after the time-only reorg, a query restarted from an
// empty cursor judges MinBlocks against the NEW times. The identifier pushed
// below the threshold is COMPLETELY excluded — even its in-window occurrences,
// and even though it appears twice inside the surviving h1 (one block) — while
// the identifier pushed up to the threshold returns EVERY in-window
// appearance, including the repeated h3 appearances at their original
// positions. The filtered-out z/x/q and the timestamp-less h5 stay absent.
func TestReorgMinBlocksRestartRejudgesQualificationAgainstNewTimes(t *testing.T) {
	index := buildMbShiftOldChain(t)
	if _, err := index.Reorg(mbShiftBranch()); err != nil {
		t.Fatalf("time-only reorg refused: %v", err)
	}

	page, err := index.QueryTxs(mbShiftQuery())
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := mbLiteralNewHits()[:mbShiftPageSize]
	if !reflect.DeepEqual(page.Hits, wantFirst) {
		t.Fatalf("new first page hits=%v, want %v", page.Hits, wantFirst)
	}
	// Whole-new-chain statistics, never re-counted over just this page: three
	// occurrences over two matched blocks, and the pinned upper height is not
	// shrunk just because the time filter plus qualification remove records.
	if page.TotalMatches != 3 || page.MatchedBlocks != 2 || page.ToHeight != 5 {
		t.Fatalf("new first page stats=%+v, want 3/2/ToHeight 5", page)
	}
	if page.NextCursor == "" {
		t.Fatal("three new-chain occurrences at page size 2 must carry a cursor")
	}

	all := collectAllMbPages(t, index, mbShiftQuery())
	if !reflect.DeepEqual(all, mbLiteralNewHits()) {
		t.Fatalf("restarted query hits=%v, want %v", all, mbLiteralNewHits())
	}

	// The disqualified identifier must be entirely gone: its h2 occurrence
	// left the window, and its in-window h1 pair is removed because a now has
	// only one distinct in-window block despite appearing twice there.
	for _, hit := range all {
		if hit.TxID == "a" {
			t.Fatalf("disqualified a must be fully excluded after restart: %+v", hit)
		}
	}
	for _, gone := range mbLiteralOldHits() {
		if containsHit(all, gone) {
			t.Fatalf("old-chain occurrence must not survive the restart: %+v", gone)
		}
	}
	// The newly qualified identifier returns every in-window appearance: both
	// repeated h3 appearances (one qualifying block, two returned hits at
	// positions 0 and 1) and the persistent h4 appearance.
	for _, gained := range mbLiteralNewHits() {
		if !containsHit(all, gained) {
			t.Fatalf("newly qualified b must return %+v after restart: %v", gained, all)
		}
	}
	// Non-filtered companions and timestamp-less occurrences never ride in.
	for _, hit := range all {
		if hit.TxID == "z" || hit.TxID == "x" || hit.TxID == "q" {
			t.Fatalf("non-filtered occurrence leaked into the answer: %+v", hit)
		}
		if hit.Height == 5 {
			t.Fatalf("timestamp-less h5 occurrence must never be returned: %+v", hit)
		}
	}
}

// collectAllMbPages restarts from a fresh first page (empty cursor), follows
// the cursor chain to the end, asserts every page carries the same whole-range
// statistics and upper height, and returns the concatenated hits.
func collectAllMbPages(t *testing.T, index *Index, query TxQuery) []TxHit {
	t.Helper()
	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatalf("first page failed: %v", err)
	}
	var all []TxHit
	page := first
	for {
		if page.TotalMatches != first.TotalMatches || page.MatchedBlocks != first.MatchedBlocks ||
			page.ToHeight != first.ToHeight {
			t.Fatalf("page statistics drifted from the first page: first=%+v page=%+v", first, page)
		}
		all = append(all, page.Hits...)
		if page.NextCursor == "" {
			return all
		}
		next := query
		next.Cursor = page.NextCursor
		page, err = index.QueryTxs(next)
		if err != nil {
			t.Fatalf("continuation failed: %v", err)
		}
	}
}

// When the new answer spans several pages, EVERY page describes the complete
// new-chain filtered range: the same TotalMatches and MatchedBlocks as the
// first page, qualification decided over the whole range rather than re-judged
// on the current page, and no residue of the old qualification (the pages
// carry only b, never a).
func TestReorgMinBlocksPagedStatsDescribeWholeNewChain(t *testing.T) {
	index := buildMbShiftOldChain(t)
	if _, err := index.Reorg(mbShiftBranch()); err != nil {
		t.Fatalf("time-only reorg refused: %v", err)
	}

	query := mbShiftQuery() // page size 2 over three new occurrences -> 2/1
	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	pages := []TxPage{first}
	for pages[len(pages)-1].NextCursor != "" {
		next := query
		next.Cursor = pages[len(pages)-1].NextCursor
		page, err := index.QueryTxs(next)
		if err != nil {
			t.Fatalf("continuation %d failed: %v", len(pages)+1, err)
		}
		pages = append(pages, page)
	}
	if len(pages) != 2 {
		t.Fatalf("pages=%d, want 2 (2/1 over three hits)", len(pages))
	}
	for i, page := range pages {
		if page.TotalMatches != 3 || page.MatchedBlocks != 2 || page.ToHeight != 5 {
			t.Fatalf("page %d stats=%+v, want whole new-chain 3/2/ToHeight 5", i+1, page)
		}
		for _, hit := range page.Hits {
			if hit.TxID != "b" {
				t.Fatalf("page %d must carry only the newly qualified b, got %+v", i+1, hit)
			}
		}
	}
	wantPages := [][]TxHit{mbLiteralNewHits()[0:2], mbLiteralNewHits()[2:3]}
	for i := range pages {
		if !reflect.DeepEqual(pages[i].Hits, wantPages[i]) {
			t.Fatalf("page %d hits=%v, want %v", i+1, pages[i].Hits, wantPages[i])
		}
	}

	// Page size does not re-decide qualification: a one-shot large page and a
	// size-one walk return the identical complete answer and fixed stats.
	big, err := index.QueryTxs(func() TxQuery {
		q := mbShiftQuery()
		q.PageSize = MaxPageSize
		return q
	}())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(big.Hits, mbLiteralNewHits()) || big.TotalMatches != 3 || big.MatchedBlocks != 2 {
		t.Fatalf("one-shot page=%+v, want the three literal new hits with 3/2", big)
	}
	ones := collectAllMbPages(t, index, func() TxQuery {
		q := mbShiftQuery()
		q.PageSize = 1
		return q
	}())
	if !reflect.DeepEqual(ones, mbLiteralNewHits()) {
		t.Fatalf("size-one walk=%v, want %v", ones, mbLiteralNewHits())
	}
}

// Ordering is decided by height and original in-block position alone; the time
// movement neither re-sorts the records nor renumbers positions, and the
// descending read over the new times is the same kept occurrences reversed by
// height/position with identical whole-range statistics.
func TestReorgMinBlocksOrderingAndPositionsSurviveTimeShift(t *testing.T) {
	index := buildMbShiftOldChain(t)
	if _, err := index.Reorg(mbShiftBranch()); err != nil {
		t.Fatalf("time-only reorg refused: %v", err)
	}

	asc := collectAllMbPages(t, index, mbShiftQuery())
	requireOrdered(t, asc)
	for _, want := range mbLiteralNewHits() {
		if !containsHit(asc, want) {
			t.Fatalf("missing literal new occurrence with original position: %+v in %v", want, asc)
		}
	}

	descQuery := mbShiftQuery()
	descQuery.Order = OrderDesc
	desc, err := index.QueryTxs(descQuery)
	if err != nil {
		t.Fatal(err)
	}
	if desc.TotalMatches != 3 || desc.MatchedBlocks != 2 || desc.ToHeight != 5 {
		t.Fatalf("descending stats=%+v, want 3/2/ToHeight 5", desc)
	}
	descAll := collectAllMbPages(t, index, descQuery)
	var reversed []TxHit
	for i := len(asc) - 1; i >= 0; i-- {
		reversed = append(reversed, asc[i])
	}
	if !reflect.DeepEqual(descAll, reversed) {
		t.Fatalf("descending hits=%v, want ascending reversed %v", descAll, reversed)
	}
}

// The zero/end/missing timestamp rules participate in qualification on this
// exact fixture, on both chain states: the real-zero h1 is the included start,
// the t=110 h3 is the excluded end before it moves and must not give b its
// second block, and the timestamp-less h5 never counts.
func TestReorgMinBlocksTimestampBoundariesOnBothChains(t *testing.T) {
	oldIndex := buildMbShiftOldChain(t)
	// Without the threshold, the window alone keeps h1 (real zero), h2 and h4,
	// never the t=110 h3 or the timestamp-less h5.
	noThreshold := func() TxQuery {
		q := mbShiftQuery()
		q.MinBlocks = 0
		return q
	}
	oldHeights := map[int64]bool{}
	for _, h := range collectAllMbPages(t, oldIndex, noThreshold()) {
		oldHeights[h.Height] = true
	}
	if !reflect.DeepEqual(oldHeights, map[int64]bool{1: true, 2: true, 4: true}) {
		t.Fatalf("old no-threshold window matched heights %v, want {1,2,4} (real zero in, end and missing out)", oldHeights)
	}
	// A window that only contains the zero second keeps h1 alone, proving the
	// real zero hits the included start rather than being treated as missing.
	zeroOnly, err := oldIndex.QueryTxs(TxQuery{TimeStart: intptr(0), TimeEnd: intptr(1)})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range zeroOnly.Hits {
		if h.Height != 1 {
			t.Fatalf("window [0,1) must keep only h1, got height %d", h.Height)
		}
	}
	if zeroOnly.TotalMatches != 3 { // [a,a,z]
		t.Fatalf("window [0,1) total=%d, want h1's three transactions", zeroOnly.TotalMatches)
	}

	// After the reorg the same no-threshold window keeps h1, h3 and h4; h2 now
	// sits at 120 outside the end and h5 is still missing its time. With the
	// threshold back on, only h3/h4's b qualifies and h1's a is excluded.
	if _, err := oldIndex.Reorg(mbShiftBranch()); err != nil {
		t.Fatal(err)
	}
	gotHeights := map[int64]bool{}
	for _, h := range collectAllMbPages(t, oldIndex, noThreshold()) {
		gotHeights[h.Height] = true
	}
	if !reflect.DeepEqual(gotHeights, map[int64]bool{1: true, 3: true, 4: true}) {
		t.Fatalf("new no-threshold window matched heights %v, want {1,3,4}", gotHeights)
	}
	qualified := collectAllMbPages(t, oldIndex, mbShiftQuery())
	gotHeights = map[int64]bool{}
	for _, h := range qualified {
		if h.TxID != "b" {
			t.Fatalf("threshold over the new times must keep only b, got %+v", h)
		}
		gotHeights[h.Height] = true
	}
	if !reflect.DeepEqual(gotHeights, map[int64]bool{3: true, 4: true}) {
		t.Fatalf("new qualified heights %v, want {3,4} (h1's a has only one in-window block)", gotHeights)
	}
}

// A cursor minted on the old chain before the time-only reorg must die with a
// zeroed ErrQueryChanged afterwards, even though every block hash, parent and
// transaction list is unchanged — the timestamps alone moved. The two chain
// states must never be stitched together; the caller can only restart from an
// empty cursor.
func TestReorgMinBlocksOldCursorDiesAcrossTimeOnlyReorg(t *testing.T) {
	index := buildMbShiftOldChain(t)
	first, err := index.QueryTxs(mbShiftQuery())
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("three old occurrences at page size 2 must carry a cursor")
	}
	// The cursor pins the OLD timestamp state through its range fingerprint,
	// and records the old threshold/window so it can only continue that query.
	oldFingerprint := buildMbShiftOldChain(t)
	payload := decodePayload(t, index, first.NextCursor)
	if payload.To != 5 || payload.Off != 2 || payload.MinBlocks != mbShiftThreshold {
		t.Fatalf("cursor pins to=%d off=%d minBlocks=%d, want 5/2/2", payload.To, payload.Off, payload.MinBlocks)
	}
	if payload.window() != (timeWindow{enabled: true, start: mbShiftWindowStart, end: mbShiftWindowEnd}) {
		t.Fatalf("cursor window=%+v, want pinned [0,110)", payload.window())
	}
	if payload.FP != fmt.Sprintf("%x", oldFingerprint.fingerprintLocked(1, 5)) {
		t.Fatal("cursor fingerprint does not match the complete OLD timestamp state")
	}

	if _, err := index.Reorg(mbShiftBranch()); err != nil {
		t.Fatalf("time-only reorg refused: %v", err)
	}
	cont := mbShiftQuery()
	cont.Cursor = first.NextCursor
	page, err := index.QueryTxs(cont)
	requireChangedZeroed(t, page, err)

	// A fresh restart against the same query arguments succeeds and is fully
	// new-chain: the continuation error is ErrQueryChanged, never an argument
	// error, and nothing about the old cursor is reusable.
	restartPage, err := index.QueryTxs(mbShiftQuery())
	if err != nil {
		t.Fatalf("fresh restart after a data change must succeed: %v", err)
	}
	if !reflect.DeepEqual(restartPage.Hits, mbLiteralNewHits()[:mbShiftPageSize]) {
		t.Fatalf("restart first page=%v, want the first two new b hits", restartPage.Hits)
	}
	if restartPage.TotalMatches != 3 || restartPage.MatchedBlocks != 2 {
		t.Fatalf("restart stats=%+v, want new-chain 3/2", restartPage)
	}
}

// An old cursor stays valid while the timestamps are untouched: re-submitting
// the identical branch is a no-op-style reorg that reports no dropped heights,
// and the cursor then drains the complete OLD answer (a only, three hits),
// proving the cursor is invalidated by the timestamp change itself rather than
// by the mere act of reorganizing.
func TestReorgMinBlocksCursorSurvivesIdenticalBranch(t *testing.T) {
	index := buildMbShiftOldChain(t)
	first, err := index.QueryTxs(mbShiftQuery())
	if err != nil {
		t.Fatal(err)
	}
	dropped, err := index.Reorg(mbShiftOldChain()[1:]) // heights 2..5 verbatim, times included
	if err != nil {
		t.Fatalf("identical-branch reorg refused: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("re-submitting the identical branch dropped=%v, want none", dropped)
	}
	cont := mbShiftQuery()
	cont.Cursor = first.NextCursor
	page, err := index.QueryTxs(cont)
	if err != nil {
		t.Fatalf("cursor must survive an identical-branch reorg: %v", err)
	}
	if page.TotalMatches != 3 || page.MatchedBlocks != 2 || page.ToHeight != 5 {
		t.Fatalf("identical-branch continuation stats=%+v, want old-chain 3/2/5", page)
	}
	all := append(append([]TxHit{}, first.Hits...), page.Hits...)
	cursor := page.NextCursor
	for cursor != "" {
		next := mbShiftQuery()
		next.Cursor = cursor
		page, err = index.QueryTxs(next)
		if err != nil {
			t.Fatalf("continuation failed: %v", err)
		}
		all = append(all, page.Hits...)
		cursor = page.NextCursor
	}
	if !reflect.DeepEqual(all, mbLiteralOldHits()) {
		t.Fatalf("identical-branch walk=%v, want the old-chain hits %v", all, mbLiteralOldHits())
	}
}

// Reorg is atomic: a branch whose LAST block carries a wrong parent link is
// rejected after the whole branch has been validated, so nothing is applied.
// The old chain's MinBlocks hits, whole-range statistics, tip and a cursor
// minted before the failed attempt all keep working against the old chain.
func TestReorgMinBlocksBrokenBranchFailsAtomicallyLeavingOldChain(t *testing.T) {
	index := buildMbShiftOldChain(t)
	first, err := index.QueryTxs(mbShiftQuery())
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("old first page over three hits must carry a cursor")
	}

	// Same time movement as the valid branch, but the terminal h5 names a
	// wrong parent (its predecessor is h4, not h3). The validation error is in
	// the last block on purpose.
	broken := mbShiftBranch()
	broken[len(broken)-1].Parent = "h3" // h5 must parent h4
	dropped, err := index.Reorg(broken)
	if err == nil {
		t.Fatal("a branch whose final parent link is wrong must be rejected")
	}
	if !strings.Contains(err.Error(), "branch parent does not match") {
		t.Fatalf("err=%v, want a branch-parent validation error", err)
	}
	if errors.Is(err, ErrInvalidArgument) || errors.Is(err, ErrQueryChanged) {
		t.Fatalf("an ingestion rejection must not be a query error: %v", err)
	}
	if dropped != nil {
		t.Fatalf("a rejected reorg must report no dropped heights, got %v", dropped)
	}
	if index.Tip != 5 {
		t.Fatalf("tip=%d after a failed reorg, want unchanged 5", index.Tip)
	}

	// Old-chain state is fully intact: a fresh page still shows a only with the
	// old 3/2 statistics, and the stored blocks are byte-for-byte the old ones.
	fresh, err := index.QueryTxs(mbShiftQuery())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Hits, mbLiteralOldHits()[:mbShiftPageSize]) {
		t.Fatalf("after a failed reorg fresh hits=%v, want old-chain %v", fresh.Hits, mbLiteralOldHits()[:mbShiftPageSize])
	}
	if fresh.TotalMatches != 3 || fresh.MatchedBlocks != 2 || fresh.ToHeight != 5 {
		t.Fatalf("after a failed reorg stats=%+v, want old-chain 3/2/5", fresh)
	}
	for _, want := range mbShiftOldChain() {
		if got := index.Blocks[want.Height]; !sameBlock(got, want) {
			t.Fatalf("height %d changed after a failed reorg: got=%+v want=%+v", want.Height, got, want)
		}
	}

	// The cursor minted before the failed attempt is still valid and drains the
	// complete old answer.
	all := append([]TxHit{}, first.Hits...)
	cursor := first.NextCursor
	for cursor != "" {
		cont := mbShiftQuery()
		cont.Cursor = cursor
		next, err := index.QueryTxs(cont)
		if err != nil {
			t.Fatalf("old cursor must remain valid after a failed reorg: %v", err)
		}
		if next.TotalMatches != 3 || next.MatchedBlocks != 2 || next.ToHeight != 5 {
			t.Fatalf("old continuation stats=%+v, want 3/2/5", next)
		}
		all = append(all, next.Hits...)
		cursor = next.NextCursor
	}
	if !reflect.DeepEqual(all, mbLiteralOldHits()) {
		t.Fatalf("old cursor walk after failed reorg=%v, want %v", all, mbLiteralOldHits())
	}
}
