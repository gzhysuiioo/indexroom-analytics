package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// This file is the OrderDesc twin of query_reorg_test.go. The existing
// concurrent shortening-reorg regression drives QueryTxs in the default
// ascending direction; here a descending, tip-bound, filtered first page is
// forced to overlap a reorg that makes the chain shorter, and every page must
// still answer from ONE complete main chain: its visible hits (height, block
// hash, original position), the whole-range statistics, and the pinned upper
// height must all describe the same branch.
//
// Old chain, tip 4 (heights 1..4 consecutive):
//
//	h1 parent g  txs [a, b, a]
//	o2 parent h1 txs [a, c]
//	o3 parent o2 txs [b, a]
//	o4 parent o3 txs [a, a, b]
//
// New legal branch, tip 3: h1 is retained; heights 2..3 are replaced:
//
//	h1 parent g  txs [a, b, a]
//	n2 parent h1 txs [a, a]
//	n3 parent n2 txs [c, a, a]
//
// The query filters "a" only, starts at height 1, leaves To at zero (the
// first page pins the tip it sees), page size 3, empty cursor, OrderDesc.
//
// Old-chain first page (range 1..4, descending positions kept):
//
//	(4,1,o4,a), (4,0,o4,a), (3,1,o3,a)
//
// Whole range: 6 occurrences of a across 4 blocks, real upper bound 4, and a
// non-empty cursor (three more occurrences remain). The two a's inside o4 are
// separate occurrences at their original positions 1 and 0.
//
// New-chain first page (range 1..3):
//
//	(3,2,n3,a), (3,1,n3,a), (2,1,n2,a)
//
// Whole range: still 6 occurrences, but only 3 blocks hold an a, the real
// upper bound is 3, and a cursor remains. No hash of the removed height 4 or
// the replaced blocks (o2, o3, o4) may appear on this page.

func descOldChainBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"a", "b", "a"}},
		{Height: 2, Hash: "o2", Parent: "h1", Txs: []string{"a", "c"}},
		{Height: 3, Hash: "o3", Parent: "o2", Txs: []string{"b", "a"}},
		{Height: 4, Hash: "o4", Parent: "o3", Txs: []string{"a", "a", "b"}},
	}
}

func descNewBranchBlocks() []Block {
	return []Block{
		{Height: 2, Hash: "n2", Parent: "h1", Txs: []string{"a", "a"}},
		{Height: 3, Hash: "n3", Parent: "n2", Txs: []string{"c", "a", "a"}},
	}
}

func descNewChainBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"a", "b", "a"}},
		{Height: 2, Hash: "n2", Parent: "h1", Txs: []string{"a", "a"}},
		{Height: 3, Hash: "n3", Parent: "n2", Txs: []string{"c", "a", "a"}},
	}
}

func buildDescOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range descOldChainBlocks() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

// descReferenceHits is an independent descending oracle built straight from
// an explicit chain, not from the Index under test: decreasing height, and
// inside one block decreasing original position, "a" occurrences only.
func descReferenceHits(chain []Block) []TxHit {
	hits := []TxHit{}
	for i := len(chain) - 1; i >= 0; i-- {
		block := chain[i]
		for position := len(block.Txs) - 1; position >= 0; position-- {
			if block.Txs[position] != "a" {
				continue
			}
			hits = append(hits, TxHit{
				Height:    block.Height,
				BlockHash: block.Hash,
				TxID:      "a",
				Position:  position,
			})
		}
	}
	return hits
}

func descReferenceMatchedBlocks(chain []Block) int64 {
	var blocks int64
	for _, block := range chain {
		for _, tx := range block.Txs {
			if tx == "a" {
				blocks++
				break
			}
		}
	}
	return blocks
}

// descReferencePage slices one page at the given offset of the oracle's full
// descending occurrence list and attaches the statistics over the WHOLE
// filtered range, not just the page. The cursor stays opaque and is checked
// separately.
func descReferencePage(chain []Block, pageSize int, offset int64) TxPage {
	full := descReferenceHits(chain)
	rest := full[offset:]
	end := pageSize
	if end > len(rest) {
		end = len(rest)
	}
	return TxPage{
		Hits:          append([]TxHit{}, rest[:end]...),
		TotalMatches:  int64(len(full)),
		MatchedBlocks: descReferenceMatchedBlocks(chain),
		ToHeight:      chain[len(chain)-1].Height,
	}
}

// Literal first pages, as derived by hand in the brief: the guarantee rests on
// these exact records, not on an oracle-vs-oracle DeepEqual.
func literalDescOldFirstPage() TxPage {
	return TxPage{
		Hits: []TxHit{
			{Height: 4, BlockHash: "o4", TxID: "a", Position: 1},
			{Height: 4, BlockHash: "o4", TxID: "a", Position: 0},
			{Height: 3, BlockHash: "o3", TxID: "a", Position: 1},
		},
		TotalMatches:  6,
		MatchedBlocks: 4,
		ToHeight:      4,
	}
}

func literalDescNewFirstPage() TxPage {
	return TxPage{
		Hits: []TxHit{
			{Height: 3, BlockHash: "n3", TxID: "a", Position: 2},
			{Height: 3, BlockHash: "n3", TxID: "a", Position: 1},
			{Height: 2, BlockHash: "n2", TxID: "a", Position: 1},
		},
		TotalMatches:  6,
		MatchedBlocks: 3,
		ToHeight:      3,
	}
}

func requireDescOrdered(t *testing.T, hits []TxHit) {
	t.Helper()
	for i := 1; i < len(hits); i++ {
		prev, cur := hits[i-1], hits[i]
		if cur.Height > prev.Height || (cur.Height == prev.Height && cur.Position >= prev.Position) {
			t.Fatalf("hits not descending at %d: %+v after %+v", i, cur, prev)
		}
	}
}

// The counting and ordering semantics the regression rests on, asserted on
// both chains and cross-checked against the live index before and after the
// shortening reorg.
func TestReorgDescQueryFixtureSemanticsOnBothChains(t *testing.T) {
	oldChain := descOldChainBlocks()
	newChain := descNewChainBlocks()

	// Setup invariant: every suffix hash that vanishes must be distinct from
	// every hash the new branch carries, or a torn page could hide itself.
	newHashes := map[string]bool{}
	for _, b := range newChain[1:] {
		newHashes[b.Hash] = true
	}
	for _, gone := range []string{"o2", "o3", "o4"} {
		if newHashes[gone] {
			t.Fatalf("setup invariant broken: removed hash %q is present on the new branch", gone)
		}
	}

	oldHits := descReferenceHits(oldChain)
	if want := literalDescOldFirstPage().Hits; !reflect.DeepEqual(oldHits[:3], want) {
		t.Fatalf("old chain first three=%v, want literal %v", oldHits[:3], want)
	}
	if len(oldHits) != 6 || descReferenceMatchedBlocks(oldChain) != 4 {
		t.Fatalf("old chain: matches=%d blocks=%d, want 6/4", len(oldHits), descReferenceMatchedBlocks(oldChain))
	}
	// All six occurrences, including the repeated a's inside one block.
	wantOldAll := []TxHit{
		{Height: 4, BlockHash: "o4", TxID: "a", Position: 1},
		{Height: 4, BlockHash: "o4", TxID: "a", Position: 0},
		{Height: 3, BlockHash: "o3", TxID: "a", Position: 1},
		{Height: 2, BlockHash: "o2", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 2},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(oldHits, wantOldAll) {
		t.Fatalf("old chain full desc walk=%v, want %v", oldHits, wantOldAll)
	}
	requireDescOrdered(t, oldHits)

	newHits := descReferenceHits(newChain)
	if want := literalDescNewFirstPage().Hits; !reflect.DeepEqual(newHits[:3], want) {
		t.Fatalf("new chain first three=%v, want literal %v", newHits[:3], want)
	}
	if len(newHits) != 6 || descReferenceMatchedBlocks(newChain) != 3 {
		t.Fatalf("new chain: matches=%d blocks=%d, want 6/3", len(newHits), descReferenceMatchedBlocks(newChain))
	}
	requireDescOrdered(t, newHits)
	for _, h := range newHits {
		if h.BlockHash == "o2" || h.BlockHash == "o3" || h.BlockHash == "o4" {
			t.Fatalf("removed/replaced hash %q must not occur on the new chain: %v", h.BlockHash, newHits)
		}
		if h.Height > 3 {
			t.Fatalf("old height %d must not occur on the shortened chain", h.Height)
		}
	}

	// The oracle must match the live index on both complete chain states.
	index := buildDescOldChain(t)
	query := TxQuery{TxIDs: []string{"a"}, PageSize: 3, Order: OrderDesc}
	live, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(live), viewOf(descReferencePage(oldChain, 3, 0))) {
		t.Fatalf("live old-chain desc page=%+v", live)
	}
	dropped, err := index.Reorg(descNewBranchBlocks())
	if err != nil {
		t.Fatalf("shortening reorg refused: %v", err)
	}
	if want := []int64{2, 3, 4}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped=%v, want %v", dropped, want)
	}
	if index.Tip != 3 {
		t.Fatalf("tip after reorg=%d, want 3", index.Tip)
	}
	if _, ok := index.Blocks[4]; ok {
		t.Fatal("height 4 still present after shortening reorg")
	}
	if _, ok := index.ByHash["o4"]; ok {
		t.Fatal("removed hash o4 still indexed")
	}
	live, err = index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(live), viewOf(descReferencePage(newChain, 3, 0))) {
		t.Fatalf("live new-chain desc page=%+v", live)
	}
}

// classifyDescTxPage accepts got only when its visible contents are the
// filtered first-page view of exactly one chain. The opaque cursor is not part
// of the view; callers validate it by following it. Provenance is checked
// against the COMPLETE filtered occurrence lists of both branches, so a
// stitched page is exposed even when its per-field counts look plausible, and
// the descending order is verified independently.
func classifyDescTxPage(got, oldRef, newRef TxPage, oldAllHits, newAllHits []TxHit) error {
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
	fmt.Fprintln(&report, "desc QueryTxs page does not match one complete main chain:")
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

	// Record-level provenance: every hit must be explainable as the same
	// (height, hash, position) occurrence on one branch.
	oldByKey := map[TxHit]bool{}
	newByKey := map[TxHit]bool{}
	for _, h := range oldAllHits {
		oldByKey[h] = true
	}
	for _, h := range newAllHits {
		newByKey[h] = true
	}
	// Each branch's hash at a height, so a record that borrows one branch's
	// position under the other's hash counts as on-neither, not as "shared".
	hashAt := func(chain []Block) map[int64]string {
		m := map[int64]string{}
		for _, b := range chain {
			m[b.Height] = b.Hash
		}
		return m
	}
	oldHashAt := hashAt(descOldChainBlocks())
	newHashAt := hashAt(descNewChainBlocks())
	var inOld, inNew, inNeither int
	var onlyOld, onlyNew int
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
			if h.Height > prev.Height || (h.Height == prev.Height && h.Position >= prev.Position) {
				ordered = false
			}
		}
		if h.Height > g.ToHeight {
			fmt.Fprintf(&report, "  hit above pinned ToHeight %d: %+v\n", g.ToHeight, h)
		}
		if h.TxID != "a" {
			fmt.Fprintf(&report, "  non-matching transaction leaked through the filter: %+v\n", h)
		}
		if hash, ok := oldHashAt[h.Height]; ok && h.BlockHash != hash {
			if hash2, ok2 := newHashAt[h.Height]; ok2 && h.BlockHash != hash2 {
				fmt.Fprintf(&report, "  hit hash %q matches neither branch at height %d\n", h.BlockHash, h.Height)
			}
		}
	}
	fmt.Fprintf(&report, "  Hits provenance  on-old=%d on-new=%d only-old=%d only-new=%d on-neither=%d (desc-ordered=%v)\n",
		inOld, inNew, onlyOld, onlyNew, inNeither, ordered)
	if onlyOld > 0 {
		tags["old"] = true
	}
	if onlyNew > 0 {
		tags["new"] = true
	}
	switch {
	case tags["old"] && tags["new"]:
		fmt.Fprintf(&report, "  => MIXED: desc hits and statistics come from both the old and the new chain; one page must reflect a single main chain")
	case inNeither > 0:
		fmt.Fprintf(&report, "  => CORRUPT: records exist on neither chain")
	case !ordered:
		fmt.Fprintf(&report, "  => CORRUPT: page is not in descending height/position order")
	default:
		fmt.Fprintf(&report, "  => CORRUPT: matches neither the old nor the new chain")
	}
	return errors.New(report.String())
}

// The classifier must accept both complete descending answers.
func TestClassifyDescTxPageAcceptsBothChains(t *testing.T) {
	oldChain, newChain := descOldChainBlocks(), descNewChainBlocks()
	oldRef := descReferencePage(oldChain, 3, 0)
	newRef := descReferencePage(newChain, 3, 0)
	oldAll, newAll := descReferenceHits(oldChain), descReferenceHits(newChain)
	if err := classifyDescTxPage(oldRef, oldRef, newRef, oldAll, newAll); err != nil {
		t.Fatalf("pure old desc page rejected: %v", err)
	}
	if err := classifyDescTxPage(newRef, oldRef, newRef, oldAll, newAll); err != nil {
		t.Fatalf("pure new desc page rejected: %v", err)
	}

	// Old-chain stats (4 blocks, upper bound 4) pinned onto new-chain records
	// is a torn answer even though both chains have six matches: this is the
	// exact mistake "do not stitch old hits to new stats" forbids.
	mixed := newRef
	mixed.MatchedBlocks = oldRef.MatchedBlocks
	mixed.ToHeight = oldRef.ToHeight
	if err := classifyDescTxPage(mixed, oldRef, newRef, oldAll, newAll); err == nil {
		t.Fatal("new-chain desc hits with old-chain stats were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("mixed page not reported as MIXED:\n%v", err)
	}

	// One removed old-chain record on a page otherwise pinned to the new tip.
	stale := newRef
	stale.Hits = append([]TxHit{}, newRef.Hits[:2]...)
	stale.Hits = append(stale.Hits, TxHit{Height: 4, BlockHash: "o4", TxID: "a", Position: 0})
	if err := classifyDescTxPage(stale, oldRef, newRef, oldAll, newAll); err == nil {
		t.Fatal("page carrying removed o4 was accepted")
	}
}

// Ordering 1: the descending first page is holding the lock on the complete
// OLD chain (tip 4 already pinned) when the shortening reorg starts. It must
// finish on the old chain — literal o4/o4/o3 records, statistics 6/4, upper
// bound 4, non-empty cursor — and never report ErrInvalidArgument or
// ErrQueryChanged for a valid first page. Once the reorg commits, its cursor
// is dead with a zeroed ErrQueryChanged.
func TestQueryTxsDescFirstPageInFlightObservesCompleteOldChain(t *testing.T) {
	index := buildDescOldChain(t)
	h := installQueryReorgHooks(t, true, false, 3)
	oldChain, newChain := descOldChainBlocks(), descNewChainBlocks()

	result := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 3, Order: OrderDesc})
		if err != nil {
			errCh <- err
			return
		}
		result <- page
	}()

	<-h.firstParked // query holds index.mu and has pinned the old tip 4
	reorgDone := make(chan struct{})
	go func() {
		if _, err := index.Reorg(descNewBranchBlocks()); err != nil {
			errCh <- err
		}
		close(reorgDone)
	}()

	close(h.releaseFirst) // the desc scan finishes against the complete old chain
	var got TxPage
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("concurrent desc first page failed: %v", err)
	}
	want := descReferencePage(oldChain, 3, 0)
	if err := classifyDescTxPage(got, want, descReferencePage(newChain, 3, 0),
		descReferenceHits(oldChain), descReferenceHits(newChain)); err != nil {
		t.Fatalf("in-flight desc first page did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Fatalf("desc query holding the lock across reorg start must see the old chain:\n%+v", got)
	}
	if !reflect.DeepEqual(got.Hits, literalDescOldFirstPage().Hits) {
		t.Fatalf("old desc first page hits=%v, want literal o4/o4/o3 records", got.Hits)
	}
	if got.NextCursor == "" {
		t.Fatal("old desc first page over 6 matches (page size 3) must carry a cursor")
	}

	close(h.releaseReorg) // let the parked reorg commit and return
	<-reorgDone
	if index.Tip != 3 {
		t.Fatalf("tip after reorg=%d, want 3", index.Tip)
	}

	// The cursor pins the OLD branch: range to 4, offset 3, descending order,
	// old fingerprint. On the shortened chain it must fail, not stitch.
	oldFingerprint := buildDescOldChain(t)
	payload := decodePayload(t, index, got.NextCursor)
	if payload.To != 4 || payload.Off != 3 || payload.Order != OrderDesc {
		t.Fatalf("cursor pins to=%d off=%d order=%d, want 4/3/OrderDesc", payload.To, payload.Off, payload.Order)
	}
	if payload.FP != fmt.Sprintf("%x", oldFingerprint.hashRangeBlockContent(1, 4)) {
		t.Fatal("cursor fingerprint does not match the complete old chain")
	}
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 3, Order: OrderDesc, Cursor: got.NextCursor})
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("old desc cursor across reorg: err=%v page=%+v, want ErrQueryChanged", err, page)
	}
	if errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a committed reorg must not be reported as an argument error: %v", err)
	}
	if !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("changed desc continuation must return a zeroed page, got %+v", page)
	}
}

// Ordering 2: the shortening reorg has fully committed (new chain installed,
// Reorg still holding the lock) when the descending first page starts. It
// waits, then answers on the complete NEW chain — literal n3/n3/n2 records,
// statistics 6/3, upper bound 3 — with no removed or replaced hash anywhere on
// the page, and a valid first-page request that never fails with
// ErrInvalidArgument or ErrQueryChanged. Its cursor paginates the new chain to
// the end.
func TestQueryTxsDescFirstPageDuringCommittedReorgObservesCompleteNewChain(t *testing.T) {
	index := buildDescOldChain(t)
	h := installQueryReorgHooks(t, true, false, 3)
	oldChain, newChain := descOldChainBlocks(), descNewChainBlocks()

	reorgErr := make(chan error, 1)
	go func() {
		_, err := index.Reorg(descNewBranchBlocks())
		reorgErr <- err
	}()
	<-h.reorgParked // new branch applied (tip 3), Reorg still holds the lock

	result := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 3, Order: OrderDesc})
		if err != nil {
			errCh <- err
			return
		}
		result <- page
	}()

	close(h.releaseReorg) // reorg returns; the first lock the query can get sees the new chain
	if err := <-reorgErr; err != nil {
		t.Fatalf("reorg failed: %v", err)
	}
	close(h.releaseFirst) // the parked first page can now scan and finish

	var got TxPage
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("desc first page during committed reorg must not fail: %v", err)
	}
	want := descReferencePage(newChain, 3, 0)
	if err := classifyDescTxPage(got, descReferencePage(oldChain, 3, 0), want,
		descReferenceHits(oldChain), descReferenceHits(newChain)); err != nil {
		t.Fatalf("desc first page across reorg commit did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Fatalf("desc query after reorg commit must see the complete new chain:\n%+v", got)
	}
	if !reflect.DeepEqual(got.Hits, literalDescNewFirstPage().Hits) {
		t.Fatalf("new desc first page hits=%v, want literal n3/n3/n2 records", got.Hits)
	}
	if got.NextCursor == "" {
		t.Fatal("new desc first page over 6 matches (page size 3) must carry a cursor")
	}
	for _, hit := range got.Hits {
		if hit.Height > got.ToHeight {
			t.Fatalf("hit above the pinned bound: %+v", hit)
		}
		if hit.BlockHash == "o2" || hit.BlockHash == "o3" || hit.BlockHash == "o4" {
			t.Fatalf("removed/replaced hash %q leaked into the new-chain page", hit.BlockHash)
		}
	}

	// The cursor pins the NEW branch and drains it exactly: three more a's,
	// statistics fixed at 6 matches over 3 blocks and upper bound 3.
	newFingerprint := New()
	for _, b := range newChain {
		if err := newFingerprint.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	payload := decodePayload(t, index, got.NextCursor)
	if payload.To != 3 || payload.Off != 3 || payload.Order != OrderDesc {
		t.Fatalf("cursor pins to=%d off=%d order=%d, want 3/3/OrderDesc", payload.To, payload.Off, payload.Order)
	}
	if payload.FP != fmt.Sprintf("%x", newFingerprint.hashRangeBlockContent(1, 3)) {
		t.Fatal("cursor fingerprint does not match the complete new chain")
	}

	second, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 3, Order: OrderDesc, Cursor: got.NextCursor})
	if err != nil {
		t.Fatalf("new-chain desc continuation failed: %v", err)
	}
	wantSecond := descReferencePage(newChain, 3, 3)
	if !reflect.DeepEqual(viewOf(second), viewOf(wantSecond)) {
		t.Fatalf("new-chain desc second page=%+v, want %+v", second, wantSecond)
	}
	if second.NextCursor != "" {
		t.Fatalf("six matches over two pages of three: cursor must end, got %q", second.NextCursor)
	}
	all := append(append([]TxHit{}, got.Hits...), second.Hits...)
	if !reflect.DeepEqual(all, descReferenceHits(newChain)) {
		t.Fatalf("new-chain desc pagination=%v, want %v", all, descReferenceHits(newChain))
	}
}
