package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This file is the OrderDesc counterpart of query_reorg_test.go. The existing
// shortening-reorg regression drives QueryTxs in the default ascending order;
// these tests pin the same "one complete main chain per first page" guarantee
// while a first page walks the tip-bound range DOWNWARD, with a filter that
// keeps repeated occurrences and statistics that must count the WHOLE range
// rather than the page.
//
// Fixture:
//
//	Old chain, tip 4:
//	  h1 parent g  txs [a, b, a] // height 1, retained by the new branch
//	  o2 parent h1 txs [a, c]
//	  o3 parent o2 txs [b, a]
//	  o4 parent o3 txs [a, a, b]
//	New shorter branch, tip 3 (keeps h1, replaces heights 2..4):
//	  n2 parent h1 txs [a, a]
//	  n3 parent n2 txs [c, a, a]
//
// Descending first page, From=1, To left at zero, TxIDs={"a"}, page size 3:
//
//	Old chain (range 1..4): (h4 o4 pos1), (h4 o4 pos0), (h3 o3 pos1);
//	  6 matches in 4 blocks, effective upper bound 4, cursor non-empty.
//	New chain (range 1..3): (h3 n3 pos2), (h3 n3 pos1), (h2 n2 pos1);
//	  6 matches in 3 blocks, effective upper bound 3, cursor non-empty.
//
// The total match count is deliberately 6 on BOTH branches: only the matched
// block count (4 vs 3), the effective upper bound (4 vs 3), and the records
// distinguish the two complete answers, so a page that stitches old-chain hits
// onto new-chain statistics cannot hide behind equal totals. Positions stay
// zero-based block positions (the two a's inside o4 arrive as 1 then 0) and
// every repeated a is a separate occurrence.

func descReorgOldChainBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"a", "b", "a"}},
		{Height: 2, Hash: "o2", Parent: "h1", Txs: []string{"a", "c"}},
		{Height: 3, Hash: "o3", Parent: "o2", Txs: []string{"b", "a"}},
		{Height: 4, Hash: "o4", Parent: "o3", Txs: []string{"a", "a", "b"}},
	}
}

func descReorgNewBranchBlocks() []Block {
	return []Block{
		{Height: 2, Hash: "n2", Parent: "h1", Txs: []string{"a", "a"}},
		{Height: 3, Hash: "n3", Parent: "n2", Txs: []string{"c", "a", "a"}},
	}
}

func descReorgNewChainBlocks() []Block {
	return append([]Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"a", "b", "a"}},
	}, descReorgNewBranchBlocks()...)
}

func buildDescReorgOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range descReorgOldChainBlocks() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

// referenceDescFilteredHits is an independent oracle for one filtered
// descending answer: it derives occurrences from the supplied chain rather
// than from the Index under test, so a shared QueryTxs bug cannot validate
// itself. Matching uses the same exact-string rule as txFilter.
func referenceDescFilteredHits(chain []Block, txID string) []TxHit {
	var asc []TxHit
	for _, block := range chain {
		for position, tx := range block.Txs {
			if tx != txID {
				continue
			}
			asc = append(asc, TxHit{
				Height:    block.Height,
				BlockHash: block.Hash,
				TxID:      tx,
				Position:  position,
			})
		}
	}
	desc := make([]TxHit, len(asc))
	for i, hit := range asc {
		desc[len(asc)-1-i] = hit
	}
	return desc
}

func referenceDescFilteredPage(chain []Block, txID string, pageSize int, offset int64) TxPage {
	full := referenceDescFilteredHits(chain, txID)
	rest := full[offset:]
	end := pageSize
	if end > len(rest) {
		end = len(rest)
	}
	var blocks int64
	for _, block := range chain {
		for _, tx := range block.Txs {
			if tx == txID {
				blocks++
				break
			}
		}
	}
	return TxPage{
		Hits:          append([]TxHit{}, rest[:end]...),
		TotalMatches:  int64(len(full)),
		MatchedBlocks: blocks,
		ToHeight:      int64(len(chain)),
	}
}

// Hand-derived first pages from the scenario description: these keep the
// guarantee tied to the literal blocks rather than to oracle DeepEqual alone.
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

// TestDescReorgFixtureSemanticsOnBothChains pins the counting and ordering
// facts both deterministic orderings rest on, and cross-checks the oracle
// against a live index before and after the shortening reorg.
func TestDescReorgFixtureSemanticsOnBothChains(t *testing.T) {
	oldChain := descReorgOldChainBlocks()
	newChain := descReorgNewChainBlocks()

	// The new branch must be a legal shortening rooted at retained h1.
	if !reflect.DeepEqual(newChain[0], oldChain[0]) {
		t.Fatal("setup invariant: height 1 must be the retained h1 on both chains")
	}

	oldAll := referenceDescFilteredHits(oldChain, "a")
	wantOldLiteral := literalDescOldFirstPage().Hits
	if !reflect.DeepEqual(oldAll[:3], wantOldLiteral) {
		t.Fatalf("old-chain desc first page=%v, want %v", oldAll[:3], wantOldLiteral)
	}
	if len(oldAll) != 6 {
		t.Fatalf("old-chain matches=%d, want 6", len(oldAll))
	}

	newAll := referenceDescFilteredHits(newChain, "a")
	wantNewLiteral := literalDescNewFirstPage().Hits
	if !reflect.DeepEqual(newAll[:3], wantNewLiteral) {
		t.Fatalf("new-chain desc first page=%v, want %v", newAll[:3], wantNewLiteral)
	}
	if len(newAll) != 6 {
		t.Fatalf("new-chain matches=%d, want 6", len(newAll))
	}
	// Removed and replaced hashes must be absent from every new-chain hit.
	for _, hit := range newAll {
		if hit.Height == 4 || hit.BlockHash == "o2" || hit.BlockHash == "o3" || hit.BlockHash == "o4" {
			t.Fatalf("new-chain oracle carries stale record %+v", hit)
		}
	}

	index := buildDescReorgOldChain(t)
	query := TxQuery{TxIDs: []string{"a"}, PageSize: 3, Order: OrderDesc}
	live, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	if err := classifyDescTxPage(live,
		referenceDescFilteredPage(oldChain, "a", 3, 0),
		referenceDescFilteredPage(newChain, "a", 3, 0),
		oldAll, newAll); err != nil {
		t.Fatalf("live old-chain first page: %v", err)
	}
	if !reflect.DeepEqual(viewOf(live), viewOf(referenceDescFilteredPage(oldChain, "a", 3, 0))) {
		t.Fatalf("live old-chain page=%+v", live)
	}
	if live.NextCursor == "" {
		t.Fatal("6 matches over a 3-row page must leave a continuation cursor")
	}
	// Whole-range statistics, not page-local: page holds 3 hits but totals 6.
	if live.TotalMatches != 6 || live.MatchedBlocks != 4 || live.ToHeight != 4 {
		t.Fatalf("old-chain stats=%+v, want 6/4/4", live)
	}

	dropped, err := index.Reorg(descReorgNewBranchBlocks())
	if err != nil {
		t.Fatalf("shortening reorg must be legal: %v", err)
	}
	if want := []int64{2, 3, 4}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped=%v, want %v", dropped, want)
	}
	if index.Tip != 3 {
		t.Fatalf("tip after reorg=%d, want 3", index.Tip)
	}
	for height := int64(2); height <= 4; height++ {
		if _, ok := index.Blocks[height]; height <= 3 && !ok {
			t.Fatalf("height %d missing after reorg", height)
		}
		if height == 4 {
			if _, ok := index.Blocks[4]; ok {
				t.Fatal("old height 4 survived the shortening reorg")
			}
		}
	}
	for _, stale := range []string{"o2", "o3", "o4"} {
		if _, ok := index.ByHash[stale]; ok {
			t.Fatalf("stale hash %q still indexed after reorg", stale)
		}
	}

	live, err = index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(live), viewOf(referenceDescFilteredPage(newChain, "a", 3, 0))) {
		t.Fatalf("live new-chain page=%+v, want %+v", live, referenceDescFilteredPage(newChain, "a", 3, 0))
	}
	if live.TotalMatches != 6 || live.MatchedBlocks != 3 || live.ToHeight != 3 {
		t.Fatalf("new-chain stats=%+v, want 6/3/3", live)
	}
}

// classifyDescTxPage accepts got only when its visible contents are the
// descending first-page view of exactly one chain. The opaque NextCursor is
// excluded (callers follow it); record provenance is checked against the
// COMPLETE branch hit lists, so a stitched page is exposed even when both
// branches report the same TotalMatches of 6.
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
	fmt.Fprintln(&report, "descending QueryTxs page does not match one complete main chain:")
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

	oldByKey := map[TxHit]bool{}
	newByKey := map[TxHit]bool{}
	for _, h := range oldAllHits {
		oldByKey[h] = true
	}
	for _, h := range newAllHits {
		newByKey[h] = true
	}
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
		fmt.Fprintf(&report, "  => MIXED: hits and statistics come from both the old and the new chain; one page must reflect a single main chain")
	case inNeither > 0:
		fmt.Fprintf(&report, "  => CORRUPT: records exist on neither chain")
	default:
		fmt.Fprintf(&report, "  => CORRUPT: matches neither the old nor the new chain")
	}
	return errors.New(report.String())
}

// The classifier must accept both complete descending answers and reject an
// ordered-looking stitch even though both branches report the same total of 6.
func TestClassifyDescTxPageRejectsMixedPages(t *testing.T) {
	oldChain, newChain := descReorgOldChainBlocks(), descReorgNewChainBlocks()
	oldAll := referenceDescFilteredHits(oldChain, "a")
	newAll := referenceDescFilteredHits(newChain, "a")
	oldRef := literalDescOldFirstPage()
	newRef := literalDescNewFirstPage()
	classify := func(page TxPage) error {
		return classifyDescTxPage(page, oldRef, newRef, oldAll, newAll)
	}
	if err := classify(oldRef); err != nil {
		t.Fatalf("pure old desc page rejected: %v", err)
	}
	if err := classify(newRef); err != nil {
		t.Fatalf("pure new desc page rejected: %v", err)
	}

	// Stitch: new-chain statistics (3 blocks, bound 3) but the first record is
	// the removed height-4 o4 hit, followed by genuine n3 records. Both totals
	// say 6 and the records stay descending, so only provenance exposes it.
	mixed := newRef
	mixed.Hits = []TxHit{
		{Height: 4, BlockHash: "o4", TxID: "a", Position: 1},
		{Height: 3, BlockHash: "n3", TxID: "a", Position: 2},
		{Height: 3, BlockHash: "n3", TxID: "a", Position: 1},
	}
	if err := classify(mixed); err == nil {
		t.Fatal("stitched desc page with a removed o4 record was accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("stitched desc page not reported as MIXED:\n%v", err)
	}

	// Old-chain records claimed under the new shortened bound: the page counts
	// a height above its own ToHeight.
	underClaimed := oldRef
	underClaimed.ToHeight = 3
	underClaimed.MatchedBlocks = 3
	if err := classify(underClaimed); err == nil {
		t.Fatal("old desc records under the new bound were accepted")
	}

	// A pure new page that let an old hash slip into records but kept new
	// counts is still a stitch. Copy the slice first: Hits shares no backing
	// array with the reference once reassigned.
	staleHash := newRef
	staleHash.Hits = append([]TxHit{}, newRef.Hits...)
	staleHash.Hits[2] = TxHit{Height: 2, BlockHash: "o2", TxID: "a", Position: 0}
	if err := classify(staleHash); err == nil {
		t.Fatal("new desc page carrying replaced hash o2 was accepted")
	}
}

// Ordering 1: a descending first page is holding the lock on the complete OLD
// chain when the shortening reorg starts. It must answer on the old chain —
// hits, whole-range statistics, and effective upper bound all old — and the
// legitimate request must not fail. Its cursor is dead after the reorg.
func TestQueryTxsDescFirstPageInFlightObservesCompleteOldChain(t *testing.T) {
	index := buildDescReorgOldChain(t)
	h := installQueryReorgHooks(t, true, false, 3)
	oldChain, newChain := descReorgOldChainBlocks(), descReorgNewChainBlocks()

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
		if _, err := index.Reorg(descReorgNewBranchBlocks()); err != nil {
			errCh <- err
		}
		close(reorgDone)
	}()

	close(h.releaseFirst) // query scans and finishes first; the reorg cannot apply yet
	var got TxPage
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("concurrent descending first page failed: %v", err)
	}

	want := referenceDescFilteredPage(oldChain, "a", 3, 0)
	if err := classifyDescTxPage(got, want, referenceDescFilteredPage(newChain, "a", 3, 0),
		referenceDescFilteredHits(oldChain, "a"), referenceDescFilteredHits(newChain, "a")); err != nil {
		t.Fatalf("in-flight descending first page did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Fatalf("desc query holding the lock across reorg start must see the old chain:\n%+v", got)
	}
	if !reflect.DeepEqual(got.Hits, literalDescOldFirstPage().Hits) {
		t.Fatalf("old desc first page hits=%v, want %v", got.Hits, literalDescOldFirstPage().Hits)
	}
	// Duplicate a's inside o4 stay separate; positions are the original
	// zero-based block positions, not renumbered for descending order.
	if got.Hits[0] != (TxHit{Height: 4, BlockHash: "o4", TxID: "a", Position: 1}) ||
		got.Hits[1] != (TxHit{Height: 4, BlockHash: "o4", TxID: "a", Position: 0}) {
		t.Fatalf("repeated o4 occurrences collapsed or renumbered: %v", got.Hits[:2])
	}
	// Statistics span the whole pinned range, not the 3-row page.
	if got.TotalMatches != 6 || got.MatchedBlocks != 4 || got.ToHeight != 4 {
		t.Fatalf("old desc stats=%+v, want total=6 blocks=4 to=4", got)
	}
	if got.NextCursor == "" {
		t.Fatal("old desc first page over 6 matches must carry a cursor")
	}

	close(h.releaseReorg)
	<-reorgDone
	if index.Tip != 3 {
		t.Fatalf("tip after reorg=%d, want 3", index.Tip)
	}

	// The cursor pins the OLD descending range (to 4, offset 3); on the new
	// chain it must report a data change, never an argument error.
	payload := decodePayload(t, index, got.NextCursor)
	if payload.To != 4 || payload.Off != 3 || payload.Order != OrderDesc {
		t.Fatalf("cursor pins to=%d off=%d order=%d, want 4/3/OrderDesc", payload.To, payload.Off, payload.Order)
	}
	page, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 3, Order: OrderDesc, Cursor: got.NextCursor})
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("old desc cursor across shortening reorg: err=%v page=%+v, want ErrQueryChanged", err, page)
	}
	if errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a committed reorg must not be reported as an argument error: %v", err)
	}
	if !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("changed desc continuation must return a zeroed page, got %+v", page)
	}
}

// Ordering 2: the shortening reorg has fully committed (new chain installed,
// Reorg still holding the lock) when the descending first page starts. It must
// wait and answer on the complete NEW chain, and the cursor must paginate the
// new branch to the end with statistics pinned to that first page — no stale
// o2/o3/o4 record may ever appear.
func TestQueryTxsDescFirstPageDuringCommittedReorgObservesCompleteNewChain(t *testing.T) {
	index := buildDescReorgOldChain(t)
	h := installQueryReorgHooks(t, true, false, 3)
	oldChain, newChain := descReorgOldChainBlocks(), descReorgNewChainBlocks()

	reorgErr := make(chan error, 1)
	go func() {
		_, err := index.Reorg(descReorgNewBranchBlocks())
		reorgErr <- err
	}()
	<-h.reorgParked // new branch applied (tip 3), Reorg still holds the lock

	result := make(chan TxPage, 1)
	go func() {
		page, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: 3, Order: OrderDesc})
		if err != nil {
			t.Errorf("descending first page during reorg failed: %v", err)
			return
		}
		result <- page
	}()

	close(h.releaseReorg) // reorg returns; the first lock the query can get sees the new chain
	if err := <-reorgErr; err != nil {
		t.Fatalf("reorg failed: %v", err)
	}
	close(h.releaseFirst) // the parked first page can now scan and finish

	got := <-result
	want := referenceDescFilteredPage(newChain, "a", 3, 0)
	if err := classifyDescTxPage(got, referenceDescFilteredPage(oldChain, "a", 3, 0), want,
		referenceDescFilteredHits(oldChain, "a"), referenceDescFilteredHits(newChain, "a")); err != nil {
		t.Fatalf("descending first page across reorg commit did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Fatalf("desc query after reorg commit must see the complete new chain:\n%+v", got)
	}
	if !reflect.DeepEqual(got.Hits, literalDescNewFirstPage().Hits) {
		t.Fatalf("new desc first page hits=%v, want %v", got.Hits, literalDescNewFirstPage().Hits)
	}
	if got.TotalMatches != 6 || got.MatchedBlocks != 3 || got.ToHeight != 3 {
		t.Fatalf("new desc stats=%+v, want total=6 blocks=3 to=3", got)
	}
	if got.NextCursor == "" {
		t.Fatal("new desc first page over 6 matches must carry a cursor")
	}

	payload := decodePayload(t, index, got.NextCursor)
	if payload.To != 3 || payload.Off != 3 || payload.Order != OrderDesc {
		t.Fatalf("cursor pins to=%d off=%d order=%d, want 3/3/OrderDesc", payload.To, payload.Off, payload.Order)
	}

	// Drain the new chain: statistics stay pinned to the first page and no
	// stale height or hash leaks in.
	query := TxQuery{TxIDs: []string{"a"}, PageSize: 3, Order: OrderDesc, Cursor: got.NextCursor}
	all := append([]TxHit{}, got.Hits...)
	for pageNo := 1; ; pageNo++ {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("new-chain desc pagination broke at page %d: %v", pageNo, err)
		}
		if page.TotalMatches != 6 || page.MatchedBlocks != 3 || page.ToHeight != 3 {
			t.Fatalf("desc page %d statistics drifted from the pinned first page: %+v", pageNo, page)
		}
		for _, hit := range page.Hits {
			if hit.Height > 3 || hit.BlockHash == "o2" || hit.BlockHash == "o3" || hit.BlockHash == "o4" {
				t.Fatalf("stale record entered the new-chain page: %+v", hit)
			}
		}
		all = append(all, page.Hits...)
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	if wantAll := referenceDescFilteredHits(newChain, "a"); !reflect.DeepEqual(all, wantAll) {
		t.Fatalf("new-chain desc pagination=%v, want %v", all, wantAll)
	}
}

// Sustained overlap with no hooks: reorgers alternate between the tip-4 old
// chain and the shortened tip-3 branch while descending, filtered readers
// start first pages continuously. Every first page must classify as exactly
// one complete chain (never ErrInvalidArgument/ErrQueryChanged); a
// continuation either continues that exact branch or fails with a zeroed
// ErrQueryChanged. A torn page fails under any scheduling.
func TestQueryTxsDescRepeatedShorteningReorgsStayConsistent(t *testing.T) {
	index := buildDescReorgOldChain(t)
	oldAll := referenceDescFilteredHits(descReorgOldChainBlocks(), "a")
	newAll := referenceDescFilteredHits(descReorgNewChainBlocks(), "a")
	oldSuffix := descReorgOldChainBlocks()[1:] // o2, o3, o4 — rooted at retained h1
	newBranch := descReorgNewBranchBlocks()

	const pageSize = 2
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
					branch = oldSuffix
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
		go func() {
			defer wg.Done()
			for run := 0; run < runs; run++ {
				select {
				case <-stop:
					return
				default:
				}
				first, err := index.QueryTxs(TxQuery{TxIDs: []string{"a"}, PageSize: pageSize, Order: OrderDesc})
				if err != nil {
					t.Errorf("descending first page must stay a legal request across reorgs: %v", err)
					return
				}
				var branchHits []TxHit
				switch first.ToHeight {
				case 4:
					branchHits = oldAll
				case 3:
					branchHits = newAll
				default:
					t.Errorf("desc first page pinned unknown tip %d", first.ToHeight)
					return
				}
				if first.TotalMatches != 6 {
					t.Errorf("desc first page total=%d, both branches carry 6 a's", first.TotalMatches)
					return
				}
				if err := classifyDescTxPage(first,
					referenceDescFilteredPage(descReorgOldChainBlocks(), "a", pageSize, 0),
					referenceDescFilteredPage(descReorgNewChainBlocks(), "a", pageSize, 0),
					oldAll, newAll); err != nil {
					t.Errorf("%v", err)
					return
				}
				wantSlice := branchHits[:pageSize]
				if !reflect.DeepEqual(first.Hits, wantSlice) {
					t.Errorf("desc first page records do not match the pinned branch:\n%+v", first.Hits)
					return
				}
				total, blocks, to := first.TotalMatches, first.MatchedBlocks, first.ToHeight
				seen := append([]TxHit{}, first.Hits...)
				query := TxQuery{TxIDs: []string{"a"}, PageSize: pageSize, Order: OrderDesc, Cursor: first.NextCursor}
				for query.Cursor != "" {
					page, err := index.QueryTxs(query)
					if errors.Is(err, ErrQueryChanged) {
						if !reflect.DeepEqual(page, TxPage{}) {
							t.Errorf("changed desc continuation leaked %+v", page)
						}
						if errors.Is(err, ErrInvalidArgument) {
							t.Errorf("a committed reorg surfaced as ErrInvalidArgument: %v", err)
						}
						break // the pinned branch was replaced; restart from a first page
					}
					if err != nil {
						t.Errorf("desc continuation failed: %v", err)
						return
					}
					if page.TotalMatches != total || page.MatchedBlocks != blocks || page.ToHeight != to {
						t.Errorf("desc statistics drifted mid-pagination: %+v", page)
						return
					}
					for _, hit := range page.Hits {
						if hit.Height > to {
							t.Errorf("desc record above pinned bound %d: %+v", to, hit)
							return
						}
						if to == 3 && (hit.BlockHash == "o2" || hit.BlockHash == "o3" || hit.BlockHash == "o4") {
							t.Errorf("stale old-chain hash on new-chain page: %+v", hit)
							return
						}
					}
					wantRest := branchHits[len(seen):]
					if len(wantRest) > pageSize {
						wantRest = wantRest[:pageSize]
					}
					if !reflect.DeepEqual(page.Hits, wantRest) {
						t.Errorf("desc continuation records do not match the pinned branch:\n%+v", page.Hits)
						return
					}
					seen = append(seen, page.Hits...)
					query.Cursor = page.NextCursor
					if query.Cursor == "" && len(seen) != len(branchHits) {
						t.Errorf("desc cursor chain ended after %d hits, branch has %d", len(seen), len(branchHits))
						return
					}
				}
			}
		}()
	}

	wg.Wait()
	close(stop)

	// Leave the index deterministically on the shortened new chain and verify
	// the complete descending filtered answer against the oracle.
	if _, err := index.Reorg(newBranch); err != nil {
		t.Fatalf("final reorg failed: %v", err)
	}
	pages := collectDescPages(t, index, TxQuery{TxIDs: []string{"a"}, PageSize: pageSize})
	var all []TxHit
	for i, page := range pages {
		if page.TotalMatches != 6 || page.MatchedBlocks != 3 || page.ToHeight != 3 {
			t.Fatalf("final desc page %d drifted: %+v", i, page)
		}
		all = append(all, page.Hits...)
	}
	if !reflect.DeepEqual(all, newAll) {
		t.Fatalf("final new-chain desc answer=%v, want %v", all, newAll)
	}
}
