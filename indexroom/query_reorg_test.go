package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This file gives QueryTxs the same reorg regression that
// stats_reorg_test.go gives QueryTimeStats. A legal reorg replaces the main
// chain suffix with a SHORTER branch while a paginated query is in flight.
// A first page issued at that instant must answer from exactly one complete
// main chain: the resolved tip, TotalMatches, MatchedBlocks, every returned
// hit (block hash and in-block position included), and the presence of the
// next cursor must all describe that same branch — an old tip height paired
// with new-chain counts, or one page stitching both branches, must fail even
// when the stitched hits are still ordered by height and position.
//
// The scenario (no TxIDs filter, PageSize 4; the first-page To is left zero
// so it pins the observed tip):
//
//	Retained ancestor (both chains):
//	  h1 txs [a, x, a]    // a twice in one block: both occurrences survive,
//	                     // positions 0 and 2, but the block counts once
//	Old branch, tip 5:
//	  h2 txs [b, c]
//	  h3 txs [a, c]
//	  h4 txs [d, d, e]   // d twice in one block
//	  h5 txs [e, f, g, h] // e also appears at h4: cross-block duplicate,
//	                     // still one occurrence per appearance
//	New shorter branch, tip 4 (replaces h2..h5, rooted at the retained h1):
//	  j2 txs [p, p, q]   // p twice in one block, different ids/counts
//	  j3 txs [r]
//	  j4 txs [s, t, u, v]
//
// Old chain (range 1..5): 14 occurrences over 5 matched blocks, tip 5.
// New chain (range 1..4): 11 occurrences over 4 matched blocks, tip 4.
// The fourth first-page hit already differs (h2/b vs j2/p), so even page 1
// carries branch-specific records; the two complete answers further differ
// in totals, block count, and tip, so any mixed page stands out. The
// orderings "query parks holding the lock, reorg waits" and "reorg commits
// parked, query waits" are both forced deterministically through rendezvous
// hooks, rather than hoped for across repeated runs.

func queryReorgOldChain() []Block {
	return []Block{
		{Height: 1, Hash: "qh1", Parent: "g", Txs: []string{"a", "x", "a"}},
		{Height: 2, Hash: "qh2", Parent: "qh1", Txs: []string{"b", "c"}},
		{Height: 3, Hash: "qh3", Parent: "qh2", Txs: []string{"a", "c"}},
		{Height: 4, Hash: "qh4", Parent: "qh3", Txs: []string{"d", "d", "e"}},
		{Height: 5, Hash: "qh5", Parent: "qh4", Txs: []string{"e", "f", "g", "h"}},
	}
}

func queryReorgNewBranch() []Block {
	return []Block{
		{Height: 2, Hash: "qj2", Parent: "qh1", Txs: []string{"p", "p", "q"}},
		{Height: 3, Hash: "qj3", Parent: "qj2", Txs: []string{"r"}},
		{Height: 4, Hash: "qj4", Parent: "qj3", Txs: []string{"s", "t", "u", "v"}},
	}
}

func buildQueryReorgChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range queryReorgOldChain() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at height %d: %v", b.Height, err)
		}
	}
	return index
}

type queryReorgRefs struct {
	oldChain []Block
	newChain []Block
}

func queryReorgReferences() queryReorgRefs {
	old := queryReorgOldChain()
	newChain := append(append([]Block{}, old[:1]...), queryReorgNewBranch()...)
	return queryReorgRefs{oldChain: old, newChain: newChain}
}

var queryReorgFirstQuery = TxQuery{PageSize: 4}

// txPageExpect is every externally observable field of one TxPage except
// the opaque cursor token, for which only presence/absence is compared.
type txPageExpect struct {
	hits    []TxHit
	total   int64
	blocks  int64
	to      int64
	hasMore bool
}

func coreOf(page TxPage) txPageExpect {
	return txPageExpect{
		hits:    page.Hits,
		total:   page.TotalMatches,
		blocks:  page.MatchedBlocks,
		to:      page.ToHeight,
		hasMore: page.NextCursor != "",
	}
}

// referenceTxHits is an independent oracle: it collects matching
// occurrences from an explicitly supplied, complete ordered chain rather
// than from the index under test, mirroring collectLocked's documented
// semantics (height then position, every occurrence, block counted once).
func referenceTxHits(chain []Block, from, to int64, txIDs []string) (hits []TxHit, blocks int64) {
	var filter map[string]struct{}
	if len(txIDs) > 0 {
		filter = make(map[string]struct{}, len(txIDs))
		for _, id := range txIDs {
			filter[id] = struct{}{}
		}
	}
	hits = []TxHit{}
	for height := from; height <= to; height++ {
		block := chain[height-1]
		matched := false
		for position, tx := range block.Txs {
			if filter != nil {
				if _, ok := filter[tx]; !ok {
					continue
				}
			}
			hits = append(hits, TxHit{Height: height, BlockHash: block.Hash, TxID: tx, Position: position})
			matched = true
		}
		if matched {
			blocks++
		}
	}
	return hits, blocks
}

// oracleExpect derives one expected page from a complete chain at an
// explicit offset. ok is false when the chain cannot answer a page pinned
// to [from, to] (its tip is below to).
func oracleExpect(chain []Block, from, to int64, txIDs []string, off int64, pageSize int) (txPageExpect, bool) {
	tip := int64(len(chain))
	if to < 1 || from > to || to > tip {
		return txPageExpect{}, false
	}
	hits, blocks := referenceTxHits(chain, from, to, txIDs)
	if off < 0 || off > int64(len(hits)) {
		return txPageExpect{}, false
	}
	rest := hits[off:]
	end := pageSize
	if end > len(rest) {
		end = len(rest)
	}
	return txPageExpect{
		hits:    append([]TxHit{}, rest[:end]...),
		total:   int64(len(hits)),
		blocks:  blocks,
		to:      to,
		hasMore: end < len(rest),
	}, true
}

func oracleFirstExpect(chain []Block, query TxQuery) (txPageExpect, bool) {
	from := query.From
	if from == 0 {
		from = 1
	}
	to := query.To
	tip := int64(len(chain))
	if to == 0 || to > tip {
		to = tip
	}
	size := query.PageSize
	if size == 0 {
		size = DefaultPageSize
	}
	if from > to {
		return txPageExpect{hits: []TxHit{}, to: to}, true
	}
	return oracleExpect(chain, from, to, query.TxIDs, 0, size)
}

// literalOldFirstPage is the hand-derived first page (PageSize 4) of the
// complete old chain.
func literalOldFirstPage() txPageExpect {
	return txPageExpect{
		hits: []TxHit{
			{Height: 1, BlockHash: "qh1", TxID: "a", Position: 0},
			{Height: 1, BlockHash: "qh1", TxID: "x", Position: 1},
			{Height: 1, BlockHash: "qh1", TxID: "a", Position: 2},
			{Height: 2, BlockHash: "qh2", TxID: "b", Position: 0},
		},
		total:   14,
		blocks:  5,
		to:      5,
		hasMore: true,
	}
}

// literalNewFirstPage is the hand-derived first page of the complete new
// chain: the retained block 1 (a, x, a), then the replacement branch's
// first p.
func literalNewFirstPage() txPageExpect {
	return txPageExpect{
		hits: []TxHit{
			{Height: 1, BlockHash: "qh1", TxID: "a", Position: 0},
			{Height: 1, BlockHash: "qh1", TxID: "x", Position: 1},
			{Height: 1, BlockHash: "qh1", TxID: "a", Position: 2},
			{Height: 2, BlockHash: "qj2", TxID: "p", Position: 0},
		},
		total:   11,
		blocks:  4,
		to:      4,
		hasMore: true,
	}
}

// classifySuccessfulPage accepts got only when it is the complete page of
// exactly one chain: Hits (including block hashes and positions),
// TotalMatches, MatchedBlocks, ToHeight, and cursor presence all come from
// that same chain. Fields stitched from the old and the new chain are
// reported MIXED; anything matching neither is reported CORRUPT. It
// deliberately never treats "hits are ordered" or "hit count fits the
// total" as sufficient: several mixed pages satisfy both.
func classifySuccessfulPage(got txPageExpect, oldE txPageExpect, oldOK bool, newE txPageExpect, newOK bool) error {
	if oldOK && reflect.DeepEqual(got, oldE) {
		return nil
	}
	if newOK && reflect.DeepEqual(got, newE) {
		return nil
	}
	var report strings.Builder
	fmt.Fprintln(&report, "QueryTxs page does not match one complete main chain:")
	if !oldOK || !newOK {
		fmt.Fprintf(&report, "  (old chain answerable=%v, new chain answerable=%v)\n", oldOK, newOK)
	}
	tags := map[string]bool{}
	tag := func(name string, gotV, oldV, newV any, matchesOld, matchesNew bool) {
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
		fmt.Fprintf(&report, "  %-18s got=%v old=%v new=%v -> %s\n", name, gotV, oldV, newV, label)
	}
	tagHits := func() {
		matchesOld := oldOK && reflect.DeepEqual(got.hits, oldE.hits)
		matchesNew := newOK && reflect.DeepEqual(got.hits, newE.hits)
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
		fmt.Fprintf(&report, "  %-18s got=%d hits old=%d hits new=%d hits -> %s\n",
			"Hits", len(got.hits), lenIf(oldOK, oldE), lenIf(newOK, newE), label)
		if !matchesOld && (!oldOK || len(got.hits) == len(oldE.hits)) {
			fmt.Fprintf(&report, "    got: %v\n", got.hits)
			if oldOK {
				fmt.Fprintf(&report, "    old: %v\n", oldE.hits)
			}
		}
		if !matchesNew && (!newOK || len(got.hits) == len(newE.hits)) {
			if newOK {
				fmt.Fprintf(&report, "    new: %v\n", newE.hits)
			}
		}
	}
	tagHits()
	tag("TotalMatches", got.total, oldIf(oldOK, oldE.total), newIf(newOK, newE.total),
		oldOK && got.total == oldE.total, newOK && got.total == newE.total)
	tag("MatchedBlocks", got.blocks, oldIf(oldOK, oldE.blocks), newIf(newOK, newE.blocks),
		oldOK && got.blocks == oldE.blocks, newOK && got.blocks == newE.blocks)
	tag("ToHeight", got.to, oldIf(oldOK, oldE.to), newIf(newOK, newE.to),
		oldOK && got.to == oldE.to, newOK && got.to == newE.to)
	tag("HasCursor", got.hasMore, oldIf(oldOK, oldE.hasMore), newIf(newOK, newE.hasMore),
		oldOK && got.hasMore == oldE.hasMore, newOK && got.hasMore == newE.hasMore)
	if tags["old"] && tags["new"] {
		fmt.Fprintf(&report, "  => MIXED: pieces come from both the old and the new chain; one page must reflect a single main chain")
	} else {
		fmt.Fprintf(&report, "  => CORRUPT: matches neither complete chain")
	}
	return fmt.Errorf("%s", report.String())
}

func lenIf(ok bool, e txPageExpect) int {
	if !ok {
		return -1
	}
	return len(e.hits)
}

func oldIf(ok bool, v any) any {
	if !ok {
		return "n/a"
	}
	return v
}

func newIf(ok bool, v any) any { return oldIf(ok, v) }

func classifyFirstPage(got TxPage, query TxQuery, refs queryReorgRefs) error {
	oldE, oldOK := oracleFirstExpect(refs.oldChain, query)
	newE, newOK := oracleFirstExpect(refs.newChain, query)
	return classifySuccessfulPage(coreOf(got), oldE, oldOK, newE, newOK)
}

// The independent oracle must reproduce the hand-derived pages on both
// complete chains, including later pages of the pinned old chain.
func TestQueryReorgReferenceMatchesHandDerived(t *testing.T) {
	refs := queryReorgReferences()

	if got, ok := oracleFirstExpect(refs.oldChain, queryReorgFirstQuery); !ok || !reflect.DeepEqual(got, literalOldFirstPage()) {
		t.Fatalf("old-chain oracle first page=%+v ok=%v\nwant %+v", got, ok, literalOldFirstPage())
	}
	if got, ok := oracleFirstExpect(refs.newChain, queryReorgFirstQuery); !ok || !reflect.DeepEqual(got, literalNewFirstPage()) {
		t.Fatalf("new-chain oracle first page=%+v ok=%v\nwant %+v", got, ok, literalNewFirstPage())
	}

	wantSecond := txPageExpect{
		hits: []TxHit{
			{Height: 2, BlockHash: "qh2", TxID: "c", Position: 1},
			{Height: 3, BlockHash: "qh3", TxID: "a", Position: 0},
			{Height: 3, BlockHash: "qh3", TxID: "c", Position: 1},
			{Height: 4, BlockHash: "qh4", TxID: "d", Position: 0},
		},
		total: 14, blocks: 5, to: 5, hasMore: true,
	}
	if got, ok := oracleExpect(refs.oldChain, 1, 5, nil, 4, 4); !ok || !reflect.DeepEqual(got, wantSecond) {
		t.Fatalf("old-chain oracle page 2=%+v ok=%v\nwant %+v", got, ok, wantSecond)
	}
	wantThird := txPageExpect{
		hits: []TxHit{
			{Height: 4, BlockHash: "qh4", TxID: "d", Position: 1},
			{Height: 4, BlockHash: "qh4", TxID: "e", Position: 2},
			{Height: 5, BlockHash: "qh5", TxID: "e", Position: 0},
			{Height: 5, BlockHash: "qh5", TxID: "f", Position: 1},
		},
		total: 14, blocks: 5, to: 5, hasMore: true,
	}
	if got, ok := oracleExpect(refs.oldChain, 1, 5, nil, 8, 4); !ok || !reflect.DeepEqual(got, wantThird) {
		t.Fatalf("old-chain oracle page 3=%+v ok=%v\nwant %+v", got, ok, wantThird)
	}
	wantLast := txPageExpect{
		hits: []TxHit{
			{Height: 5, BlockHash: "qh5", TxID: "g", Position: 2},
			{Height: 5, BlockHash: "qh5", TxID: "h", Position: 3},
		},
		total:   14,
		blocks:  5,
		to:      5,
		hasMore: false,
	}
	if got, ok := oracleExpect(refs.oldChain, 1, 5, nil, 12, 4); !ok || !reflect.DeepEqual(got, wantLast) {
		t.Fatalf("old-chain oracle last page=%+v ok=%v\nwant %+v", got, ok, wantLast)
	}

	// The shortened new chain cannot serve a page still pinned at old tip 5.
	if _, ok := oracleExpect(refs.newChain, 1, 5, nil, 4, 4); ok {
		t.Fatal("new-chain oracle must refuse a page pinned above its tip")
	}
}

// The occurrence/block-count semantics the regression rests on, asserted
// directly for both chains.
func TestQueryReorgCountingSemanticsOnBothChains(t *testing.T) {
	old := literalOldFirstPage()
	if old.total != 14 || old.blocks != 5 || old.to != 5 {
		t.Fatalf("old chain: total=%d blocks=%d to=%d, want 14/5/5", old.total, old.blocks, old.to)
	}
	if old.hits[0] != (TxHit{Height: 1, BlockHash: "qh1", TxID: "a", Position: 0}) ||
		old.hits[2] != (TxHit{Height: 1, BlockHash: "qh1", TxID: "a", Position: 2}) {
		t.Fatalf("duplicate a in h1 must survive twice at positions 0 and 2: %v", old.hits[:3])
	}
	// d occurs twice in h4 and e occurs in two different blocks: all
	// occurrences count, but each block contributes once to MatchedBlocks.
	fullOld, _ := referenceTxHits(queryReorgOldChain(), 1, 5, nil)
	if len(fullOld) != 14 {
		t.Fatalf("old full occurrences=%d, want 14", len(fullOld))
	}
	var dCount, eCount int
	for _, hit := range fullOld {
		switch {
		case hit.TxID == "d" && hit.Height == 4:
			dCount++
		case hit.TxID == "e":
			eCount++
		}
	}
	if dCount != 2 || eCount != 2 {
		t.Fatalf("d occurrences=%d (want 2), e occurrences=%d (want 2)", dCount, eCount)
	}

	first := literalNewFirstPage()
	if first.total != 11 || first.blocks != 4 || first.to != 4 {
		t.Fatalf("new chain: total=%d blocks=%d to=%d, want 11/4/4", first.total, first.blocks, first.to)
	}
	fullNew, newBlocks := referenceTxHits(queryReorgReferences().newChain, 1, 4, nil)
	if len(fullNew) != 11 || newBlocks != 4 {
		t.Fatalf("new full occurrences=%d blocks=%d, want 11/4", len(fullNew), newBlocks)
	}
	ps := map[[2]any]bool{}
	for _, hit := range fullNew {
		if hit.TxID == "p" {
			ps[[2]any{hit.Height, hit.Position}] = true
		}
		for _, gone := range []string{"b", "c", "d", "e", "f", "g", "h"} {
			if hit.TxID == gone {
				t.Fatalf("removed old-chain tx %q survived on the new chain: %+v", gone, hit)
			}
		}
	}
	if len(ps) != 2 || !ps[[2]any{int64(2), 0}] || !ps[[2]any{int64(2), 1}] {
		t.Fatalf("p must appear twice in qj2 at positions 0 and 1: %v", ps)
	}

	// The branches must be observably different on every discriminating axis.
	if old.total == first.total || old.blocks == first.blocks || old.to == first.to {
		t.Fatal("test setup: old and new chains must differ in totals, block count, and tip")
	}
	if reflect.DeepEqual(fullOld, fullNew) {
		t.Fatal("test setup: the branches must differ in actual transaction records")
	}
}

// Baseline, no concurrency: before the reorg only the old-chain page is
// possible; after it returns only the complete new-chain answer, including
// full pagination to the end.
func TestQueryTxsBeforeAndAfterShorteningReorg(t *testing.T) {
	index := buildQueryReorgChain(t)
	refs := queryReorgReferences()

	before, err := index.QueryTxs(queryReorgFirstQuery)
	if err != nil {
		t.Fatal(err)
	}
	if err := classifyFirstPage(before, queryReorgFirstQuery, refs); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(coreOf(before), literalOldFirstPage()) {
		t.Fatalf("pre-reorg first page=%+v, want complete old chain", coreOf(before))
	}

	dropped, err := index.Reorg(queryReorgNewBranch())
	if err != nil {
		t.Fatalf("shortening reorg refused: %v", err)
	}
	if want := []int64{2, 3, 4, 5}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped=%v, want %v", dropped, want)
	}
	if index.Tip != 4 {
		t.Fatalf("tip=%d, want shortened tip 4", index.Tip)
	}
	for _, gone := range []string{"qh2", "qh3", "qh4", "qh5"} {
		if _, ok := index.ByHash[gone]; ok {
			t.Fatalf("removed hash %s still indexed", gone)
		}
	}

	after, err := index.QueryTxs(queryReorgFirstQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(coreOf(after), literalNewFirstPage()) {
		t.Fatalf("post-reorg first page=%+v, want complete new chain", coreOf(after))
	}

	// Removed transactions are gone; replacement duplicates come through.
	if page, err := index.QueryTxs(TxQuery{TxIDs: []string{"f"}}); err != nil || page.TotalMatches != 0 {
		t.Fatalf("removed tx f still matches: page=%+v err=%v", page, err)
	}
	pPage, err := index.QueryTxs(TxQuery{TxIDs: []string{"p"}})
	if err != nil {
		t.Fatal(err)
	}
	wantP := []TxHit{
		{Height: 2, BlockHash: "qj2", TxID: "p", Position: 0},
		{Height: 2, BlockHash: "qj2", TxID: "p", Position: 1},
	}
	if !reflect.DeepEqual(pPage.Hits, wantP) || pPage.MatchedBlocks != 1 {
		t.Fatalf("p hits=%+v blocks=%d, want two occurrences in one block", pPage.Hits, pPage.MatchedBlocks)
	}

	// Every page of a full walk matches the complete new chain and keeps the
	// pinned statistics; concatenated hits are exactly all 11 occurrences.
	pages := collectPages(t, index, queryReorgFirstQuery)
	var all []TxHit
	for i, page := range pages {
		if page.TotalMatches != 11 || page.MatchedBlocks != 4 || page.ToHeight != 4 {
			t.Fatalf("new-chain page %d changed stats: %+v", i, page)
		}
		all = append(all, page.Hits...)
	}
	wantAll, _ := referenceTxHits(refs.newChain, 1, 4, nil)
	if !reflect.DeepEqual(all, wantAll) {
		t.Fatalf("new-chain pagination=%v\nwant %v", all, wantAll)
	}
}

// weakOrderedPageCheck is the deliberately weak guarantee this regression
// must be stronger than: hits ordered by height/position and the page size
// fitting the total. Several old/new mixtures satisfy it.
func weakOrderedPageCheck(page txPageExpect) bool {
	lastHeight, lastPosition := int64(0), -1
	for _, hit := range page.hits {
		if hit.Height < lastHeight || (hit.Height == lastHeight && hit.Position <= lastPosition) {
			return false
		}
		lastHeight, lastPosition = hit.Height, hit.Position
	}
	return len(page.hits) > 0 && int64(len(page.hits)) <= page.total && page.blocks >= 1
}

// The classifier must reject mixed and corrupt pages — including ordered
// ones that a weak ordering check accepts.
func TestClassifyTxPageRejectsMixedAndCorruptReturns(t *testing.T) {
	old := literalOldFirstPage()
	first := literalNewFirstPage()
	refs := queryReorgReferences()
	oldE, oldOK := oracleFirstExpect(refs.oldChain, queryReorgFirstQuery)
	newE, newOK := oracleFirstExpect(refs.newChain, queryReorgFirstQuery)
	if err := classifySuccessfulPage(old, oldE, oldOK, newE, newOK); err != nil {
		t.Fatalf("pure old page rejected: %v", err)
	}
	if err := classifySuccessfulPage(first, oldE, oldOK, newE, newOK); err != nil {
		t.Fatalf("pure new page rejected: %v", err)
	}

	// Mixture 1: new-chain hits and cursor, but the OLD tip height with OLD
	// totals. Hits stay ordered and within 1..5, so the weak check accepts.
	mixed1 := first
	mixed1.to = old.to
	mixed1.total = old.total
	mixed1.blocks = old.blocks
	if !weakOrderedPageCheck(mixed1) {
		t.Fatal("test setup: mixed1 is meant to pass the weak check")
	}
	if err := classifySuccessfulPage(mixed1, oldE, oldOK, newE, newOK); err == nil {
		t.Fatal("new-chain records under old-chain stats were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("mixed1 not reported as MIXED:\n%v", err)
	}

	// Mixture 2: old-chain hits with NEW-chain totals, block count, and tip.
	// Ordered, within range, fits the total — weak check passes.
	mixed2 := old
	mixed2.to = first.to
	mixed2.total = first.total
	mixed2.blocks = first.blocks
	if !weakOrderedPageCheck(mixed2) {
		t.Fatal("test setup: mixed2 is meant to pass the weak check")
	}
	if err := classifySuccessfulPage(mixed2, oldE, oldOK, newE, newOK); err == nil {
		t.Fatal("old-chain records under new-chain stats were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("mixed2 not reported as MIXED:\n%v", err)
	}

	// Mixture 3: one page stitching BOTH branches, still strictly ordered by
	// height/position: old-only h3 followed by new-only j4.
	mixed3 := txPageExpect{
		hits: []TxHit{
			{Height: 3, BlockHash: "qh3", TxID: "a", Position: 0},
			{Height: 4, BlockHash: "qj4", TxID: "s", Position: 0},
		},
		total: 2, blocks: 2, to: 5, hasMore: false,
	}
	if !weakOrderedPageCheck(mixed3) {
		t.Fatal("test setup: mixed3 is meant to pass the weak check")
	}
	if err := classifySuccessfulPage(mixed3, oldE, oldOK, newE, newOK); err == nil {
		t.Fatal("a page stitching both branches was accepted")
	}

	// Corruption belonging to neither chain (wrong occurrence count) fails.
	corrupt := first
	corrupt.total++
	if err := classifySuccessfulPage(corrupt, oldE, oldOK, newE, newOK); err == nil {
		t.Fatal("corrupt page matching neither chain was accepted")
	}
}

// Installs one-shot rendezvous hooks and guarantees they are removed.
type queryReorgHooks struct {
	queryParked  chan struct{}
	releaseQuery chan struct{}
	reorgParked  chan struct{}
	releaseReorg chan struct{}
	queryOnce    sync.Once
	reorgOnce    sync.Once
}

func installQueryReorgHooks(t *testing.T) *queryReorgHooks {
	t.Helper()
	h := &queryReorgHooks{
		queryParked:  make(chan struct{}),
		releaseQuery: make(chan struct{}),
		reorgParked:  make(chan struct{}),
		releaseReorg: make(chan struct{}),
	}
	queryTxsHookLocked = func() {
		h.queryOnce.Do(func() { close(h.queryParked) })
		<-h.releaseQuery
	}
	reorgAppliedHookLocked = func(newTip int64) {
		if newTip != 4 {
			t.Errorf("reorg hook saw newTip=%d, want 4", newTip)
		}
		h.reorgOnce.Do(func() { close(h.reorgParked) })
		<-h.releaseReorg
	}
	t.Cleanup(func() {
		queryTxsHookLocked = nil
		reorgAppliedHookLocked = nil
	})
	return h
}

// First page order 1: the query has pinned the OLD tip (5) and is holding
// the lock when the reorg starts. It must finish on the complete old chain;
// afterwards the returned cursor is dead because its pinned range changed.
func TestQueryTxsFirstPageInFlightObservesCompleteOldChain(t *testing.T) {
	index := buildQueryReorgChain(t)
	refs := queryReorgReferences()
	h := installQueryReorgHooks(t)

	pageCh := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, err := index.QueryTxs(queryReorgFirstQuery)
		if err != nil {
			errCh <- err
			return
		}
		pageCh <- page
	}()

	<-h.queryParked // first page holds index.mu and has pinned tip 5
	reorgDone := make(chan struct{})
	go func() {
		if _, err := index.Reorg(queryReorgNewBranch()); err != nil {
			errCh <- err
		}
		close(reorgDone)
	}()

	// Release the parked query first; the reorg cannot touch the chain until
	// the query drops the lock.
	close(h.releaseQuery)
	var got TxPage
	select {
	case got = <-pageCh:
	case err := <-errCh:
		t.Fatalf("concurrent call failed: %v", err)
	}
	if err := classifyFirstPage(got, queryReorgFirstQuery, refs); err != nil {
		t.Fatalf("in-flight first page did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(coreOf(got), literalOldFirstPage()) {
		t.Fatalf("query holding the lock across reorg start must see old chain:\n%+v", coreOf(got))
	}

	close(h.releaseReorg) // unblock the parked reorg
	<-reorgDone

	// The old cursor now points at a replaced range: ErrQueryChanged, zero
	// page, no next cursor.
	failed, err := index.QueryTxs(TxQuery{PageSize: 4, Cursor: got.NextCursor})
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("stale cursor err=%v, want ErrQueryChanged", err)
	}
	if !reflect.DeepEqual(failed, TxPage{}) {
		t.Fatalf("ErrQueryChanged carried data or stats: %+v", failed)
	}
}

// First page order 2: the reorg has already installed the complete shorter
// new chain (tip 4) but has not returned when the first page starts. The
// query must wait and then answer from the complete new chain, and its
// cursor must paginate the new chain consistently.
func TestQueryTxsFirstPageInFlightObservesCompleteNewChain(t *testing.T) {
	index := buildQueryReorgChain(t)
	refs := queryReorgReferences()
	h := installQueryReorgHooks(t)

	reorgErr := make(chan error, 1)
	go func() {
		_, err := index.Reorg(queryReorgNewBranch())
		reorgErr <- err
	}()
	<-h.reorgParked // new branch applied (tip 4), Reorg still holds the lock

	pageCh := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, err := index.QueryTxs(queryReorgFirstQuery)
		if err != nil {
			errCh <- err
			return
		}
		pageCh <- page
	}()

	// Releasing the reorg hands the lock to the waiting query; the first
	// chain state that query can see is already entirely the new one.
	close(h.releaseReorg)
	if err := <-reorgErr; err != nil {
		t.Fatalf("reorg failed: %v", err)
	}
	close(h.releaseQuery) // drain the query seam (released channel: no park)

	var got TxPage
	select {
	case got = <-pageCh:
	case err := <-errCh:
		t.Fatalf("concurrent query failed: %v", err)
	}
	if err := classifyFirstPage(got, queryReorgFirstQuery, refs); err != nil {
		t.Fatalf("query across reorg commit did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(coreOf(got), literalNewFirstPage()) {
		t.Fatalf("query after reorg commit must see the complete new chain:\n%+v", coreOf(got))
	}

	// Its cursor continues over the same complete new chain: constant pinned
	// stats and exactly every new-chain occurrence, no old-chain suffix.
	query := TxQuery{PageSize: 4, Cursor: got.NextCursor}
	var all = append([]TxHit{}, got.Hits...)
	for {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("new-chain continuation failed: %v", err)
		}
		if page.TotalMatches != 11 || page.MatchedBlocks != 4 || page.ToHeight != 4 {
			t.Fatalf("continuation stats diverged from the observed branch: %+v", page)
		}
		all = append(all, page.Hits...)
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	wantAll, _ := referenceTxHits(refs.newChain, 1, 4, nil)
	if !reflect.DeepEqual(all, wantAll) {
		t.Fatalf("new-chain pagination=%v\nwant %v", all, wantAll)
	}
}

// Continuation order 1: the continuation first observes the complete old
// chain (parked holding the lock, reorg waits): it succeeds with the old
// next page and the SAME pinned stats and end height as the first page.
// After the reorg commits, reusing the old cursor fails ErrQueryChanged.
//
// Continuation order 2: the reorg commits first: the continuation fails
// ErrQueryChanged with a zero page and no next cursor.
func TestQueryTxsContinuationAcrossInRangeReorg(t *testing.T) {
	wantSecond := txPageExpect{
		hits: []TxHit{
			{Height: 2, BlockHash: "qh2", TxID: "c", Position: 1},
			{Height: 3, BlockHash: "qh3", TxID: "a", Position: 0},
			{Height: 3, BlockHash: "qh3", TxID: "c", Position: 1},
			{Height: 4, BlockHash: "qh4", TxID: "d", Position: 0},
		},
		total: 14, blocks: 5, to: 5, hasMore: true,
	}

	t.Run("continuation observes old chain first", func(t *testing.T) {
		index := buildQueryReorgChain(t)
		refs := queryReorgReferences()
		first, err := index.QueryTxs(queryReorgFirstQuery)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(coreOf(first), literalOldFirstPage()) {
			t.Fatalf("setup first page=%+v", coreOf(first))
		}

		h := installQueryReorgHooks(t)
		pageCh := make(chan TxPage, 1)
		errCh := make(chan error, 1)
		go func() {
			page, qerr := index.QueryTxs(TxQuery{PageSize: 4, Cursor: first.NextCursor})
			if qerr != nil {
				errCh <- qerr
				return
			}
			pageCh <- page
		}()
		<-h.queryParked // continuation verified the range and holds the lock

		reorgDone := make(chan struct{})
		go func() {
			if _, rerr := index.Reorg(queryReorgNewBranch()); rerr != nil {
				errCh <- rerr
			}
			close(reorgDone)
		}()
		close(h.releaseQuery) // continuation scans and finishes first

		var got TxPage
		select {
		case got = <-pageCh:
		case err := <-errCh:
			t.Fatalf("concurrent call failed: %v", err)
		}
		oldE, oldOK := oracleExpect(refs.oldChain, 1, 5, nil, 4, 4)
		if err := classifySuccessfulPage(coreOf(got), oldE, oldOK, txPageExpect{}, false); err != nil {
			t.Fatalf("continuation did not return the old chain's next page:\n%v", err)
		}
		if !reflect.DeepEqual(coreOf(got), wantSecond) {
			t.Fatalf("old next page=%+v\nwant %+v", coreOf(got), wantSecond)
		}
		// Stats and the pinned end must be identical to the first page.
		if got.TotalMatches != first.TotalMatches || got.MatchedBlocks != first.MatchedBlocks || got.ToHeight != first.ToHeight {
			t.Fatalf("continuation stats=%d/%d/%d, first page=%d/%d/%d",
				got.TotalMatches, got.MatchedBlocks, got.ToHeight,
				first.TotalMatches, first.MatchedBlocks, first.ToHeight)
		}

		close(h.releaseReorg)
		<-reorgDone

		// Once the reorg has committed, every old cursor — including the one
		// just used successfully — must fail with a zeroed result.
		for _, cursor := range []string{first.NextCursor, got.NextCursor} {
			failed, err := index.QueryTxs(TxQuery{PageSize: 4, Cursor: cursor})
			if !errors.Is(err, ErrQueryChanged) {
				t.Fatalf("old cursor after reorg err=%v, want ErrQueryChanged", err)
			}
			if !reflect.DeepEqual(failed, TxPage{}) {
				t.Fatalf("ErrQueryChanged carried data: %+v", failed)
			}
		}
	})

	t.Run("reorg commits before continuation", func(t *testing.T) {
		index := buildQueryReorgChain(t)
		first, err := index.QueryTxs(queryReorgFirstQuery)
		if err != nil {
			t.Fatal(err)
		}

		h := installQueryReorgHooks(t)
		reorgErr := make(chan error, 1)
		go func() {
			_, rerr := index.Reorg(queryReorgNewBranch())
			reorgErr <- rerr
		}()
		<-h.reorgParked // replacement complete, tip 4, lock still held

		pageCh := make(chan TxPage, 1)
		errCh := make(chan error, 1)
		go func() {
			page, qerr := index.QueryTxs(TxQuery{PageSize: 4, Cursor: first.NextCursor})
			if qerr != nil {
				errCh <- qerr
				return
			}
			pageCh <- page
		}()
		close(h.releaseReorg)
		if err := <-reorgErr; err != nil {
			t.Fatalf("reorg failed: %v", err)
		}
		close(h.releaseQuery)

		select {
		case bad := <-pageCh:
			t.Fatalf("continuation after shortening returned records: %+v", bad)
		case err := <-errCh:
			if !errors.Is(err, ErrQueryChanged) {
				t.Fatalf("continuation err=%v, want ErrQueryChanged", err)
			}
		}

		// A fresh first-page query starts over on the new chain.
		fresh, err := index.QueryTxs(queryReorgFirstQuery)
		if err != nil {
			t.Fatalf("fresh query after reorg failed: %v", err)
		}
		if !reflect.DeepEqual(coreOf(fresh), literalNewFirstPage()) {
			t.Fatalf("fresh page=%+v, want complete new chain", coreOf(fresh))
		}
	})
}

// An explicit To pins the requested end height as well: a reorg shortening
// the chain below that explicit end invalidates the cursor even though the
// first page never observed a live tip.
func TestQueryTxsExplicitEndCursorDiesWhenChainShortensBelow(t *testing.T) {
	index := buildQueryReorgChain(t)
	first, err := index.QueryTxs(TxQuery{From: 1, To: 5, PageSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	if first.ToHeight != 5 || first.TotalMatches != 14 || first.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", first)
	}
	if _, err := index.Reorg(queryReorgNewBranch()); err != nil {
		t.Fatal(err)
	}
	failed, err := index.QueryTxs(TxQuery{From: 1, To: 5, PageSize: 4, Cursor: first.NextCursor})
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("err=%v, want ErrQueryChanged (tip below explicit end)", err)
	}
	if !reflect.DeepEqual(failed, TxPage{}) {
		t.Fatalf("ErrQueryChanged carried data: %+v", failed)
	}
}

// Fixed-range boundary: the reorg only replaces a suffix OUTSIDE the pinned
// range [1,3] and the new chain still covers height 3. The parked
// continuation must succeed concurrently, and the cursor must keep working
// after the reorg returns — no missing or duplicated remaining hits, and no
// out-of-range transaction (new or old) ever enters the result.
func TestQueryTxsCursorSurvivesConcurrentReorgOutsideFixedRange(t *testing.T) {
	index := buildQueryReorgChain(t)
	const from, to = int64(1), int64(3)
	first, err := index.QueryTxs(TxQuery{From: from, To: to, PageSize: 4})
	if err != nil {
		t.Fatal(err)
	}
	wantSecond := txPageExpect{
		hits: []TxHit{
			{Height: 2, BlockHash: "qh2", TxID: "c", Position: 1},
			{Height: 3, BlockHash: "qh3", TxID: "a", Position: 0},
			{Height: 3, BlockHash: "qh3", TxID: "c", Position: 1},
		},
		total: 7, blocks: 3, to: 3, hasMore: false,
	}
	if core := coreOf(first); !reflect.DeepEqual(core, txPageExpect{
		hits: []TxHit{
			{Height: 1, BlockHash: "qh1", TxID: "a", Position: 0},
			{Height: 1, BlockHash: "qh1", TxID: "x", Position: 1},
			{Height: 1, BlockHash: "qh1", TxID: "a", Position: 2},
			{Height: 2, BlockHash: "qh2", TxID: "b", Position: 0},
		},
		total: 7, blocks: 3, to: 3, hasMore: true,
	}) {
		t.Fatalf("setup first page=%+v", core)
	}

	h := installQueryReorgHooks(t)
	pageCh := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, qerr := index.QueryTxs(TxQuery{From: from, To: to, PageSize: 4, Cursor: first.NextCursor})
		if qerr != nil {
			errCh <- qerr
			return
		}
		pageCh <- page
	}()
	<-h.queryParked // continuation holds the lock with the range verified

	// Suffix-only reorg rooted at retained h3: replaces h4 and drops h5; the
	// fixed end height 3 and everything below it are untouched.
	suffix := []Block{{Height: 4, Hash: "qk4", Parent: "qh3", Txs: []string{"z"}}}
	reorgDone := make(chan struct{})
	go func() {
		dropped, rerr := index.Reorg(suffix)
		if rerr != nil {
			errCh <- rerr
			return
		}
		if !reflect.DeepEqual(dropped, []int64{4, 5}) {
			errCh <- fmt.Errorf("dropped=%v, want [4 5]", dropped)
		}
		close(reorgDone)
	}()
	close(h.releaseQuery) // continuation finishes while the reorg waits

	var got TxPage
	select {
	case got = <-pageCh:
	case err := <-errCh:
		t.Fatalf("concurrent call failed: %v", err)
	}
	if !reflect.DeepEqual(coreOf(got), wantSecond) {
		t.Fatalf("continuation across out-of-range reorg=%+v\nwant %+v", coreOf(got), wantSecond)
	}
	close(h.releaseReorg)
	<-reorgDone

	// After the reorg commits, the same cursor still returns the same page;
	// following it yields no further records and never repeats any.
	again, err := index.QueryTxs(TxQuery{From: from, To: to, PageSize: 4, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("cursor invalidated by an out-of-range reorg: %v", err)
	}
	if !reflect.DeepEqual(again, got) {
		t.Fatalf("same cursor returned different pages:\n%+v\n%+v", again, got)
	}
	if again.NextCursor != "" {
		t.Fatalf("pinned range exhausted but cursor offered another page: %q", again.NextCursor)
	}

	// Full walk from the first cursor: exactly the seven in-range occurrences,
	// nothing from the removed h5 or the replacement block qk4 (tx z).
	query := TxQuery{From: from, To: to, PageSize: 1, Cursor: first.NextCursor}
	var rest []TxHit
	for {
		page, perr := index.QueryTxs(query)
		if perr != nil {
			t.Fatalf("pinned continuation after reorg failed: %v", perr)
		}
		if page.TotalMatches != 7 || page.MatchedBlocks != 3 || page.ToHeight != 3 {
			t.Fatalf("pinned stats changed: %+v", page)
		}
		rest = append(rest, page.Hits...)
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	wantRest := []TxHit{
		{Height: 2, BlockHash: "qh2", TxID: "c", Position: 1},
		{Height: 3, BlockHash: "qh3", TxID: "a", Position: 0},
		{Height: 3, BlockHash: "qh3", TxID: "c", Position: 1},
	}
	if !reflect.DeepEqual(rest, wantRest) {
		t.Fatalf("remaining pinned hits=%v\nwant %v", rest, wantRest)
	}

	// Out-of-range content really is out of range for this cursor's query.
	whole, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var foundZ, foundRemoved bool
	for _, hit := range whole.Hits {
		if hit.TxID == "z" && hit.BlockHash == "qk4" {
			foundZ = true
		}
		if hit.Height == 5 || hit.BlockHash == "qh5" || hit.TxID == "h" {
			foundRemoved = true
		}
	}
	if !foundZ {
		t.Fatal("replacement block qk4/tx z must be visible to an unpinned query")
	}
	if foundRemoved {
		t.Fatal("removed suffix content (h5/tx h) survived the reorg")
	}
}

// Sustained overlap: reorgs alternate between the old tip-5 branch and the
// shortened tip-4 branch while readers continuously start queries and walk
// cursors. Every successful page must be the complete page of one chain; a
// changed pinned range must surface as ErrQueryChanged with a zero page.
// Deterministic and timing-independent — a torn or stitched page fails
// regardless of scheduling.
func TestQueryTxsRepeatedReorgsStayOnOneCompleteChain(t *testing.T) {
	index := buildQueryReorgChain(t)
	refs := queryReorgReferences()
	oldBranch := queryReorgOldChain()[1:] // h2..h5 rooted at qh1, restores tip 5

	const reorgers = 2
	const rounds = 40
	const readers = 4
	const reads = 80

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
				branch := queryReorgNewBranch()
				if (i+seed)%2 == 0 {
					branch = oldBranch
				}
				if _, err := index.Reorg(branch); err != nil {
					t.Errorf("reorg failed: %v", err)
					return
				}
			}
		}(r)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			for i := 0; i < reads; i++ {
				select {
				case <-stop:
					return
				default:
				}
				query := TxQuery{PageSize: 2 + (i+reader)%3} // 2..4, varies per round
				first, err := index.QueryTxs(query)
				if err != nil {
					t.Errorf("first page failed: %v", err)
					return
				}
				if err := classifyFirstPage(first, query, refs); err != nil {
					t.Errorf("%v", err)
					return
				}

				pinnedTo := first.ToHeight
				total := first.TotalMatches
				seen := map[string]bool{}
				lastHeight, lastPosition := int64(0), -1
				counted := len(first.Hits)
				for _, hit := range first.Hits {
					if hit.Height < lastHeight || (hit.Height == lastHeight && hit.Position <= lastPosition) {
						t.Errorf("first page hits out of order: %+v", first.Hits)
						return
					}
					lastHeight, lastPosition = hit.Height, hit.Position
					seen[fmt.Sprintf("%d:%d", hit.Height, hit.Position)] = true
				}

				query.Cursor = first.NextCursor
				finished := first.NextCursor == ""
				for query.Cursor != "" {
					off := counted
					page, qerr := index.QueryTxs(query)
					if errors.Is(qerr, ErrQueryChanged) {
						if !reflect.DeepEqual(page, TxPage{}) {
							t.Errorf("ErrQueryChanged carried data: %+v", page)
						}
						break
					}
					if qerr != nil {
						t.Errorf("continuation failed: %v", qerr)
						return
					}
					oldE, oldOK := oracleExpect(refs.oldChain, 1, pinnedTo, nil, int64(off), query.PageSize)
					newE, newOK := oracleExpect(refs.newChain, 1, pinnedTo, nil, int64(off), query.PageSize)
					if err := classifySuccessfulPage(coreOf(page), oldE, oldOK, newE, newOK); err != nil {
						t.Errorf("%v", err)
						return
					}
					if page.TotalMatches != total || page.ToHeight != pinnedTo {
						t.Errorf("pinned stats changed mid-walk: page=%+v first=%+v", page, first)
						return
					}
					for _, hit := range page.Hits {
						if hit.Height < lastHeight || (hit.Height == lastHeight && hit.Position <= lastPosition) {
							t.Errorf("hits out of order across page boundary: %+v", page.Hits)
							return
						}
						if hit.Height > pinnedTo {
							t.Errorf("hit above pinned end %d: %+v", pinnedTo, hit)
							return
						}
						key := fmt.Sprintf("%d:%d", hit.Height, hit.Position)
						if seen[key] {
							t.Errorf("occurrence %s paginated twice", key)
							return
						}
						seen[key] = true
						lastHeight, lastPosition = hit.Height, hit.Position
					}
					counted += len(page.Hits)
					query.Cursor = page.NextCursor
					finished = page.NextCursor == ""
				}
				// A walk that reached its own end without ErrQueryChanged must
				// have delivered every occurrence exactly once.
				if finished && int64(counted) != total {
					t.Errorf("paginated %d hits, pinned total says %d", counted, total)
					return
				}
			}
		}(r)
	}
	wg.Wait()
	close(stop)

	// Leave the index deterministically on the new chain and verify a full
	// walk against the complete branch records.
	if _, err := index.Reorg(queryReorgNewBranch()); err != nil {
		t.Fatalf("final reorg failed: %v", err)
	}
	pages := collectPages(t, index, queryReorgFirstQuery)
	var all []TxHit
	for _, page := range pages {
		if page.TotalMatches != 11 || page.MatchedBlocks != 4 || page.ToHeight != 4 {
			t.Fatalf("final walk stats changed: %+v", page)
		}
		all = append(all, page.Hits...)
	}
	wantAll, _ := referenceTxHits(refs.newChain, 1, 4, nil)
	if !reflect.DeepEqual(all, wantAll) {
		t.Fatalf("final walk=%v\nwant %v", all, wantAll)
	}
}
