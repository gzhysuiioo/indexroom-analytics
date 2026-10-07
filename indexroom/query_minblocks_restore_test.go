package indexroom

import (
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is the automated regression for the MinBlocks qualification of a
// QueryTxs first page (empty cursor) after a snapshot Restore has replaced the
// WHOLE main chain — in particular with a SHORTER chain whose block contents
// differ. The qualification decision must be made entirely from the restored
// blocks: an identifier that only reached the threshold through heights the
// restore deletes must vanish from the new result completely, while an
// identifier that reaches it on the new chain must return every one of its
// matching occurrences inside the queried range, with its original in-block
// positions (never renumbered, duplicates never merged). When the range's
// upper bound is the current tip, ToHeight follows the restored tip and is
// never shrunk just because an identifier is filtered out.
//
// The windowed fixture (version 2, real zero vs missing time, half-open
// [start, end)) also pins that only blocks passing BOTH the TxIDs filter and
// the timestamp window help an identifier qualify: out-of-window blocks and
// blocks without a timestamp contribute nothing, while a real zero at the
// included window start is still a real time.
//
// The MinBlocks query (window [0,100), TxIDs {old,new,solo}, MinBlocks 2,
// page size 2, To left at zero so the first page pins the observed tip):
//
//	OLD main chain already ingested, tip 6:
//	  h1 parent g   [old, old]      time 0     in window: real zero at the included start
//	  h2 parent h1  [old]           time 50    in window
//	  h3 parent h2  [new]           time 150   OUT of window: never helps "new" qualify
//	  h4 parent h3  [new, z]        no time    missing: never helps "new" (the nil trap)
//	  h5 parent h4  [new, solo]     time 80    in window; "solo" here is on a DELETED height
//	  h6 parent h5  [zz]           time 90    in window but neither id matches the TxIDs set
//	Version-2 snapshot replacing it, tip 4 (shorter; content differs everywhere):
//	  j1 parent g   [new]           time 0     in window: real zero, j1#0
//	  j2 parent j1  [new, new]      time 50    in window: duplicate in one block, j2#0,j2#1
//	  j3 parent j2  [old]           time 150   OUT of window: old cannot qualify via it
//	  j4 parent j3  [new, zz, solo, solo] no time  missing: j4#0 "new" must NOT be returned
//	                                        or help qualify; "solo" cannot qualify either
//
// Qualification (threshold 2, distinct IN-WINDOW blocks):
//
//	old chain: "old" in {h1, h2} -> QUALIFIED; "new" has only the single
//	  in-window block h5 (h3 is out of window and h4 has no time) -> not
//	  qualified; "solo" has the single in-window block h5 -> not qualified.
//	new chain: "new" in {j1, j2} -> QUALIFIED (the j2 duplicate counts one
//	  block; missing-time j4 contributes nothing even though it holds "new");
//	  "old" has only the out-of-window j3 -> not qualified; "solo" sits only in
//	  missing-time j4 -> not qualified.
//
// Both chains therefore carry a real-zero block that must count and a
// missing-time block holding a queried identifier that must not: treating a
// missing timestamp as zero would make "new" qualify on the old chain (h4 +
// h5) and return the extra j4#0 occurrence on the new chain — both
// observable. The real zero at the included window start is a real time on
// both chains.
//
// So restoring flips the qualified set: "old" qualified on the old chain
// through the in-window retained heights, but on the new chain its only
// occurrence is the out-of-window j3, so not one "old" occurrence survives;
// "solo" is the decisive deleted-height case — its single in-window block was
// h5, which the shorter snapshot deletes, and on the new chain it appears
// only inside j4 behind a block the TxIDs filter never matches, so it has no
// occurrence on either side of the result. Every old-chain height disappears
// (the snapshot replaces height 1 too, with a different hash), so the two
// complete answers share no record.
//
// Old answer (range 1..6): h1 old#0, h1 old#1, h2 old#0 = 3 occurrences over
// 2 matched blocks (h1, h2), ToHeight 6.
// New answer (range 1..4): j1 new#0, j2 new#0, j2 new#1 = 3 occurrences over
// 2 matched blocks (j1, j2), ToHeight 4.
//
// The totals happen to match (3/2), which is deliberate: records, hashes,
// heights, positions and the pinned ToHeight (6 vs 4) carry the chain
// identity, and the regression must not be able to pass by copying counts.
// "solo" additionally breaks the count symmetry for the qualifier-negative
// checks. A separate window-free fixture (mbRsPlain*) guards the no-window
// rule — a missing timestamp does not exclude a block — and the case of an
// identifier that qualifies on the OLD chain ONLY through a deleted height.

const (
	mbRsWindowStart = int64(0)
	mbRsWindowEnd   = int64(100)
	mbRsMinBlocks   = int64(2)
	mbRsPageSize    = 2
)

// mbRsBlock builds one fixture block.
func mbRsBlock(height int64, hash, parent string, txs []string, time *int64) Block {
	return Block{Height: height, Hash: hash, Parent: parent, Txs: txs, Time: time}
}

// mbRsOldChain is the main chain before the restore (tip 6).
func mbRsOldChain() []Block {
	return []Block{
		mbRsBlock(1, "h1", "g", []string{"old", "old"}, intptr(0)),
		mbRsBlock(2, "h2", "h1", []string{"old"}, intptr(50)),
		mbRsBlock(3, "h3", "h2", []string{"new"}, intptr(150)),
		mbRsBlock(4, "h4", "h3", []string{"new", "z"}, nil),
		mbRsBlock(5, "h5", "h4", []string{"new", "solo"}, intptr(80)),
		mbRsBlock(6, "h6", "h5", []string{"zz"}, intptr(90)),
	}
}

// mbRsNewChain is the shorter version-2 snapshot chain (tip 4).
func mbRsNewChain() []Block {
	return []Block{
		mbRsBlock(1, "j1", "g", []string{"new"}, intptr(0)),
		mbRsBlock(2, "j2", "j1", []string{"new", "new"}, intptr(50)),
		mbRsBlock(3, "j3", "j2", []string{"old"}, intptr(150)),
		mbRsBlock(4, "j4", "j3", []string{"new", "zz", "solo", "solo"}, nil),
	}
}

// mbRsNewSnapshot is the version-2 wire form of mbRsNewChain.
const mbRsNewSnapshot = `{"version":2,"tip":4,"blocks":[` +
	`{"height":1,"hash":"j1","parent":"g","txs":["new"],"timestamp":0},` +
	`{"height":2,"hash":"j2","parent":"j1","txs":["new","new"],"timestamp":50},` +
	`{"height":3,"hash":"j3","parent":"j2","txs":["old"],"timestamp":150},` +
	`{"height":4,"hash":"j4","parent":"j3","txs":["new","zz","solo","solo"],"timestamp":null}` +
	`]}`

// mbRsBrokenSnapshot is the same document with the LAST block's parent link
// wrong, so the invalidity is discovered only at the end of the document.
const mbRsBrokenSnapshot = `{"version":2,"tip":4,"blocks":[` +
	`{"height":1,"hash":"j1","parent":"g","txs":["new"],"timestamp":0},` +
	`{"height":2,"hash":"j2","parent":"j1","txs":["new","new"],"timestamp":50},` +
	`{"height":3,"hash":"j3","parent":"j2","txs":["old"],"timestamp":150},` +
	`{"height":4,"hash":"j4","parent":"j3-broken","txs":["new","zz","solo","solo"],"timestamp":null}` +
	`]}`

func buildMbRsOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range mbRsOldChain() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at height %d: %v", b.Height, err)
		}
	}
	return index
}

// ingestMbRsChain builds the snapshot chain by block-by-block Append instead
// of Restore, for the restore-vs-ingestion equivalence guarantee.
func ingestMbRsChain(t *testing.T, chain []Block) *Index {
	t.Helper()
	index := New()
	for _, b := range chain {
		if err := index.Append(b); err != nil {
			t.Fatalf("ingest append at height %d: %v", b.Height, err)
		}
	}
	return index
}

// mbRsQuery is the fixed first-page request: tip-bound To, the id set, the
// [0,100) window, threshold 2, page size 2.
func mbRsQuery() TxQuery {
	return TxQuery{
		TxIDs:     []string{"old", "new", "solo"},
		TimeStart: intptr(mbRsWindowStart),
		TimeEnd:   intptr(mbRsWindowEnd),
		MinBlocks: mbRsMinBlocks,
		PageSize:  mbRsPageSize,
	}
}

// mbRsOracle is an independent reference implementation of the MinBlocks +
// TxIDs + half-open time-window query over an in-memory chain. It shares no
// code with the Index under test: it decides qualification from distinct
// in-window blocks, then keeps every surviving occurrence in height/position
// order (ascending or descending), and reports the totals over the WHOLE
// range together with the tip-bound ToHeight. It is derived from the supplied
// chain, never from the Index.
type mbRsOracle struct {
	ids      map[string]struct{}
	window   timeWindow
	minBlock int64
}

func newMbRsOracle(query TxQuery) mbRsOracle {
	window, err := normalizeTimeWindow(query.TimeStart, query.TimeEnd)
	if err != nil {
		panic(err) // test fixtures only ever pass validated windows
	}
	o := mbRsOracle{window: window, minBlock: query.MinBlocks}
	if len(query.TxIDs) > 0 {
		o.ids = make(map[string]struct{}, len(query.TxIDs))
		for _, id := range query.TxIDs {
			o.ids[id] = struct{}{}
		}
	}
	return o
}

func (o mbRsOracle) matchesID(tx string) bool {
	if o.ids == nil {
		return true
	}
	_, ok := o.ids[tx]
	return ok
}

// qualified returns the identifiers reaching MinBlocks distinct blocks that
// pass every condition; nil means MinBlocks is disabled.
func (o mbRsOracle) qualified(chain []Block) map[string]struct{} {
	if o.minBlock <= 0 {
		return nil
	}
	counts := map[string]int64{}
	for _, block := range chain {
		if !o.window.contains(block.Time) {
			continue
		}
		seen := map[string]struct{}{}
		for _, tx := range block.Txs {
			if !o.matchesID(tx) {
				continue
			}
			if _, ok := seen[tx]; ok {
				continue
			}
			seen[tx] = struct{}{}
			counts[tx]++
		}
	}
	qualified := map[string]struct{}{}
	for id, n := range counts {
		if n >= o.minBlock {
			qualified[id] = struct{}{}
		}
	}
	return qualified
}

// answer returns every surviving occurrence (ordered per order), the totals
// over the whole range, and the tip-bound ToHeight for chain.
func (o mbRsOracle) answer(chain []Block, order TxOrder) (hits []TxHit, total, matchedBlocks, toHeight int64) {
	qualified := o.qualified(chain)
	hits = []TxHit{}
	heights := make([]int, 0, len(chain))
	for i := range chain {
		heights = append(heights, i)
	}
	if order == OrderDesc {
		sort.Sort(sort.Reverse(sort.IntSlice(heights)))
	}
	for _, i := range heights {
		block := chain[i]
		if !o.window.contains(block.Time) {
			continue
		}
		positions := make([]int, 0, len(block.Txs))
		for p := range block.Txs {
			positions = append(positions, p)
		}
		if order == OrderDesc {
			sort.Sort(sort.Reverse(sort.IntSlice(positions)))
		}
		matched := false
		for _, p := range positions {
			tx := block.Txs[p]
			if !o.matchesID(tx) {
				continue
			}
			if qualified != nil {
				if _, ok := qualified[tx]; !ok {
					continue
				}
			}
			hits = append(hits, TxHit{Height: block.Height, BlockHash: block.Hash, TxID: tx, Position: p})
			matched = true
		}
		if matched {
			matchedBlocks++
		}
	}
	return hits, int64(len(hits)), matchedBlocks, int64(len(chain))
}

// Hand-derived complete answers; the guarantee rests on these literal
// records and counts, not on the oracle alone.
func mbRsOldHits() []TxHit {
	return []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "old", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "old", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "old", Position: 0},
	}
}

func mbRsNewHits() []TxHit {
	return []TxHit{
		{Height: 1, BlockHash: "j1", TxID: "new", Position: 0},
		{Height: 2, BlockHash: "j2", TxID: "new", Position: 0},
		{Height: 2, BlockHash: "j2", TxID: "new", Position: 1},
	}
}

// The structural and counting invariants the regression rests on, asserted
// for both chains and cross-checked with the live index before and after the
// restore.
func TestMinBlocksRestoreFixtureSemantics(t *testing.T) {
	oldChain, newChain := mbRsOldChain(), mbRsNewChain()
	if len(oldChain) != 6 || len(newChain) != 4 {
		t.Fatalf("chain lengths old=%d new=%d, want 6 and 4 (a shorter chain)", len(oldChain), len(newChain))
	}

	// The snapshot must parse as intended, including real-zero vs missing
	// timestamps, and the broken document must be invalid only in its last
	// block's parent link.
	tip, blocks, err := parseSnapshot(strings.NewReader(mbRsNewSnapshot))
	if err != nil {
		t.Fatalf("valid snapshot must parse: %v", err)
	}
	if tip != 4 || len(blocks) != 4 {
		t.Fatalf("parsed snapshot tip=%d blocks=%d, want tip 4 with 4 blocks", tip, len(blocks))
	}
	if !reflect.DeepEqual(blocks, newChain) {
		t.Fatalf("parsed snapshot blocks=%v, want %v", blocks, newChain)
	}
	if _, _, err := parseSnapshot(strings.NewReader(mbRsBrokenSnapshot)); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("broken snapshot err=%v, want ErrInvalidSnapshot", err)
	}

	// The two chains share no block hash: Restore replaces height 1 as well,
	// so no record is explainable by both chains.
	for _, nb := range newChain {
		for _, ob := range oldChain {
			if nb.Hash == ob.Hash {
				t.Fatalf("fixture hash %q shared between the chains", nb.Hash)
			}
		}
	}

	window := timeWindow{enabled: true, start: mbRsWindowStart, end: mbRsWindowEnd}
	// Per-height window membership over the spread 0/50/150/missing/80/90 on
	// the old chain and 0/50/150/missing on the new one. The end-excluded
	// boundary itself is exercised separately with [0,50) vs [0,51).
	wantOldInWindow := map[int64]bool{1: true, 2: true, 3: false, 4: false, 5: true, 6: true}
	for height, wantIn := range wantOldInWindow {
		if got := window.contains(oldChain[height-1].Time); got != wantIn {
			t.Fatalf("old height %d window membership=%v, want %v (time=%v)",
				height, got, wantIn, oldChain[height-1].Time)
		}
	}
	wantNewInWindow := map[int64]bool{1: true, 2: true, 3: false, 4: false}
	for height, wantIn := range wantNewInWindow {
		if got := window.contains(newChain[height-1].Time); got != wantIn {
			t.Fatalf("new height %d window membership=%v, want %v (time=%v)",
				height, got, wantIn, newChain[height-1].Time)
		}
	}
	// A real zero is a real timestamp: h1/j1 at the included window start must
	// count; it must never be confused with a missing time.
	if oldChain[0].Time == nil || *oldChain[0].Time != 0 {
		t.Fatal("old h1 must carry a real zero timestamp")
	}
	if newChain[0].Time == nil || *newChain[0].Time != 0 {
		t.Fatal("new j1 must carry a real zero timestamp")
	}
	if newChain[0].Time != nil && !window.contains(newChain[0].Time) {
		t.Fatal("a real zero at the included window start must survive the window")
	}
	if oldChain[3].Time != nil {
		t.Fatal("old h4 must carry a missing timestamp, not a zero")
	}
	if newChain[3].Time != nil {
		t.Fatal("new j4 must carry a missing timestamp, not a zero")
	}

	o := newMbRsOracle(mbRsQuery())
	oldQualified := o.qualified(oldChain)
	newQualified := o.qualified(newChain)
	if _, ok := oldQualified["old"]; !ok {
		t.Fatal("old chain: old must qualify through in-window h1 and h2")
	}
	for _, id := range []string{"new", "solo"} {
		if _, ok := oldQualified[id]; ok {
			t.Fatalf("old chain: %s must NOT qualify (h3 out of window, h4 missing time; solo only in h5)", id)
		}
	}
	if _, ok := newQualified["new"]; !ok {
		t.Fatal("new chain: new must qualify through in-window j1 and j2 (j2's duplicate is one block)")
	}
	for _, id := range []string{"old", "solo"} {
		if _, ok := newQualified[id]; !ok {
			continue
		}
		t.Fatalf("new chain: %s must NOT qualify (old only in out-of-window j3; solo only in missing-time j4)", id)
	}

	// Literal answers must equal the oracle on both chains.
	oldOracleHits, oldTotal, oldBlocks, oldTo := o.answer(oldChain, OrderAsc)
	if !reflect.DeepEqual(oldOracleHits, mbRsOldHits()) {
		t.Fatalf("old oracle hits=%v, want %v", oldOracleHits, mbRsOldHits())
	}
	if oldTotal != 3 || oldBlocks != 2 || oldTo != 6 {
		t.Fatalf("old oracle stats=%d/%d to=%d, want 3/2 to=6", oldTotal, oldBlocks, oldTo)
	}
	newOracleHits, newTotal, newBlocks, newTo := o.answer(newChain, OrderAsc)
	if !reflect.DeepEqual(newOracleHits, mbRsNewHits()) {
		t.Fatalf("new oracle hits=%v, want %v", newOracleHits, mbRsNewHits())
	}
	if newTotal != 3 || newBlocks != 2 || newTo != 4 {
		t.Fatalf("new oracle stats=%d/%d to=%d, want 3/2 to=4", newTotal, newBlocks, newTo)
	}

	// The oracle must match the live index on both complete chain states.
	index := buildMbRsOldChain(t)
	live, err := index.QueryTxs(mbRsQuery())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(live), TxPage{
		Hits: mbRsOldHits()[:mbRsPageSize], TotalMatches: 3, MatchedBlocks: 2, ToHeight: 6,
	}) {
		t.Fatalf("live old-chain first page=%+v", live)
	}
	if live.NextCursor == "" {
		t.Fatal("old first page over 3 matches with page size 2 must carry a cursor")
	}

	if err := index.Restore(strings.NewReader(mbRsNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	if index.Tip != 4 {
		t.Fatalf("tip after restore=%d, want 4", index.Tip)
	}
	live, err = index.QueryTxs(mbRsQuery())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(live), TxPage{
		Hits: mbRsNewHits()[:mbRsPageSize], TotalMatches: 3, MatchedBlocks: 2, ToHeight: 4,
	}) {
		t.Fatalf("live new-chain first page=%+v", live)
	}
	if live.NextCursor == "" {
		t.Fatal("new first page over 3 matches with page size 2 must carry a cursor")
	}
}

// collectMbRsPages walks the cursor chain and returns every page together
// with the concatenated hits, asserting each page carries the whole-range
// stats and tip-bound ToHeight expected for the restored chain.
func collectMbRsPages(t *testing.T, index *Index, query TxQuery, total, blocks, toHeight int64) ([]TxPage, []TxHit) {
	t.Helper()
	var pages []TxPage
	var all []TxHit
	for {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("query failed: %v", err)
		}
		if page.TotalMatches != total || page.MatchedBlocks != blocks || page.ToHeight != toHeight {
			t.Fatalf("page stats=%d/%d to=%d, want the whole filtered range %d/%d to=%d",
				page.TotalMatches, page.MatchedBlocks, page.ToHeight, total, blocks, toHeight)
		}
		pages = append(pages, page)
		all = append(all, page.Hits...)
		if page.NextCursor == "" {
			return pages, all
		}
		query.Cursor = page.NextCursor
	}
}

// The core guarantee: after restoring the shorter, different chain, a fresh
// empty-cursor query re-judges MinBlocks entirely from the restored blocks.
// The identifier whose only in-window block sat on a deleted height ("solo"
// at h5) and the identifier surviving only in an out-of-window restored block
// ("old" at j3) must vanish completely; "new" qualifies through j1 and j2 and
// keeps every matching occurrence — both j2 duplicates at their original
// positions — with ToHeight following the new tip 4 rather than being pinned
// to the old tip 6 or shrunk by the filter.
func TestQueryTxsMinBlocksReJudgedAfterShorterRestore(t *testing.T) {
	index := buildMbRsOldChain(t)

	before := collectPages(t, index, mbRsQuery())
	var beforeHits []TxHit
	for _, page := range before {
		if page.TotalMatches != 3 || page.MatchedBlocks != 2 || page.ToHeight != 6 {
			t.Fatalf("pre-restore page stats=%d/%d to=%d, want 3/2 to=6",
				page.TotalMatches, page.MatchedBlocks, page.ToHeight)
		}
		beforeHits = append(beforeHits, page.Hits...)
	}
	if !reflect.DeepEqual(beforeHits, mbRsOldHits()) {
		t.Fatalf("pre-restore hits=%v, want %v", beforeHits, mbRsOldHits())
	}

	if err := index.Restore(strings.NewReader(mbRsNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	if index.Tip != 4 {
		t.Fatalf("tip=%d, want the shorter restored tip 4", index.Tip)
	}

	// No residue of the replaced chain: deleted heights and old hashes are
	// gone, including the h5 that held "solo"'s only in-window occurrence.
	for _, height := range []int64{5, 6} {
		if _, ok := index.Blocks[height]; ok {
			t.Fatalf("deleted height %d still present", height)
		}
	}
	for _, gone := range []string{"h1", "h2", "h3", "h4", "h5", "h6"} {
		if _, ok := index.ByHash[gone]; ok {
			t.Fatalf("replaced hash %s still indexed", gone)
		}
	}

	pages, afterHits := collectMbRsPages(t, index, mbRsQuery(), 3, 2, 4)
	if len(pages) != 2 {
		t.Fatalf("new-chain pages=%d, want 2 (2 hits then 1)", len(pages))
	}
	if !reflect.DeepEqual(afterHits, mbRsNewHits()) {
		t.Fatalf("post-restore hits=%v, want %v", afterHits, mbRsNewHits())
	}
	for _, hit := range afterHits {
		if hit.Height > 4 || strings.HasPrefix(hit.BlockHash, "h") {
			t.Fatalf("old-chain record leaked into the restored result: %+v", hit)
		}
		switch hit.TxID {
		case "old":
			t.Fatalf("old qualified on the old chain but must vanish (only j3, out of window): %+v", hit)
		case "solo":
			t.Fatalf("solo qualified only through deleted h5 and must vanish: %+v", hit)
		}
	}
	// The duplicate inside j2 keeps BOTH occurrences at their original
	// zero-based positions; nothing is merged or renumbered.
	if !containsHit(afterHits, mbRsNewHits()[1]) || !containsHit(afterHits, mbRsNewHits()[2]) {
		t.Fatalf("j2 duplicate occurrences must both survive with original positions: %v", afterHits)
	}
	requireOrdered(t, afterHits)

	// "old" and "solo" must not appear under ANY related query shape either:
	// explicitly filtering for them returns a successful empty page pinned to
	// the new tip with zero stats and no cursor.
	for _, id := range []string{"old", "solo"} {
		q := mbRsQuery()
		q.TxIDs = []string{id}
		page, err := index.QueryTxs(q)
		if err != nil {
			t.Fatalf("query for %s failed: %v", id, err)
		}
		if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 ||
			page.ToHeight != 4 || page.NextCursor != "" {
			t.Fatalf("%s must produce a zero-stats empty page pinned to tip 4: %+v", id, page)
		}
	}

	// Window evidence on the new chain: j2's time 50 is exactly the excluded
	// end of [0,50), so "new" then has only j1 and nobody qualifies; moving
	// the end to 51 includes j2 and the three occurrences return. The window
	// stays half-open: start included, end excluded.
	startZero := mbRsQuery()
	startZero.PageSize = 1000
	startZero.TimeStart, startZero.TimeEnd = intptr(0), intptr(50)
	excluded, err := index.QueryTxs(startZero)
	if err != nil {
		t.Fatal(err)
	}
	if len(excluded.Hits) != 0 || excluded.TotalMatches != 0 || excluded.MatchedBlocks != 0 || excluded.ToHeight != 4 {
		t.Fatalf("window [0,50) must exclude j2 at the end and leave nobody qualified: %+v", excluded)
	}
	startZero.TimeEnd = intptr(51)
	included, err := index.QueryTxs(startZero)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(included), TxPage{Hits: mbRsNewHits(), TotalMatches: 3, MatchedBlocks: 2, ToHeight: 4}) {
		t.Fatalf("window [0,51) must include j2 at time 50 and return every new occurrence: %+v", included)
	}

	// Without the window the missing-time j4 is NOT excluded: "new" then also
	// occupies j4, so it keeps a fourth occurrence (j4#0) while still
	// qualifying; "old" still has only j3 and "solo" only j4, so neither
	// reaches two distinct blocks. This pins "a missing timestamp is not a
	// zero" from the opposite side of the enabled-window checks above.
	noWindow := mbRsQuery()
	noWindow.PageSize = 1000
	noWindow.TimeStart, noWindow.TimeEnd = nil, nil
	nowPage, err := index.QueryTxs(noWindow)
	if err != nil {
		t.Fatal(err)
	}
	wantNoWindow := []TxHit{
		{Height: 1, BlockHash: "j1", TxID: "new", Position: 0},
		{Height: 2, BlockHash: "j2", TxID: "new", Position: 0},
		{Height: 2, BlockHash: "j2", TxID: "new", Position: 1},
		{Height: 4, BlockHash: "j4", TxID: "new", Position: 0},
	}
	if !reflect.DeepEqual(viewOf(nowPage), TxPage{Hits: wantNoWindow, TotalMatches: 4, MatchedBlocks: 3, ToHeight: 4}) {
		t.Fatalf("no-window query=%+v, want 4 occurrences over 3 blocks including missing-time j4", nowPage)
	}
}

// Descending pages walk the same kept occurrences backwards, with positions
// never renumbered and the same whole-range statistics and restored tip on
// every page.
func TestQueryTxsMinBlocksRestoreDescending(t *testing.T) {
	index := buildMbRsOldChain(t)
	if err := index.Restore(strings.NewReader(mbRsNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	query := mbRsQuery()
	query.Order = OrderDesc
	pages, all := collectMbRsPages(t, index, query, 3, 2, 4)
	if len(pages) != 2 {
		t.Fatalf("desc pages=%d, want 2", len(pages))
	}
	want := []TxHit{
		{Height: 2, BlockHash: "j2", TxID: "new", Position: 1},
		{Height: 2, BlockHash: "j2", TxID: "new", Position: 0},
		{Height: 1, BlockHash: "j1", TxID: "new", Position: 0},
	}
	if !reflect.DeepEqual(all, want) {
		t.Fatalf("desc hits=%v, want %v", all, want)
	}
	// Equality already pins positions to the original block indices (1 and 0
	// inside j2, 0 inside j1); descending order never renumbers a hit.
}

// Reading the restored result page by page partitions the complete filtered
// answer with no repetition and no omission, and resizing pages between
// continuations changes neither the records nor the whole-range statistics.
func TestQueryTxsMinBlocksRestorePagingPartitionsRange(t *testing.T) {
	for _, size := range []int{1, 2, 3, 1000} {
		t.Run("pagesize/"+strconv.Itoa(size), func(t *testing.T) {
			index := buildMbRsOldChain(t)
			if err := index.Restore(strings.NewReader(mbRsNewSnapshot)); err != nil {
				t.Fatalf("restore refused: %v", err)
			}
			query := mbRsQuery()
			query.PageSize = size
			seen := map[TxHit]bool{}
			var all []TxHit
			for {
				page, err := index.QueryTxs(query)
				if err != nil {
					t.Fatalf("page size %d failed: %v", size, err)
				}
				if page.TotalMatches != 3 || page.MatchedBlocks != 2 || page.ToHeight != 4 {
					t.Fatalf("page size %d stats=%+v, want 3/2 to=4", size, page)
				}
				for _, hit := range page.Hits {
					if seen[hit] {
						t.Fatalf("page size %d returned an occurrence twice: %+v", size, hit)
					}
					seen[hit] = true
					all = append(all, hit)
				}
				if page.NextCursor == "" {
					break
				}
				query.Cursor = page.NextCursor
			}
			if !reflect.DeepEqual(all, mbRsNewHits()) {
				t.Fatalf("page size %d hits=%v, want the complete range %v", size, all, mbRsNewHits())
			}
		})
	}
}

// A chain obtained by Restore and the SAME chain ingested block by block must
// return identical transactions and statistics under identical queries, in
// both directions and across every page size.
func TestQueryTxsMinBlocksRestoreEquivalentToIngestion(t *testing.T) {
	restored := buildMbRsOldChain(t)
	if err := restored.Restore(strings.NewReader(mbRsNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	ingested := ingestMbRsChain(t, mbRsNewChain())

	for _, order := range []TxOrder{OrderAsc, OrderDesc} {
		for _, size := range []int{1, 2, 3} {
			query := mbRsQuery()
			query.Order, query.PageSize = order, size
			rPages := collectPages(t, restored, query)
			iPages := collectPages(t, ingested, query)
			if len(rPages) != len(iPages) {
				t.Fatalf("order=%v size=%d page counts restored=%d ingested=%d",
					order, size, len(rPages), len(iPages))
			}
			for i := range rPages {
				if !reflect.DeepEqual(viewOf(rPages[i]), viewOf(iPages[i])) {
					t.Fatalf("order=%v size=%d page %d differs:\nrestored=%+v\ningested=%+v",
						order, size, i, rPages[i], iPages[i])
				}
			}
		}
	}
}

// A legal threshold nobody reaches after the restore is a successful empty
// page with zero statistics, the restored tip as ToHeight, and no cursor.
func TestQueryTxsMinBlocksRestoreNoneQualifiedEmptyPage(t *testing.T) {
	index := buildMbRsOldChain(t)
	if err := index.Restore(strings.NewReader(mbRsNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	query := mbRsQuery()
	query.MinBlocks = 3
	page, err := index.QueryTxs(query)
	if err != nil {
		t.Fatalf("a threshold nobody reaches must not be an error: %v", err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 ||
		page.ToHeight != 4 || page.NextCursor != "" {
		t.Fatalf("want a zero-stats empty page pinned to restored tip 4, got %+v", page)
	}

	// A narrow window [100,110) keeps no block at all (j1@0 and j2@50 are
	// below the start, j3@150 above the end, j4 has no time): same successful
	// empty-page rule.
	narrow := mbRsQuery()
	narrow.TimeStart, narrow.TimeEnd = intptr(100), intptr(110)
	page, err = index.QueryTxs(narrow)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 ||
		page.ToHeight != 4 || page.NextCursor != "" {
		t.Fatalf("empty window must yield a zero-stats empty page, got %+v", page)
	}
}

// The no-window rule across a shorter restore: without a timestamp window a
// missing timestamp does not exclude a block. The fixture below also includes
// the decisive "qualified on the old chain ONLY through a deleted height"
// identifier.
//
//	OLD chain, tip 5 (no TxIDs filter, no time window, MinBlocks 2):
//	  h1 parent g  [old, dup, dup]   no time
//	  h2 parent h1 [old]             no time
//	  h3 parent h2 [gone]            no time   // deleted height: "gone" block A
//	  h4 parent h3 [gone, keep]      no time   // deleted height: "gone" block B
//	  h5 parent h4 [keep]            no time
//	NEW snapshot, tip 3 (shorter):
//	  k1 parent g  [old]             no time
//	  k2 parent k1 [keep, keep]      no time   // duplicate in one block counts one
//	  k3 parent k2 [keep]            no time
//
// Old qualification: "old" {h1,h2}, "gone" {h3,h4} (only deleted heights),
// "keep" {h4,h5} (h4 deleted), "dup" {h1} (its two in-block repeats still
// count as ONE block, so it does not qualify). Old kept occurrences:
// h1 old#0, h2 old#0, h3 gone#0, h4 gone#0, h4 keep#1, h5 keep#0 = 6
// occurrences over blocks h1..h5 = 5 matched blocks.
// New qualification: "old" {k1} -> not qualified; "keep" {k2,k3} -> qualified
// (the k2 duplicate is one block); "gone" is absent entirely. New kept
// occurrences: k2 keep#0, k2 keep#1, k3 keep#0 = 3 occurrences over 2
// matched blocks, ToHeight 3.
func mbRsPlainOldChain() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"old", "dup", "dup"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"old"}},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"gone"}},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"gone", "keep"}},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"keep"}},
	}
}

func mbRsPlainNewChain() []Block {
	return []Block{
		{Height: 1, Hash: "k1", Parent: "g", Txs: []string{"old"}},
		{Height: 2, Hash: "k2", Parent: "k1", Txs: []string{"keep", "keep"}},
		{Height: 3, Hash: "k3", Parent: "k2", Txs: []string{"keep"}},
	}
}

const mbRsPlainNewSnapshot = `{"version":1,"tip":3,"blocks":[` +
	`{"height":1,"hash":"k1","parent":"g","txs":["old"]},` +
	`{"height":2,"hash":"k2","parent":"k1","txs":["keep","keep"]},` +
	`{"height":3,"hash":"k3","parent":"k2","txs":["keep"]}` +
	`]}`

func TestQueryTxsMinBlocksReJudgedAfterShorterRestoreNoWindow(t *testing.T) {
	index := New()
	for _, b := range mbRsPlainOldChain() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	query := TxQuery{MinBlocks: 2, PageSize: 3}

	_, oldAll := collectMbRsPages(t, index, query, 6, 5, 5)
	wantOld := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "old", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "old", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "gone", Position: 0},
		{Height: 4, BlockHash: "h4", TxID: "gone", Position: 0},
		{Height: 4, BlockHash: "h4", TxID: "keep", Position: 1},
		{Height: 5, BlockHash: "h5", TxID: "keep", Position: 0},
	}
	if !reflect.DeepEqual(oldAll, wantOld) {
		t.Fatalf("old no-window hits=%v, want %v", oldAll, wantOld)
	}

	if err := index.Restore(strings.NewReader(mbRsPlainNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	if index.Tip != 3 {
		t.Fatalf("tip=%d, want 3", index.Tip)
	}
	// "gone" qualified on the old chain ONLY through deleted heights h3/h4;
	// it must have no occurrence on the new chain at all.
	gonePage, err := index.QueryTxs(TxQuery{TxIDs: []string{"gone"}})
	if err != nil || len(gonePage.Hits) != 0 || gonePage.ToHeight != 3 {
		t.Fatalf("deleted-only identifier gone=%+v err=%v, want an empty page pinned to 3", gonePage, err)
	}

	_, newAll := collectMbRsPages(t, index, query, 3, 2, 3)
	wantNew := []TxHit{
		{Height: 2, BlockHash: "k2", TxID: "keep", Position: 0},
		{Height: 2, BlockHash: "k2", TxID: "keep", Position: 1},
		{Height: 3, BlockHash: "k3", TxID: "keep", Position: 0},
	}
	if !reflect.DeepEqual(newAll, wantNew) {
		t.Fatalf("new no-window hits=%v, want %v", newAll, wantNew)
	}
	for _, hit := range newAll {
		if hit.TxID == "gone" || hit.TxID == "old" || hit.TxID == "dup" {
			t.Fatalf("identifier that lost qualification leaked through: %+v", hit)
		}
	}

	// The missing timestamps did not exclude any block here (no window): the
	// same chains restored WITH an enabling window behave differently, guarded
	// by the windowed fixture. This pins the no-window half of the rule.
}

// A snapshot whose LAST block carries a wrong parent link must reject the
// whole restore with ErrInvalidSnapshot: the old chain's MinBlocks answer and
// statistics stay exactly as they were, and a valid pagination cursor minted
// before the failed restore continues to read the OLD chain to its end.
func TestQueryTxsMinBlocksBadTailSnapshotRejectedKeepsOldChainAndCursor(t *testing.T) {
	index := buildMbRsOldChain(t)
	query := mbRsQuery()
	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	if first.ToHeight != 6 || first.TotalMatches != 3 || first.MatchedBlocks != 2 || first.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", first)
	}
	if !reflect.DeepEqual(first.Hits, mbRsOldHits()[:mbRsPageSize]) {
		t.Fatalf("first page hits=%v, want %v", first.Hits, mbRsOldHits()[:mbRsPageSize])
	}

	// The bad tail is the last block's parent discovered only at the end.
	err = index.Restore(strings.NewReader(mbRsBrokenSnapshot))
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("restore err=%v, want ErrInvalidSnapshot", err)
	}
	if !strings.Contains(err.Error(), "does not link to its parent") {
		t.Fatalf("restore must fail on the last block's parent link: %v", err)
	}
	if index.Tip != 6 {
		t.Fatalf("tip=%d after rejected restore, want 6", index.Tip)
	}
	// Nothing from the snapshot's valid prefix was installed.
	for _, leaked := range []string{"j1", "j2", "j3", "j4"} {
		if _, ok := index.ByHash[leaked]; ok {
			t.Fatalf("snapshot hash %s from the valid prefix was left indexed", leaked)
		}
	}

	// The old chain still answers exactly as before from a fresh full walk.
	_, freshHits := collectMbRsPages(t, index, mbRsQuery(), 3, 2, 6)
	if !reflect.DeepEqual(freshHits, mbRsOldHits()) {
		t.Fatalf("hits after rejected restore=%v, want the original %v", freshHits, mbRsOldHits())
	}

	// The cursor minted before the failed restore is still valid and continues
	// the OLD chain: page 2 is the remaining single occurrence, with the
	// original pinned statistics and tip, then the cursor chain ends.
	cont := mbRsQuery()
	cont.Cursor = first.NextCursor
	page2, err := index.QueryTxs(cont)
	if err != nil {
		t.Fatalf("pre-restore cursor must keep reading the old chain: %v", err)
	}
	if !reflect.DeepEqual(page2.Hits, mbRsOldHits()[2:]) {
		t.Fatalf("page2 hits=%v, want %v", page2.Hits, mbRsOldHits()[2:])
	}
	if page2.TotalMatches != 3 || page2.MatchedBlocks != 2 || page2.ToHeight != 6 || page2.NextCursor != "" {
		t.Fatalf("page2 must carry the original pinned stats and end the walk: %+v", page2)
	}

	// A full walk starting from the same first page partitions the original
	// three occurrences exactly once (guards against any state half-applied by
	// the rejected restore).
	joined := append(append([]TxHit{}, first.Hits...), page2.Hits...)
	if !reflect.DeepEqual(joined, mbRsOldHits()) {
		t.Fatalf("cursor walk across rejected restore=%v, want %v", joined, mbRsOldHits())
	}
}

// After a SUCCESSFUL shorter restore the old cursor — whose pinned range
// [1,6] the new tip 4 no longer covers — must fail with ErrQueryChanged and a
// fully zeroed page, never resume on stitched data; restarting from an empty
// cursor describes the new chain.
func TestQueryTxsMinBlocksOldCursorDiesAfterSuccessfulShorterRestore(t *testing.T) {
	index := buildMbRsOldChain(t)
	first, err := index.QueryTxs(mbRsQuery())
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("first page must carry a cursor")
	}

	if err := index.Restore(strings.NewReader(mbRsNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	page, err := index.QueryTxs(TxQuery{
		TxIDs:     mbRsQuery().TxIDs,
		TimeStart: intptr(mbRsWindowStart),
		TimeEnd:   intptr(mbRsWindowEnd),
		MinBlocks: mbRsMinBlocks,
		PageSize:  mbRsPageSize,
		Cursor:    first.NextCursor,
	})
	requireChangedZeroed(t, page, err)

	// Restarting from an empty cursor re-pins to the restored tip and returns
	// the new-chain answer page by page.
	_, all := collectMbRsPages(t, index, mbRsQuery(), 3, 2, 4)
	if !reflect.DeepEqual(all, mbRsNewHits()) {
		t.Fatalf("fresh walk after restore=%v, want %v", all, mbRsNewHits())
	}
}
