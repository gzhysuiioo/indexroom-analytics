package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This file guards a paginated, time-windowed QueryTxs against the reorg shape
// that is easiest to miss: the chain keeps the SAME height span, the SAME block
// hashes, the SAME parent links and the SAME ordered transaction lists at
// every height — only the timestamps of SOME blocks move. The pinned upper
// height therefore does not change and no hash appears or vanishes; the only
// thing that changes is which occurrences survive the half-open timestamp
// window. A page still has to describe ONE complete main chain: its hit
// records, TotalMatches, MatchedBlocks, the pinned upper height and the
// continuation cursor must all be old-chain or all new-chain, never hits read
// under the old times with statistics counted under the new times.
//
// The window is [0, 110): start included, end excluded. Starting the window at
// zero is deliberate: h1's real timestamp of zero sits exactly at the included
// start and must stay matched both before and after the reorg, while h4's
// missing timestamp is rejected, so the "zero is a value, nil is absent"
// distinction survives the reorg instead of being asserted only on static
// chains. Timestamps are deliberately non-monotonic in height (110 appears
// below 105), so the window can never be mistaken for a height range.
//
//	                    old time   new time   in window old -> new
//	h1 parent g  [m, x]    0          0        yes -> yes   (real zero at the included start, unchanged)
//	h2 parent h1 [m, m]   105        120       yes -> OUT   (both occurrences leave together)
//	h3 parent h2 [m, z]   110        100       OUT -> in    (excluded end becomes included)
//	h4 parent h3 [m, q]    nil       105       OUT -> in    (missing stays excluded, a value enters)
//	h5 parent h4 [m, m]   108        102       yes -> yes   (stays, times still differ)
//
// Filter "m", window [0,110), ascending, page size 2.
//
// Old answer (range 1..5): h1 m@0, h2 m@0, h2 m@1, h5 m@0, h5 m@1 = 5
// occurrences over 3 matched blocks (h1, h2, h5), upper height 5.
// New answer (range 1..5): h1 m@0, h3 m@0, h4 m@0, h5 m@0, h5 m@1 = 5
// occurrences over 4 matched blocks (h1, h3, h4, h5), upper height 5.
//
// The totals are deliberately equal (5) while every other observable differs:
// the two complete answers share only the h1 and h5 records. A torn page whose
// hits come from the old times but whose MatchedBlocks came from the new times
// (or the reverse) is exposed even though TotalMatches alone would look fine.

const (
	twShiftWindowStart = int64(0)
	twShiftWindowEnd   = int64(110)
	twShiftPageSize    = 2
)

func twShiftBlock(height int64, hash, parent string, txs []string, time *int64) Block {
	return Block{Height: height, Hash: hash, Parent: parent, Txs: txs, Time: time}
}

// twShiftOldChain is the main chain before the time-only reorg.
func twShiftOldChain() []Block {
	return []Block{
		twShiftBlock(1, "h1", "g", []string{"m", "x"}, intptr(0)),
		twShiftBlock(2, "h2", "h1", []string{"m", "m"}, intptr(105)),
		twShiftBlock(3, "h3", "h2", []string{"m", "z"}, intptr(110)),
		twShiftBlock(4, "h4", "h3", []string{"m", "q"}, nil),
		twShiftBlock(5, "h5", "h4", []string{"m", "m"}, intptr(108)),
	}
}

// twShiftBranch is the alternate branch handed to Reorg. It starts at height 2
// rooted at the retained h1 and keeps every hash, parent link and transaction
// list; only timestamps move (h2 105->120, h3 110->100, h4 nil->105,
// h5 108->102), while h1 at zero is left untouched.
func twShiftBranch() []Block {
	return []Block{
		twShiftBlock(2, "h2", "h1", []string{"m", "m"}, intptr(120)),
		twShiftBlock(3, "h3", "h2", []string{"m", "z"}, intptr(100)),
		twShiftBlock(4, "h4", "h3", []string{"m", "q"}, intptr(105)),
		twShiftBlock(5, "h5", "h4", []string{"m", "m"}, intptr(102)),
	}
}

// twShiftNewChain is the complete main chain after the reorg: retained h1 plus
// the re-timed branch.
func twShiftNewChain() []Block {
	chain := append([]Block{}, twShiftOldChain()[:1]...)
	return append(chain, twShiftBranch()...)
}

func buildTwShiftOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range twShiftOldChain() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

func twShiftQuery() TxQuery {
	return TxQuery{
		TxIDs:     []string{"m"},
		TimeStart: intptr(twShiftWindowStart),
		TimeEnd:   intptr(twShiftWindowEnd),
		PageSize:  twShiftPageSize,
	}
}

// twReferenceHits is an independent oracle: it walks an explicit chain by
// increasing height and in-block position, keeping an occurrence only when its
// identifier passes the set filter AND its block timestamp lies in the
// half-open window. Ordering is derived from height/position alone, never from
// the timestamps, which are non-monotonic in this fixture. It does not use the
// Index under test, so a shared QueryTxs bug cannot make an expectation match
// itself.
func twReferenceHits(chain []Block, txIDs []string, start, end int64) []TxHit {
	want := map[string]bool{}
	for _, id := range txIDs {
		want[id] = true
	}
	inWindow := func(when *int64) bool {
		if when == nil {
			return false // a missing timestamp never matches
		}
		return *when >= start && *when < end // start included, end excluded
	}
	hits := []TxHit{}
	for _, block := range chain {
		if !inWindow(block.Time) {
			continue
		}
		for position, tx := range block.Txs {
			if want[tx] {
				hits = append(hits, TxHit{
					Height:    block.Height,
					BlockHash: block.Hash,
					TxID:      tx,
					Position:  position,
				})
			}
		}
	}
	return hits
}

func twReferenceMatchedBlocks(chain []Block, txIDs []string, start, end int64) int64 {
	want := map[string]bool{}
	for _, id := range txIDs {
		want[id] = true
	}
	var blocks int64
	for _, block := range chain {
		if block.Time == nil || *block.Time < start || *block.Time >= end {
			continue
		}
		for _, tx := range block.Txs {
			if want[tx] {
				blocks++ // a block counts once however many matches it holds
				break
			}
		}
	}
	return blocks
}

// twReferencePage slices one page at the given offset of the oracle's complete
// filtered occurrence list and attaches the statistics over the WHOLE pinned
// range. The cursor stays opaque; tests validate it by following it.
func twReferencePage(chain []Block, pageSize int, offset int64) TxPage {
	full := twReferenceHits(chain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd)
	rest := full[offset:]
	n := pageSize
	if n > len(rest) {
		n = len(rest)
	}
	return TxPage{
		Hits:          append([]TxHit{}, rest[:n]...),
		TotalMatches:  int64(len(full)),
		MatchedBlocks: twReferenceMatchedBlocks(chain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd),
		ToHeight:      chain[len(chain)-1].Height,
	}
}

// Hand-derived first pages; the guarantee rests on these literal records and
// counts, not on oracle-vs-DeepEqual alone.
func literalTwShiftOldFirstPage() TxPage {
	return TxPage{
		Hits: []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "m", Position: 0},
			{Height: 2, BlockHash: "h2", TxID: "m", Position: 0},
		},
		TotalMatches:  5,
		MatchedBlocks: 3,
		ToHeight:      5,
	}
}

func literalTwShiftNewFirstPage() TxPage {
	return TxPage{
		Hits: []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "m", Position: 0},
			{Height: 3, BlockHash: "h3", TxID: "m", Position: 0},
		},
		TotalMatches:  5,
		MatchedBlocks: 4,
		ToHeight:      5,
	}
}

// The structural invariant the whole regression rests on: above the retained
// h1, the reorg changes timestamps only. Every height, hash, parent link and
// ordered transaction list is identical on the two chains, the tip stays 5,
// and at least one block actually moves across each window boundary.
func TestReorgTimeWindowFixtureKeepsStructureMovesTimesOnly(t *testing.T) {
	oldChain, newChain := twShiftOldChain(), twShiftNewChain()
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
	// Per-height transition table, including the unchanged real-zero h1 and the
	// missing->known h4.
	flips := map[int64]struct {
		oldIn, newIn bool
		oldTime      *int64
		newTime      *int64
	}{
		1: {true, true, intptr(0), intptr(0)},
		2: {true, false, intptr(105), intptr(120)},
		3: {false, true, intptr(110), intptr(100)},
		4: {false, true, nil, intptr(105)},
		5: {true, true, intptr(108), intptr(102)},
	}
	movedOut, movedIn := 0, 0
	for height, want := range flips {
		o, n := oldChain[height-1].Time, newChain[height-1].Time
		if !sameIntptr(o, want.oldTime) || !sameIntptr(n, want.newTime) {
			t.Fatalf("height %d times old=%v new=%v, want %v/%v", height, o, n, want.oldTime, want.newTime)
		}
		gotOld := timeWindow{enabled: true, start: twShiftWindowStart, end: twShiftWindowEnd}.contains(o)
		gotNew := timeWindow{enabled: true, start: twShiftWindowStart, end: twShiftWindowEnd}.contains(n)
		if gotOld != want.oldIn || gotNew != want.newIn {
			t.Fatalf("height %d window membership old=%v(want %v) new=%v(want %v)",
				height, gotOld, want.oldIn, gotNew, want.newIn)
		}
		if want.oldIn && !want.newIn {
			movedOut++
		}
		if !want.oldIn && want.newIn {
			movedIn++
		}
	}
	if movedOut != 1 || movedIn != 2 {
		t.Fatalf("boundary crossings: movedOut=%d movedIn=%d, want 1/2", movedOut, movedIn)
	}

	// A live Reorg of this shape must report exactly the re-timed heights
	// (h1's time is unchanged and is not reported), keep the tip at 5, and
	// leave every hash/parent/tx list in place.
	index := buildTwShiftOldChain(t)
	dropped, err := index.Reorg(twShiftBranch())
	if err != nil {
		t.Fatalf("time-only reorg refused: %v", err)
	}
	if want := []int64{2, 3, 4, 5}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped=%v, want the four re-timed heights %v", dropped, want)
	}
	if index.Tip != 5 {
		t.Fatalf("tip=%d, want unchanged 5", index.Tip)
	}
	for _, want := range newChain {
		got, ok := index.Blocks[want.Height]
		if !ok {
			t.Fatalf("height %d missing after reorg", want.Height)
		}
		if !equalBlockContent(got, want) {
			t.Fatalf("height %d stored block=%+v, want %+v", want.Height, got, want)
		}
		if index.ByHash[want.Hash] != want.Height {
			t.Fatalf("hash %q resolves to %d, want %d", want.Hash, index.ByHash[want.Hash], want.Height)
		}
	}
}

func sameIntptr(a, b *int64) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// The counting, ordering and window semantics the regression rests on,
// asserted on both chains and cross-checked against the live index before and
// after the time-only reorg.
func TestReorgTimeWindowQueryFixtureSemanticsOnBothChains(t *testing.T) {
	oldChain, newChain := twShiftOldChain(), twShiftNewChain()

	oldHits := twReferenceHits(oldChain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd)
	newHits := twReferenceHits(newChain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd)
	wantOld := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "m", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "m", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "m", Position: 1},
		{Height: 5, BlockHash: "h5", TxID: "m", Position: 0},
		{Height: 5, BlockHash: "h5", TxID: "m", Position: 1},
	}
	wantNew := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "m", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "m", Position: 0},
		{Height: 4, BlockHash: "h4", TxID: "m", Position: 0},
		{Height: 5, BlockHash: "h5", TxID: "m", Position: 0},
		{Height: 5, BlockHash: "h5", TxID: "m", Position: 1},
	}
	if !reflect.DeepEqual(oldHits, wantOld) {
		t.Fatalf("old oracle hits=%v, want %v", oldHits, wantOld)
	}
	if !reflect.DeepEqual(newHits, wantNew) {
		t.Fatalf("new oracle hits=%v, want %v", newHits, wantNew)
	}
	// Both chains hold five occurrences, but over a different number of blocks:
	// this count divergence is what a same-total torn page cannot paper over.
	if twReferenceMatchedBlocks(oldChain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd) != 3 {
		t.Fatal("old chain must match 3 blocks (h1, h2, h5)")
	}
	if twReferenceMatchedBlocks(newChain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd) != 4 {
		t.Fatal("new chain must match 4 blocks (h1, h3, h4, h5)")
	}
	// Repeated identifiers inside one block keep their own positions and count
	// separately; the non-matching companions x, z, q never leak through.
	for _, gone := range []TxHit{
		{Height: 2, BlockHash: "h2", TxID: "m", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "m", Position: 1},
	} {
		if containsHit(newHits, gone) {
			t.Fatalf("old-time occurrence %+v must leave the window", gone)
		}
	}
	for _, entered := range []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "m", Position: 0},
		{Height: 4, BlockHash: "h4", TxID: "m", Position: 0},
	} {
		if !containsHit(newHits, entered) {
			t.Fatalf("new-time occurrence %+v must enter the window: %v", entered, newHits)
		}
	}
	// Timestamps are not monotonic with height (h3 at 110 sits below h2 at
	// 105/120, h5 dips to 102 after h4's 105), yet both answers stay in
	// height/position order — never timestamp order.
	requireOrdered(t, oldHits)
	requireOrdered(t, newHits)

	// The oracle must match the live index on both complete chain states.
	index := buildTwShiftOldChain(t)
	live, err := index.QueryTxs(twShiftQuery())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(live), viewOf(twReferencePage(oldChain, twShiftPageSize, 0))) {
		t.Fatalf("live old-chain page=%+v", live)
	}
	if !reflect.DeepEqual(live.Hits, literalTwShiftOldFirstPage().Hits) {
		t.Fatalf("live old first page hits=%v, want literal %v", live.Hits, literalTwShiftOldFirstPage().Hits)
	}

	if _, err := index.Reorg(twShiftBranch()); err != nil {
		t.Fatalf("time-only reorg refused: %v", err)
	}
	live, err = index.QueryTxs(twShiftQuery())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(live), viewOf(twReferencePage(newChain, twShiftPageSize, 0))) {
		t.Fatalf("live new-chain page=%+v", live)
	}
	if !reflect.DeepEqual(live.Hits, literalTwShiftNewFirstPage().Hits) {
		t.Fatalf("live new first page hits=%v, want literal %v", live.Hits, literalTwShiftNewFirstPage().Hits)
	}

	// On the OLD chain, starting the window exactly at zero still rejects the
	// missing timestamp: widening the end to 200 admits every block carrying a
	// value (including the real-zero h1 and the 110 h3) but never the nil h4.
	oldIndex := buildTwShiftOldChain(t)
	wide, err := oldIndex.QueryTxs(TxQuery{
		TxIDs: []string{"m"}, TimeStart: intptr(0), TimeEnd: intptr(200),
	})
	if err != nil {
		t.Fatal(err)
	}
	wideHeights := map[int64]bool{}
	for _, h := range wide.Hits {
		wideHeights[h.Height] = true
	}
	if !reflect.DeepEqual(wideHeights, map[int64]bool{1: true, 2: true, 3: true, 5: true}) {
		t.Fatalf("old chain [0,200) matched heights %v, want every valued block 1,2,3,5 and not the nil h4", wideHeights)
	}
	if wide.TotalMatches != 6 || wide.MatchedBlocks != 4 {
		t.Fatalf("old chain [0,200) stats=%+v, want 6 occurrences over 4 blocks", wide)
	}

	// The half-open boundary and the real-zero/missing distinction are pinned
	// on the new chain as well: the included zero keeps h1, a positive start
	// drops it, h3 at 100 is included by [100,101), and h4's 105 is the
	// excluded end of [0,105).
	for _, tc := range []struct {
		name        string
		start, end  int64
		wantHeights []int64
	}{
		{"fixture window [0,110)", 0, 110, []int64{1, 3, 4, 5}},
		{"positive start drops real zero", 1, 110, []int64{3, 4, 5}},
		{"end excluded at 105", 0, 105, []int64{1, 3, 5}},
		{"only the included 100", 100, 101, []int64{3}},
		{"window around zero keeps just h1", 0, 1, []int64{1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page, err := index.QueryTxs(TxQuery{
				TxIDs: []string{"m"}, TimeStart: intptr(tc.start), TimeEnd: intptr(tc.end),
			})
			if err != nil {
				t.Fatal(err)
			}
			gotHeights := map[int64]bool{}
			for _, h := range page.Hits {
				gotHeights[h.Height] = true
			}
			want := map[int64]bool{}
			for _, h := range tc.wantHeights {
				want[h] = true
			}
			if !reflect.DeepEqual(gotHeights, want) {
				t.Fatalf("window [%d,%d) matched heights %v, want %v; hits=%v",
					tc.start, tc.end, gotHeights, want, page.Hits)
			}
		})
	}
}

// classifyTimeWindowTxPage accepts got only when its visible contents are the
// first-page view of exactly one chain state under the pinned window. The
// opaque cursor is excluded and validated separately by following it. Records
// are provenanced against the COMPLETE filtered occurrence lists, so a page
// that stitches old-time hits to new-time statistics is rejected as MIXED even
// though both chains report TotalMatches 5 and the same upper height 5.
func classifyTimeWindowTxPage(got, oldRef, newRef TxPage, oldAllHits, newAllHits []TxHit) error {
	type view struct {
		Hits          []TxHit
		TotalMatches  int64
		MatchedBlocks int64
		ToHeight      int64
	}
	of := func(p TxPage) view {
		return view{Hits: p.Hits, TotalMatches: p.TotalMatches, MatchedBlocks: p.MatchedBlocks, ToHeight: p.ToHeight}
	}
	g, o, n := of(got), of(oldRef), of(newRef)
	if reflect.DeepEqual(g, o) || reflect.DeepEqual(g, n) {
		return nil
	}

	var report strings.Builder
	fmt.Fprintln(&report, "time-windowed QueryTxs page does not match one complete main chain:")
	tags := map[string]bool{}
	tag := func(name string, gotV, oldV, newV any) {
		matchesOld := reflect.DeepEqual(gotV, oldV)
		matchesNew := reflect.DeepEqual(gotV, newV)
		label := "neither"
		switch {
		case matchesOld && matchesNew:
			label = "both"
		case matchesOld:
			label = "OLD"
			tags["old"] = true
		case matchesNew:
			label = "NEW"
			tags["new"] = true
		}
		fmt.Fprintf(&report, "  %-15s got=%v old=%v new=%v -> %s\n", name, gotV, oldV, newV, label)
	}
	tag("ToHeight", g.ToHeight, o.ToHeight, n.ToHeight)
	tag("TotalMatches", g.TotalMatches, o.TotalMatches, n.TotalMatches)
	tag("MatchedBlocks", g.MatchedBlocks, o.MatchedBlocks, n.MatchedBlocks)

	oldByKey, newByKey := map[TxHit]bool{}, map[TxHit]bool{}
	for _, h := range oldAllHits {
		oldByKey[h] = true
	}
	for _, h := range newAllHits {
		newByKey[h] = true
	}
	var inOld, inNew, inNeither, onlyOld, onlyNew int
	ordered := true
	for i, h := range g.Hits {
		co, cn := oldByKey[h], newByKey[h]
		if co {
			inOld++
		}
		if cn {
			inNew++
		}
		if co && !cn {
			onlyOld++
		}
		if cn && !co {
			onlyNew++
		}
		if !co && !cn {
			inNeither++
		}
		if i > 0 {
			prev := g.Hits[i-1]
			if h.Height < prev.Height || (h.Height == prev.Height && h.Position <= prev.Position) {
				ordered = false
			}
		}
		if h.Height > g.ToHeight {
			fmt.Fprintf(&report, "  hit above pinned ToHeight %d: %+v\n", g.ToHeight, h)
		}
	}
	fmt.Fprintf(&report, "  Hits provenance  on-old=%d on-new=%d only-old=%d only-new=%d on-neither=%d (ordered=%v)\n",
		inOld, inNew, onlyOld, onlyNew, inNeither, ordered)
	if onlyOld > 0 {
		tags["old"] = true
	}
	if onlyNew > 0 {
		tags["new"] = true
	}
	switch {
	case tags["old"] && tags["new"]:
		fmt.Fprintf(&report, "  => MIXED: records and statistics come from different timestamp states; one page must reflect a single main chain")
	case inNeither > 0:
		fmt.Fprintf(&report, "  => CORRUPT: records exist under neither timestamp state")
	case !ordered:
		fmt.Fprintf(&report, "  => CORRUPT: page is not in height/position order")
	default:
		fmt.Fprintf(&report, "  => CORRUPT: matches neither the old nor the new timestamp state")
	}
	return errors.New(report.String())
}

// The classifier must accept both complete answers and reject the
// equal-total mixtures this reorg shape is prone to.
func TestClassifyTimeWindowTxPageRejectsStitchedPages(t *testing.T) {
	oldChain, newChain := twShiftOldChain(), twShiftNewChain()
	oldRef := twReferencePage(oldChain, twShiftPageSize, 0)
	newRef := twReferencePage(newChain, twShiftPageSize, 0)
	oldAll := twReferenceHits(oldChain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd)
	newAll := twReferenceHits(newChain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd)
	classify := func(page TxPage) error { return classifyTimeWindowTxPage(page, oldRef, newRef, oldAll, newAll) }
	if err := classify(oldRef); err != nil {
		t.Fatalf("pure old page rejected: %v", err)
	}
	if err := classify(newRef); err != nil {
		t.Fatalf("pure new page rejected: %v", err)
	}

	// Mixture 1: old-time records, but new-time MatchedBlocks. TotalMatches is
	// 5 on both chains, so a total-only consistency check would accept this.
	mixed1 := oldRef
	mixed1.MatchedBlocks = newRef.MatchedBlocks
	if err := classify(mixed1); err == nil {
		t.Fatal("old hits with new-time block count were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("mixed1 not reported as MIXED:\n%v", err)
	}

	// Mixture 2: the exact torn answer the task names — records read under the
	// old times, statistics counted under the new times — built by hand rather
	// than borrowed from either reference page.
	mixed2 := TxPage{
		Hits: []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "m", Position: 0}, // shared
			{Height: 2, BlockHash: "h2", TxID: "m", Position: 0}, // OLD only
		},
		TotalMatches:  5,
		MatchedBlocks: 4, // NEW
		ToHeight:      5,
	}
	if err := classify(mixed2); err == nil {
		t.Fatal("stitched old-records/new-stats page was accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("mixed2 not reported as MIXED:\n%v", err)
	}

	// Mixture 3: new-time records carrying the old block count.
	mixed3 := newRef
	mixed3.MatchedBlocks = oldRef.MatchedBlocks
	if err := classify(mixed3); err == nil {
		t.Fatal("new hits with old-time block count were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("mixed3 not reported as MIXED:\n%v", err)
	}

	// Corruption: a matched-block count that belongs to neither state.
	corrupt := newRef
	corrupt.MatchedBlocks = 99
	if err := classify(corrupt); err == nil {
		t.Fatal("corrupt stats matching neither state were accepted")
	}
}

// Ordering 1: the windowed first page is holding the lock on the complete OLD
// timestamp state (tip already pinned) when the time-only reorg starts and
// blocks on the lock. The page must finish entirely against the old times —
// literal h1/h2 records, 5 occurrences over 3 blocks, upper height 5 and a
// cursor pinned to the old fingerprint — and must not fail a valid first page.
// After the reorg commits, that cursor must die with a zeroed
// ErrQueryChanged: the records the new times filter in may not be appended
// after the old page.
func TestQueryTxsTimeWindowFirstPageInFlightObservesCompleteOldTimes(t *testing.T) {
	index := buildTwShiftOldChain(t)
	h := installQueryReorgHooks(t, true, false, 5)
	oldChain, newChain := twShiftOldChain(), twShiftNewChain()

	result := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, err := index.QueryTxs(twShiftQuery())
		if err != nil {
			errCh <- err
			return
		}
		result <- page
	}()

	<-h.firstParked // first page holds index.mu and has pinned the old state
	reorgDone := make(chan struct{})
	go func() {
		if _, err := index.Reorg(twShiftBranch()); err != nil {
			errCh <- err
		}
		close(reorgDone)
	}()

	close(h.releaseFirst) // the page scans and finishes against the old times
	var got TxPage
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("concurrent first page failed: %v", err)
	}
	want := twReferencePage(oldChain, twShiftPageSize, 0)
	if err := classifyTimeWindowTxPage(got, want, twReferencePage(newChain, twShiftPageSize, 0),
		twReferenceHits(oldChain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd),
		twReferenceHits(newChain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd)); err != nil {
		t.Fatalf("in-flight first page did not return one complete chain state:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Fatalf("page holding the lock across reorg start must see the old times:\n%+v", got)
	}
	if !reflect.DeepEqual(got.Hits, literalTwShiftOldFirstPage().Hits) {
		t.Fatalf("old first page hits=%v, want literal h1/h2 records", got.Hits)
	}
	if got.NextCursor == "" {
		t.Fatal("old first page over 5 matches (page size 2) must carry a cursor")
	}

	close(h.releaseReorg) // let the time-only reorg commit and return
	<-reorgDone
	if index.Tip != 5 {
		t.Fatalf("tip after time-only reorg=%d, want 5", index.Tip)
	}

	// The cursor pins the OLD timestamp state even though the height span,
	// hashes, parent links and transaction lists are all unchanged: only the
	// timestamp bytes in the fingerprint differ.
	oldFingerprint := buildTwShiftOldChain(t)
	payload := decodePayload(t, index, got.NextCursor)
	if payload.To != 5 || payload.Off != 2 {
		t.Fatalf("cursor pins to=%d off=%d, want 5/2", payload.To, payload.Off)
	}
	if payload.window() != (timeWindow{enabled: true, start: twShiftWindowStart, end: twShiftWindowEnd}) {
		t.Fatalf("cursor window=%+v, want the pinned [0,110)", payload.window())
	}
	if payload.FP != fmt.Sprintf("%x", oldFingerprint.hashRangeBlockContent(1, 5)) {
		t.Fatal("cursor fingerprint does not match the complete OLD timestamp state")
	}

	// Continuing the old cursor on the new state must fail closed: no hits, no
	// replacement cursor, no usable statistics — the h3/h4 records the new
	// times filter in must never be stitched behind the h1/h2 old page.
	page, err := index.QueryTxs(func() TxQuery {
		q := twShiftQuery()
		q.Cursor = got.NextCursor
		return q
	}())
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("old cursor across time-only reorg: err=%v page=%+v, want ErrQueryChanged", err, page)
	}
	if errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a committed reorg must not be reported as an argument error: %v", err)
	}
	if !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("changed continuation must return a fully zeroed page, got %+v", page)
	}
}

// Ordering 2: the time-only reorg has fully committed (new timestamps live,
// Reorg still holding the lock) when the first page starts. It waits, then
// answers entirely against the NEW times — literal h1/h3 records, 5
// occurrences over 4 blocks, upper height 5 — and its cursor must drain the
// new state to the end with no duplicate or missing occurrence, identical
// statistics on every page, and no cursor once the last match is returned.
func TestQueryTxsTimeWindowFirstPageDuringCommittedReorgObservesCompleteNewTimes(t *testing.T) {
	index := buildTwShiftOldChain(t)
	h := installQueryReorgHooks(t, true, false, 5)
	oldChain, newChain := twShiftOldChain(), twShiftNewChain()

	reorgErr := make(chan error, 1)
	go func() {
		_, err := index.Reorg(twShiftBranch())
		reorgErr <- err
	}()
	<-h.reorgParked // new timestamps applied (tip still 5), Reorg holds the lock

	result := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, err := index.QueryTxs(twShiftQuery())
		if err != nil {
			errCh <- err
			return
		}
		result <- page
	}()

	close(h.releaseReorg) // Reorg returns; the first lock the page can get is the new state
	if err := <-reorgErr; err != nil {
		t.Fatalf("time-only reorg failed: %v", err)
	}
	close(h.releaseFirst) // the parked first page can now scan and finish

	var got TxPage
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("first page during committed reorg must not fail: %v", err)
	}
	want := twReferencePage(newChain, twShiftPageSize, 0)
	if err := classifyTimeWindowTxPage(got, twReferencePage(oldChain, twShiftPageSize, 0), want,
		twReferenceHits(oldChain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd),
		twReferenceHits(newChain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd)); err != nil {
		t.Fatalf("first page across reorg commit did not return one complete chain state:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Fatalf("page after reorg commit must see the complete new times:\n%+v", got)
	}
	if !reflect.DeepEqual(got.Hits, literalTwShiftNewFirstPage().Hits) {
		t.Fatalf("new first page hits=%v, want literal h1/h3 records", got.Hits)
	}
	if got.NextCursor == "" {
		t.Fatal("new first page over 5 matches (page size 2) must carry a cursor")
	}
	for _, hit := range got.Hits {
		if hit.Height == 2 {
			t.Fatalf("old-time h2 occurrence must not appear under the new times: %+v", hit)
		}
	}

	// The cursor pins the NEW timestamp state.
	newFingerprint := New()
	for _, b := range newChain {
		if err := newFingerprint.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	payload := decodePayload(t, index, got.NextCursor)
	if payload.To != 5 || payload.Off != 2 {
		t.Fatalf("cursor pins to=%d off=%d, want 5/2", payload.To, payload.Off)
	}
	if payload.FP != fmt.Sprintf("%x", newFingerprint.hashRangeBlockContent(1, 5)) {
		t.Fatal("cursor fingerprint does not match the complete NEW timestamp state")
	}

	// Drain the cursor while the chain stays put: every match of the new state
	// exactly once, statistics fixed at 5/4/upper-5 on every page, and the
	// cursor chain ends after the fifth occurrence.
	query := twShiftQuery()
	query.Cursor = got.NextCursor
	all := append([]TxHit{}, got.Hits...)
	pageCount := 1
	for {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("new-state continuation failed at page %d: %v", pageCount+1, err)
		}
		if page.TotalMatches != 5 || page.MatchedBlocks != 4 || page.ToHeight != 5 {
			t.Fatalf("page %d statistics drifted from the pinned first page: %+v", pageCount+1, page)
		}
		all = append(all, page.Hits...)
		pageCount++
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	wantAll := twReferenceHits(newChain, []string{"m"}, twShiftWindowStart, twShiftWindowEnd)
	if !reflect.DeepEqual(all, wantAll) {
		t.Fatalf("new-state pagination duplicated or lost occurrences:\n got=%v\nwant=%v", all, wantAll)
	}
	if pageCount != 3 {
		t.Fatalf("pages=%d, want 3 (2/2/1 over five hits)", pageCount)
	}
}

// A cursor minted against the old state must keep working only while the
// timestamp state is untouched; once any pinned height is re-timed — even a
// block the window excluded (h3 at 110) or one with a missing timestamp (h4) —
// the continuation fails closed with a zeroed page. Repeating the exact old
// branch is a no-op-style reorg that leaves the cursor valid.
func TestQueryTxsTimeWindowCursorDiesOnAnyPinnedTimestampMove(t *testing.T) {
	sameAsOld := twShiftOldChain()[1:] // heights 2..5 verbatim, including times

	t.Run("re-timing an excluded-end block kills the cursor", func(t *testing.T) {
		index := buildTwShiftOldChain(t)
		first, err := index.QueryTxs(twShiftQuery())
		if err != nil {
			t.Fatal(err)
		}
		// Move only h3 (110 -> 105): the tip stays 5 and the other three
		// blocks are byte-identical, yet the range fingerprint changes.
		branch := []Block{
			twShiftBlock(2, "h2", "h1", []string{"m", "m"}, intptr(105)),
			twShiftBlock(3, "h3", "h2", []string{"m", "z"}, intptr(105)),
			twShiftBlock(4, "h4", "h3", []string{"m", "q"}, nil),
			twShiftBlock(5, "h5", "h4", []string{"m", "m"}, intptr(108)),
		}
		if _, err := index.Reorg(branch); err != nil {
			t.Fatal(err)
		}
		if index.Tip != 5 {
			t.Fatalf("tip=%d, want the unchanged tip 5", index.Tip)
		}
		query := twShiftQuery()
		query.Cursor = first.NextCursor
		page, err := index.QueryTxs(query)
		requireChangedZeroed(t, page, err)
	})

	t.Run("giving a missing timestamp a value kills the cursor", func(t *testing.T) {
		index := buildTwShiftOldChain(t)
		first, err := index.QueryTxs(twShiftQuery())
		if err != nil {
			t.Fatal(err)
		}
		// Only h4 changes: nil -> 105 (it additionally enters the window).
		branch := []Block{
			twShiftBlock(2, "h2", "h1", []string{"m", "m"}, intptr(105)),
			twShiftBlock(3, "h3", "h2", []string{"m", "z"}, intptr(110)),
			twShiftBlock(4, "h4", "h3", []string{"m", "q"}, intptr(105)),
			twShiftBlock(5, "h5", "h4", []string{"m", "m"}, intptr(108)),
		}
		if _, err := index.Reorg(branch); err != nil {
			t.Fatal(err)
		}
		query := twShiftQuery()
		query.Cursor = first.NextCursor
		page, err := index.QueryTxs(query)
		requireChangedZeroed(t, page, err)
	})

	t.Run("identical branch keeps the cursor alive", func(t *testing.T) {
		index := buildTwShiftOldChain(t)
		pages := collectPages(t, index, twShiftQuery())
		cursor := pages[0].NextCursor
		dropped, err := index.Reorg(sameAsOld)
		if err != nil {
			t.Fatal(err)
		}
		if len(dropped) != 0 {
			t.Fatalf("re-submitting the identical branch dropped=%v, want none", dropped)
		}
		query := twShiftQuery()
		query.Cursor = cursor
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("cursor must survive an identical-branch reorg: %v", err)
		}
		if page.TotalMatches != 5 || page.MatchedBlocks != 3 || page.ToHeight != 5 {
			t.Fatalf("statistics drifted: %+v", page)
		}
	})
}

// Timing-independent overlap: reorgers alternate between the old and the new
// timestamp states while windowed readers paginate continuously, with no
// rendezvous hooks. The fixed tip and equal totals make this the dangerous
// case for a torn answer; every first page must classify as exactly one
// timestamp state, and every continuation must either keep draining that exact
// state or fail with a zeroed ErrQueryChanged.
func TestQueryTxsRepeatedTimeOnlyReorgsStayConsistent(t *testing.T) {
	index := buildTwShiftOldChain(t)
	oldAll := twReferenceHits(twShiftOldChain(), []string{"m"}, twShiftWindowStart, twShiftWindowEnd)
	newAll := twReferenceHits(twShiftNewChain(), []string{"m"}, twShiftWindowStart, twShiftWindowEnd)
	oldBranch := twShiftOldChain()[1:]
	newBranch := twShiftBranch()

	const pageSize = twShiftPageSize
	const reorgers = 2
	const rounds = 40
	const readers = 4
	const runs = 60

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < reorgers; r++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				select {
				case <-stop:
					return
				default:
				}
				branch := newBranch
				if (i+seed)%2 == 0 {
					branch = oldBranch
				}
				if _, err := index.Reorg(branch); err != nil {
					t.Errorf("time-only reorg failed: %v", err)
					return
				}
			}
		}(r)
	}

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for run := 0; run < runs; run++ {
				select {
				case <-stop:
					return
				default:
				}
				first, err := index.QueryTxs(twShiftQuery())
				if err != nil {
					t.Errorf("first page failed: %v", err)
					return
				}
				// Tip 5 and TotalMatches 5 on both states: only the records and
				// MatchedBlocks distinguish them, which is exactly why the
				// full classifier (not a count check) must be used.
				var stateHits []TxHit
				switch first.MatchedBlocks {
				case 3:
					stateHits = oldAll
				case 4:
					stateHits = newAll
				default:
					t.Errorf("first page matched-blocks=%d, want 3 (old) or 4 (new)", first.MatchedBlocks)
					return
				}
				if first.ToHeight != 5 || first.TotalMatches != 5 {
					t.Errorf("first page stats=%+v, want total 5 upper 5", first)
					return
				}
				if err := classifyTimeWindowTxPage(first,
					twReferencePage(twShiftOldChain(), pageSize, 0),
					twReferencePage(twShiftNewChain(), pageSize, 0),
					oldAll, newAll); err != nil {
					t.Errorf("%v", err)
					return
				}
				seen := append([]TxHit{}, first.Hits...)
				if !reflect.DeepEqual(first.Hits, stateHits[:len(first.Hits)]) {
					t.Errorf("first page records do not match its timestamp state:\n%+v", first.Hits)
					return
				}
				query := twShiftQuery()
				query.Cursor = first.NextCursor
				for query.Cursor != "" {
					page, err := index.QueryTxs(query)
					if errors.Is(err, ErrQueryChanged) {
						if !reflect.DeepEqual(page, TxPage{}) {
							t.Errorf("changed continuation leaked %+v", page)
						}
						break // the pinned timestamp state moved; restart fresh
					}
					if err != nil {
						t.Errorf("continuation failed: %v", err)
						return
					}
					if page.TotalMatches != first.TotalMatches || page.ToHeight != first.ToHeight {
						t.Errorf("statistics drifted mid-pagination: %+v", page)
						return
					}
					if page.MatchedBlocks != first.MatchedBlocks {
						t.Errorf("matched-block count changed from %d to %d mid-pagination",
							first.MatchedBlocks, page.MatchedBlocks)
						return
					}
					wantSlice := stateHits[len(seen):]
					if len(wantSlice) > pageSize {
						wantSlice = wantSlice[:pageSize]
					}
					if !reflect.DeepEqual(page.Hits, wantSlice) {
						t.Errorf("continuation records do not match the pinned state:\n%+v", page.Hits)
						return
					}
					seen = append(seen, page.Hits...)
					query.Cursor = page.NextCursor
					if query.Cursor == "" && len(seen) != len(stateHits) {
						t.Errorf("cursor chain ended after %d hits, state has %d", len(seen), len(stateHits))
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(stop)

	// Leave the index deterministically on the new timestamp state and read it
	// to the end against the oracle.
	if _, err := index.Reorg(newBranch); err != nil {
		t.Fatalf("final reorg failed: %v", err)
	}
	pages := collectPages(t, index, twShiftQuery())
	var all []TxHit
	for _, page := range pages {
		if page.TotalMatches != 5 || page.MatchedBlocks != 4 || page.ToHeight != 5 {
			t.Fatalf("final pagination stats drifted: %+v", page)
		}
		all = append(all, page.Hits...)
	}
	if !reflect.DeepEqual(all, newAll) {
		t.Fatalf("final new-state pagination=%v, want %v", all, newAll)
	}
}
