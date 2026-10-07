package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This file guards QueryTxs against a main-chain reorg that makes the chain
// shorter while a paginated query is in flight. A first page issued at that
// instant must answer from ONE complete main chain — either the old chain or
// the new one: the pinned upper height, the whole-range statistics, every
// returned record, and the continuation cursor must all describe that same
// branch. Once the first page has pinned its range, a continuation either
// still sees the complete chain the first page saw (statistics and pinned
// height identical) or fails with ErrQueryChanged and a fully zeroed result;
// it can never stitch two branches together.
//
// The scenario (no filter, To left at zero so the first page pins the tip):
//
//	Retained ancestor (both chains):
//	  h1 parent g  txs [x, x] // same id twice in one block: two occurrences
//	  h2 parent h1 txs [y]
//	Old branch, tip 5:
//	  h3 parent h2 txs [a, c]
//	  h4 parent h3 txs [d, d]    // duplicate inside one block; block counts once
//	  h5 parent h4 txs [e, f, g]
//	New shorter branch, tip 4 (replaces h3..h5, rooted at the retained h2):
//	  j3 parent h2 txs [p, q]
//	  j4 parent j3 txs [r, s, r] // different multiplicity; h5 vanishes
//
// Old-chain answer (range 1..5): 10 occurrences over 5 blocks, tip 5.
// New-chain answer (range 1..4):  8 occurrences over 4 blocks, tip 4.
//
// The suffix transaction sets are disjoint ({a,c,d,e,f,g} vs {p,q,r,s}) and
// every suffix hash differs, so the two complete answers are observably
// different not just in their counts but in every record above height 2. A
// page that happens to stay ordered (e.g. one new-chain hit followed by one
// old-chain hit at consecutive positions) is still a torn answer and must be
// rejected by classifyTxPage.

func queryRetainedBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"x", "x"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"y"}},
	}
}

func queryOldSuffixBlocks() []Block {
	return []Block{
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a", "c"}},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"d", "d"}},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"e", "f", "g"}},
	}
}

func queryNewShortBranch() []Block {
	return []Block{
		{Height: 3, Hash: "j3", Parent: "h2", Txs: []string{"p", "q"}},
		{Height: 4, Hash: "j4", Parent: "j3", Txs: []string{"r", "s", "r"}},
	}
}

func queryOldChainBlocks() []Block {
	chain := append([]Block{}, queryRetainedBlocks()...)
	return append(chain, queryOldSuffixBlocks()...)
}

func queryNewChainBlocks() []Block {
	chain := append([]Block{}, queryRetainedBlocks()...)
	return append(chain, queryNewShortBranch()...)
}

func buildQueryOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range queryOldChainBlocks() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

// referenceTxHits is an independent oracle: it derives the complete ordered
// occurrence list from an explicitly supplied chain, not from the Index under
// test, so a shared QueryTxs bug cannot make an expectation match itself.
func referenceTxHits(chain []Block) []TxHit {
	hits := []TxHit{}
	for _, block := range chain {
		for position, tx := range block.Txs {
			hits = append(hits, TxHit{
				Height:    block.Height,
				BlockHash: block.Hash,
				TxID:      tx,
				Position:  position,
			})
		}
	}
	return hits
}

func referenceMatchedBlocks(chain []Block) int64 {
	var blocks int64
	for _, block := range chain {
		if len(block.Txs) > 0 {
			blocks++
		}
	}
	return blocks
}

// referenceTxPage is one page of the complete answer for chain (heights
// 1..len(chain)) sliced at offset, with the whole-range statistics the page
// must carry. The continuation cursor is deliberately excluded: it is opaque
// and is validated separately by actually following it.
func referenceTxPage(chain []Block, pageSize int, offset int64) TxPage {
	full := referenceTxHits(chain)
	rest := full[offset:]
	end := pageSize
	if end > len(rest) {
		end = len(rest)
	}
	return TxPage{
		Hits:          append([]TxHit{}, rest[:end]...),
		TotalMatches:  int64(len(full)),
		MatchedBlocks: referenceMatchedBlocks(chain),
		ToHeight:      int64(len(chain)),
	}
}

// Hand-derived key records, so the guarantee rests on the literal
// transactions of each branch rather than on oracle-vs-DeepEqual alone.
func literalOldFirstPage() TxPage {
	return TxPage{
		Hits: []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "x", Position: 0},
			{Height: 1, BlockHash: "h1", TxID: "x", Position: 1},
			{Height: 2, BlockHash: "h2", TxID: "y", Position: 0},
		},
		TotalMatches:  10,
		MatchedBlocks: 5,
		ToHeight:      5,
	}
}

func literalNewFirstPage() TxPage {
	page := literalOldFirstPage()
	page.TotalMatches = 8
	page.MatchedBlocks = 4
	page.ToHeight = 4
	return page
}

// The counting and ordering semantics the regression rests on, asserted for
// each chain and cross-checked against the live index before and after the
// reorg.
func TestReorgQueryFixtureSemanticsOnBothChains(t *testing.T) {
	oldChain := queryOldChainBlocks()
	newChain := queryNewChainBlocks()

	// Setup invariant: suffix identifier sets must be disjoint and every
	// suffix hash must differ, otherwise a mixed page could be
	// indistinguishable by content.
	oldIDs := map[string]struct{}{}
	for _, b := range oldChain[2:] {
		for _, tx := range b.Txs {
			oldIDs[tx] = struct{}{}
		}
	}
	for _, b := range newChain[2:] {
		for _, tx := range b.Txs {
			if _, conflict := oldIDs[tx]; conflict {
				t.Fatalf("setup invariant broken: new-branch tx %q also exists on the old suffix", tx)
			}
		}
	}

	oldHits := referenceTxHits(oldChain)
	wantOld := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "x", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "x", Position: 1},
		{Height: 4, BlockHash: "h4", TxID: "d", Position: 0},
		{Height: 4, BlockHash: "h4", TxID: "d", Position: 1},
		{Height: 5, BlockHash: "h5", TxID: "g", Position: 2},
	}
	for _, want := range wantOld {
		if !containsHit(oldHits, want) {
			t.Fatalf("old chain misses literal occurrence %+v in %v", want, oldHits)
		}
	}
	if len(oldHits) != 10 || referenceMatchedBlocks(oldChain) != 5 {
		t.Fatalf("old chain: hits=%d blocks=%d, want 10/5", len(oldHits), referenceMatchedBlocks(oldChain))
	}
	newHits := referenceTxHits(newChain)
	if len(newHits) != 8 || referenceMatchedBlocks(newChain) != 4 {
		t.Fatalf("new chain: hits=%d blocks=%d, want 8/4", len(newHits), referenceMatchedBlocks(newChain))
	}
	// h4's two d's are one block; j4's two r's are one block; removed
	// height 5 is simply absent.
	for _, gone := range []TxHit{
		{Height: 5, BlockHash: "h5", TxID: "e", Position: 0},
		{Height: 4, BlockHash: "h4", TxID: "d", Position: 0},
	} {
		if containsHit(newHits, gone) {
			t.Fatalf("new chain must not carry %+v", gone)
		}
	}
	if !containsHit(newHits, TxHit{Height: 4, BlockHash: "j4", TxID: "r", Position: 0}) ||
		!containsHit(newHits, TxHit{Height: 4, BlockHash: "j4", TxID: "r", Position: 2}) {
		t.Fatalf("new chain must keep both r occurrences: %v", newHits)
	}
	// Every occurrence list is ordered by height then position.
	requireOrdered(t, oldHits)
	requireOrdered(t, newHits)

	// The oracle must match the live index on both complete chain states.
	index := buildQueryOldChain(t)
	live, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live.Hits, oldHits) || live.TotalMatches != 10 || live.MatchedBlocks != 5 || live.ToHeight != 5 {
		t.Fatalf("live old-chain query=%+v, want 10/5/tip5", live)
	}
	dropped, err := index.Reorg(queryNewShortBranch())
	if err != nil {
		t.Fatalf("shortening reorg refused: %v", err)
	}
	if want := []int64{3, 4, 5}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped=%v, want %v", dropped, want)
	}
	if index.Tip != 4 {
		t.Fatalf("tip=%d, want shortened tip 4", index.Tip)
	}
	if _, ok := index.Blocks[5]; ok {
		t.Fatal("height 5 still present after shortening reorg")
	}
	if _, ok := index.ByHash["h5"]; ok {
		t.Fatal("removed hash h5 still indexed")
	}
	live, err = index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live.Hits, newHits) || live.TotalMatches != 8 || live.MatchedBlocks != 4 || live.ToHeight != 4 {
		t.Fatalf("live new-chain query=%+v, want 8/4/tip4", live)
	}
}

func containsHit(hits []TxHit, want TxHit) bool {
	for _, hit := range hits {
		if hit == want {
			return true
		}
	}
	return false
}

func requireOrdered(t *testing.T, hits []TxHit) {
	t.Helper()
	for i := 1; i < len(hits); i++ {
		prev, cur := hits[i-1], hits[i]
		if cur.Height < prev.Height || (cur.Height == prev.Height && cur.Position <= prev.Position) {
			t.Fatalf("hits not ordered at %d: %+v after %+v", i, cur, prev)
		}
	}
}

// classifyTxPage accepts got only when its visible contents are the first-page
// view of exactly one chain. The opaque NextCursor is not part of this view;
// callers validate it by following it. Record provenance is checked against
// the COMPLETE branch hit lists, not just the records on the reference page,
// so a stitched page drawn from later heights is still exposed. It does NOT
// treat "hit count equals TotalMatches" as sufficient: an ordered, correctly
// counted stitched page still carries records from both branches.
func classifyTxPage(got, oldRef, newRef TxPage, oldAllHits, newAllHits []TxHit) error {
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
	fmt.Fprintln(&report, "QueryTxs page does not match one complete main chain:")
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

	// Record-level provenance: each hit must be explainable by the same chain
	// at the same page offset. This is what rejects an ordered-looking page
	// that interleaves two branches.
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
	// A record that belongs to exactly one branch tags that branch; shared
	// ancestor records tag neither.
	if onlyOld > 0 {
		tags["old"] = true
	}
	if onlyNew > 0 {
		tags["new"] = true
	}
	switch {
	case tags["old"] && tags["new"]:
		fmt.Fprintf(&report, "  => MIXED: stats and records come from both the old and the new chain; one page must reflect a single main chain")
	case inNeither > 0:
		fmt.Fprintf(&report, "  => CORRUPT: records exist on neither chain")
	default:
		fmt.Fprintf(&report, "  => CORRUPT: matches neither the old nor the new chain")
	}
	return errors.New(report.String())
}

// The classifier must accept both complete answers and reject ordered-looking
// mixtures and plain corruption.
func TestClassifyTxPageRejectsMixedAndCorruptPages(t *testing.T) {
	oldRef := literalOldFirstPage()
	newRef := literalNewFirstPage()
	oldAll := referenceTxHits(queryOldChainBlocks())
	newAll := referenceTxHits(queryNewChainBlocks())
	classify := func(page TxPage) error { return classifyTxPage(page, oldRef, newRef, oldAll, newAll) }
	if err := classify(oldRef); err != nil {
		t.Fatalf("pure old page rejected: %v", err)
	}
	if err := classify(newRef); err != nil {
		t.Fatalf("pure new page rejected: %v", err)
	}

	// Mixture 1: shared first-page records (identical on both chains), but
	// new-chain counts under the OLD tip height. Hit count still equals the
	// total on both chains, so a count-equality check would accept it.
	mixed1 := newRef
	mixed1.ToHeight = oldRef.ToHeight
	if int64(len(mixed1.Hits)) != 3 || mixed1.TotalMatches != 8 {
		t.Fatal("test setup: mixed1 counts are meant to look internally consistent")
	}
	if err := classify(mixed1); err == nil {
		t.Fatal("new-chain counts under the old tip height were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("mixed1 not reported as MIXED:\n%v", err)
	}

	// Mixture 2: old-chain counts and records under the NEW shortened tip
	// height — claims tip 4 while still counting removed height 5.
	mixed2 := oldRef
	mixed2.ToHeight = newRef.ToHeight
	if err := classify(mixed2); err == nil {
		t.Fatal("old-chain counts under the new tip height were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("mixed2 not reported as MIXED:\n%v", err)
	}

	// Mixture 3: an ordered-looking page (height 3 positions 0 and 1) that
	// stitches one old-branch record to one new-branch record, tagged old by
	// its stale tip height.
	mixed3 := oldRef
	mixed3.Hits = []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
		{Height: 3, BlockHash: "j3", TxID: "q", Position: 1},
	}
	if err := classify(mixed3); err == nil {
		t.Fatal("stitched hits from both branches were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("mixed3 not reported as MIXED:\n%v", err)
	}

	// Corruption: stats belonging to no chain at all.
	corrupt := newRef
	corrupt.MatchedBlocks = 99
	if err := classify(corrupt); err == nil {
		t.Fatal("corrupt stats matching neither chain were accepted")
	}
}

// Deterministic rendezvous for the QueryTxs/Reorg overlap tests. A parked
// first page holds the lock with its tip already pinned; a parked
// continuation holds it after every pinned-range check has passed; a parked
// reorg has the new branch fully applied but has not returned.
type queryReorgHooks struct {
	firstParked  chan struct{}
	releaseFirst chan struct{}
	contParked   chan struct{}
	releaseCont  chan struct{}
	reorgParked  chan struct{}
	releaseReorg chan struct{}

	allowedReorgTips map[int64]bool
	firstOnce        sync.Once
	contOnce         sync.Once
	reorgOnce        sync.Once
}

func installQueryReorgHooks(t *testing.T, parkFirst, parkCont bool, reorgTips ...int64) *queryReorgHooks {
	t.Helper()
	h := &queryReorgHooks{
		reorgParked:  make(chan struct{}),
		releaseReorg: make(chan struct{}),
	}
	if parkFirst {
		h.firstParked = make(chan struct{})
		h.releaseFirst = make(chan struct{})
	}
	if parkCont {
		h.contParked = make(chan struct{})
		h.releaseCont = make(chan struct{})
	}
	if len(reorgTips) > 0 {
		h.allowedReorgTips = map[int64]bool{}
		for _, tip := range reorgTips {
			h.allowedReorgTips[tip] = true
		}
	}
	queryTxsHookLocked = func(from, to int64, continuation bool) {
		switch {
		case continuation && h.contParked != nil:
			h.contOnce.Do(func() { close(h.contParked) })
			<-h.releaseCont
		case !continuation && h.firstParked != nil:
			h.firstOnce.Do(func() { close(h.firstParked) })
			<-h.releaseFirst
		}
	}
	reorgAppliedHookLocked = func(newTip int64) {
		if h.allowedReorgTips != nil && !h.allowedReorgTips[newTip] {
			t.Errorf("reorg hook saw newTip=%d, want one of %v", newTip, h.allowedReorgTips)
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

// decodePayload exposes the signed cursor contents within the package so a
// test can prove which branch a cursor pins (upper height, offset, range
// fingerprint) without using it.
func decodePayload(t *testing.T, index *Index, cursor string) cursorPayload {
	t.Helper()
	payload, err := index.decodeCursor(cursor)
	if err != nil {
		t.Fatalf("cursor not decodable on its own index: %v", err)
	}
	return payload
}

// Ordering 1: the first page is holding the lock on the complete OLD chain
// when the shortening reorg starts. It must finish on the old chain; once the
// reorg completes, its cursor is dead.
func TestQueryTxsFirstPageInFlightObservesCompleteOldChain(t *testing.T) {
	index := buildQueryOldChain(t)
	h := installQueryReorgHooks(t, true, false, 4)
	oldChain, newChain := queryOldChainBlocks(), queryNewChainBlocks()

	result := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, err := index.QueryTxs(TxQuery{PageSize: 3})
		if err != nil {
			errCh <- err
			return
		}
		result <- page
	}()

	<-h.firstParked // query holds index.mu and has pinned tip 5
	reorgDone := make(chan struct{})
	go func() {
		if _, err := index.Reorg(queryNewShortBranch()); err != nil {
			errCh <- err
		}
		close(reorgDone)
	}()

	close(h.releaseFirst) // query scans and finishes first; reorg cannot apply yet
	var got TxPage
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("concurrent call failed: %v", err)
	}
	want := referenceTxPage(oldChain, 3, 0)
	if err := classifyTxPage(got, want, referenceTxPage(newChain, 3, 0),
		referenceTxHits(oldChain), referenceTxHits(newChain)); err != nil {
		t.Fatalf("in-flight first page did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Fatalf("query holding the lock across reorg start must see old chain:\n%+v", got)
	}
	if !reflect.DeepEqual(got.Hits, literalOldFirstPage().Hits) {
		t.Fatalf("old first page hits=%v, want literal old records", got.Hits)
	}
	if got.NextCursor == "" {
		t.Fatal("old first page over 10 matches must carry a cursor")
	}

	close(h.releaseReorg) // now let the parked reorg commit and return
	<-reorgDone
	if index.Tip != 4 {
		t.Fatalf("tip after reorg=%d, want 4", index.Tip)
	}

	// The cursor pins the OLD branch (tip 5, offset 3, old fingerprint); on
	// the new chain it must fail rather than resume on mixed data.
	oldFingerprint := buildQueryOldChain(t)
	payload := decodePayload(t, index, got.NextCursor)
	if payload.To != 5 || payload.Off != 3 {
		t.Fatalf("cursor pins to=%d off=%d, want 5/3", payload.To, payload.Off)
	}
	if payload.FP != fmt.Sprintf("%x", oldFingerprint.hashRangeBlockContent(1, 5)) {
		t.Fatal("cursor fingerprint does not match the complete old chain")
	}
	page, err := index.QueryTxs(TxQuery{PageSize: 3, Cursor: got.NextCursor})
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("old cursor across reorg: err=%v page=%+v, want ErrQueryChanged", err, page)
	}
	if !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("changed continuation must return a zeroed page, got %+v", page)
	}
}

// Ordering 2: the shortening reorg has fully committed (new chain installed,
// Reorg still holding the lock) when the first page starts. It must wait and
// then answer on the complete NEW chain, and its cursor must paginate the new
// branch to the end.
func TestQueryTxsFirstPageDuringCommittedReorgObservesCompleteNewChain(t *testing.T) {
	index := buildQueryOldChain(t)
	h := installQueryReorgHooks(t, true, false, 4)
	oldChain, newChain := queryOldChainBlocks(), queryNewChainBlocks()

	reorgErr := make(chan error, 1)
	go func() {
		_, err := index.Reorg(queryNewShortBranch())
		reorgErr <- err
	}()
	<-h.reorgParked // new branch applied (tip 4), Reorg still holds the lock

	result := make(chan TxPage, 1)
	go func() {
		page, err := index.QueryTxs(TxQuery{PageSize: 3})
		if err != nil {
			t.Errorf("query during reorg failed: %v", err)
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
	want := referenceTxPage(newChain, 3, 0)
	if err := classifyTxPage(got, referenceTxPage(oldChain, 3, 0), want,
		referenceTxHits(oldChain), referenceTxHits(newChain)); err != nil {
		t.Fatalf("first page across reorg commit did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Fatalf("query after reorg commit must see the complete new chain:\n%+v", got)
	}
	if !reflect.DeepEqual(got.Hits, literalNewFirstPage().Hits) {
		t.Fatalf("new first page hits=%v, want literal new records", got.Hits)
	}
	if got.NextCursor == "" {
		t.Fatal("new first page over 8 matches must carry a cursor")
	}

	// The cursor pins the NEW branch; following it must drain exactly the new
	// records with statistics pinned to the first page — no old-suffix leak.
	newFingerprint := New()
	for _, b := range newChain {
		if err := newFingerprint.Append(b); err != nil {
			t.Fatal(err)
		}
	}
	payload := decodePayload(t, index, got.NextCursor)
	if payload.To != 4 || payload.Off != 3 {
		t.Fatalf("cursor pins to=%d off=%d, want 4/3", payload.To, payload.Off)
	}
	if payload.FP != fmt.Sprintf("%x", newFingerprint.hashRangeBlockContent(1, 4)) {
		t.Fatal("cursor fingerprint does not match the complete new chain")
	}

	query := TxQuery{PageSize: 3, Cursor: got.NextCursor}
	var all = append([]TxHit{}, got.Hits...)
	for pageNo := 1; ; pageNo++ {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("new-chain pagination broke at page %d: %v", pageNo, err)
		}
		if page.TotalMatches != 8 || page.MatchedBlocks != 4 || page.ToHeight != 4 {
			t.Fatalf("page %d statistics drifted from the pinned first page: %+v", pageNo, page)
		}
		all = append(all, page.Hits...)
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	if !reflect.DeepEqual(all, referenceTxHits(newChain)) {
		t.Fatalf("new-chain pagination=%v, want %v", all, referenceTxHits(newChain))
	}
}

// viewOf strips the opaque cursor for whole-page DeepEqual comparisons.
func viewOf(page TxPage) TxPage {
	page.NextCursor = ""
	return page
}

// After the first page pinned its range, every continuation outcome is
// deterministic: a block inside the pinned range changed or the chain shrank
// below the pinned upper bound -> ErrQueryChanged with a fully zeroed result.
func TestQueryTxsContinuationAfterReorgIsChangedAndZeroed(t *testing.T) {
	t.Run("suffix replaced under a pinned tip", func(t *testing.T) {
		index := buildQueryOldChain(t)
		first, err := index.QueryTxs(TxQuery{PageSize: 3})
		if err != nil {
			t.Fatal(err)
		}
		if first.NextCursor == "" || first.ToHeight != 5 || first.TotalMatches != 10 {
			t.Fatalf("unexpected first page: %+v", first)
		}
		if _, err := index.Reorg(queryNewShortBranch()); err != nil {
			t.Fatal(err)
		}
		page, err := index.QueryTxs(TxQuery{PageSize: 3, Cursor: first.NextCursor})
		requireChangedZeroed(t, page, err)
	})

	t.Run("chain shortened below an explicit to", func(t *testing.T) {
		index := buildQueryOldChain(t)
		first, err := index.QueryTxs(TxQuery{To: 5, PageSize: 3})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := index.Reorg(queryNewShortBranch()); err != nil {
			t.Fatal(err) // tip becomes 4, below the pinned to 5
		}
		page, err := index.QueryTxs(TxQuery{To: 5, PageSize: 3, Cursor: first.NextCursor})
		requireChangedZeroed(t, page, err)
	})

	t.Run("block replaced inside the pinned range with the tip intact", func(t *testing.T) {
		index := buildQueryOldChain(t)
		first, err := index.QueryTxs(TxQuery{To: 5, PageSize: 3})
		if err != nil {
			t.Fatal(err)
		}
		// Same length as the old suffix, different content starting at h3:
		// the tip stays 5 but the pinned fingerprint must not match.
		sameLength := []Block{
			{Height: 3, Hash: "k3", Parent: "h2", Txs: []string{"k"}},
			{Height: 4, Hash: "k4", Parent: "k3", Txs: []string{"k"}},
			{Height: 5, Hash: "k5", Parent: "k4", Txs: []string{"k"}},
		}
		if _, err := index.Reorg(sameLength); err != nil {
			t.Fatal(err)
		}
		if index.Tip != 5 {
			t.Fatalf("setup: tip must stay 5, got %d", index.Tip)
		}
		page, err := index.QueryTxs(TxQuery{To: 5, PageSize: 3, Cursor: first.NextCursor})
		requireChangedZeroed(t, page, err)

		// The index stays usable: a fresh first page describes the new chain.
		fresh, err := index.QueryTxs(TxQuery{PageSize: 100})
		if err != nil {
			t.Fatal(err)
		}
		liveChain := append([]Block{}, queryRetainedBlocks()...)
		liveChain = append(liveChain, sameLength...)
		want := referenceTxPage(liveChain, 100, 0)
		if !reflect.DeepEqual(viewOf(fresh), viewOf(want)) {
			t.Fatalf("fresh page=%+v, want %+v", fresh, want)
		}
	})
}

func requireChangedZeroed(t *testing.T, page TxPage, err error) {
	t.Helper()
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("err=%v, want ErrQueryChanged (page=%+v)", err, page)
	}
	if errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("a committed reorg must not be reported as an argument error: %v", err)
	}
	if !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("changed continuation must return no records, cursor, or statistics; got %+v", page)
	}
}

// A continuation that has already verified the pinned range while the OLD
// chain is complete may return the old next page even if a reorg is waiting
// for the lock. Its statistics and pinned height must match the first page;
// after the reorg commits, every old cursor is dead.
func TestQueryTxsContinuationInFlightObservesCompleteOldChainThenDies(t *testing.T) {
	index := buildQueryOldChain(t)
	oldChain := queryOldChainBlocks()

	first, err := index.QueryTxs(TxQuery{PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Hits, referenceTxPage(oldChain, 2, 0).Hits) {
		t.Fatalf("first page=%+v", first)
	}
	cursorOne := first.NextCursor

	h := installQueryReorgHooks(t, false, true, 4)

	result := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, err := index.QueryTxs(TxQuery{PageSize: 2, Cursor: cursorOne})
		if err != nil {
			errCh <- err
			return
		}
		result <- page
	}()

	<-h.contParked // continuation passed every pinned-range check on the old chain
	reorgDone := make(chan struct{})
	go func() {
		if _, err := index.Reorg(queryNewShortBranch()); err != nil {
			errCh <- err
		}
		close(reorgDone)
	}()

	close(h.releaseCont) // continuation finishes on the complete old chain first
	var second TxPage
	select {
	case second = <-result:
	case err := <-errCh:
		t.Fatalf("concurrent call failed: %v", err)
	}
	wantSecond := referenceTxPage(oldChain, 2, 2) // y at h2, then a at h3
	if !reflect.DeepEqual(viewOf(second), viewOf(wantSecond)) {
		t.Fatalf("old next page=%+v, want complete old-chain page %+v", second, wantSecond)
	}
	// Whole-range statistics and the pinned upper height are exactly those of
	// the first page, even though the reorg is committed a moment later.
	if second.TotalMatches != first.TotalMatches ||
		second.MatchedBlocks != first.MatchedBlocks ||
		second.ToHeight != first.ToHeight {
		t.Fatalf("continuation stats=%d/%d/%d differ from first page %d/%d/%d",
			second.TotalMatches, second.MatchedBlocks, second.ToHeight,
			first.TotalMatches, first.MatchedBlocks, first.ToHeight)
	}
	if second.NextCursor == "" {
		t.Fatal("six old hits remain, expected another cursor")
	}

	close(h.releaseReorg)
	<-reorgDone

	// Every cursor minted against the old chain is now dead, including the
	// first one, and neither leaks records: both return fully zeroed pages.
	for name, cursor := range map[string]string{"page one": cursorOne, "page two": second.NextCursor} {
		page, err := index.QueryTxs(TxQuery{PageSize: 2, Cursor: cursor})
		if !errors.Is(err, ErrQueryChanged) || !reflect.DeepEqual(page, TxPage{}) {
			t.Fatalf("%s cursor after reorg: page=%+v err=%v, want zeroed ErrQueryChanged", name, page, err)
		}
	}
	// A fresh first page starts over on the complete new chain.
	fresh, err := index.QueryTxs(TxQuery{PageSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(fresh), viewOf(referenceTxPage(queryNewChainBlocks(), 3, 0))) {
		t.Fatalf("fresh page after reorg=%+v", fresh)
	}
}

// Fixed-range boundary: when a reorg only replaces the suffix ABOVE the pinned
// range and the new chain still covers the pinned upper bound, an outstanding
// cursor keeps working. Remaining records are neither lost nor duplicated and
// no out-of-range transaction enters the result.
func TestQueryTxsCursorSurvivesOutOfRangeShorteningReorg(t *testing.T) {
	// Heights 1..5 come from the shared fixture (so the pinned prefix has its
	// literal records); height 6 extends the old tip with a suffix-only block.
	index := buildQueryOldChain(t)
	if err := index.Append(Block{Height: 6, Hash: "h6", Parent: "h5", Txs: []string{"z"}}); err != nil {
		t.Fatalf("setup append at 6: %v", err)
	}
	pinnedChain := queryOldChainBlocks()[:4] // heights 1..4: x,x,y,a,c,d,d
	first, err := index.QueryTxs(TxQuery{From: 1, To: 4, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := referenceTxPage(pinnedChain, 2, 0)
	if !reflect.DeepEqual(viewOf(first), viewOf(wantFirst)) || first.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", first)
	}

	// Replace heights 5..6 with a shorter suffix whose tip (5) still covers
	// the pinned upper bound 4. Nothing inside [1,4] changes.
	dropped, err := index.Reorg([]Block{
		{Height: 5, Hash: "k5", Parent: "h4", Txs: []string{"nb1", "nb2"}},
	})
	if err != nil {
		t.Fatalf("out-of-range reorg refused: %v", err)
	}
	if !reflect.DeepEqual(dropped, []int64{5, 6}) || index.Tip != 5 {
		t.Fatalf("unexpected reorg outcome: dropped=%v tip=%d", dropped, index.Tip)
	}

	query := TxQuery{From: 1, To: 4, PageSize: 2, Cursor: first.NextCursor}
	all := append([]TxHit{}, first.Hits...)
	pages := 1
	for {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("cursor must survive a reorg confined above the pinned range: %v", err)
		}
		if page.TotalMatches != 7 || page.MatchedBlocks != 4 || page.ToHeight != 4 {
			t.Fatalf("pinned statistics drifted: %+v", page)
		}
		for _, hit := range page.Hits {
			if hit.Height > 4 {
				t.Fatalf("out-of-range height entered the result: %+v", hit)
			}
			if hit.BlockHash == "h5" || hit.BlockHash == "h6" || hit.BlockHash == "k5" {
				t.Fatalf("out-of-range block %q entered the result", hit.BlockHash)
			}
			if hit.TxID == "e" || hit.TxID == "f" || hit.TxID == "g" || hit.TxID == "z" ||
				hit.TxID == "nb1" || hit.TxID == "nb2" {
				t.Fatalf("out-of-range transaction %q entered the result", hit.TxID)
			}
		}
		all = append(all, page.Hits...)
		pages++
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	if !reflect.DeepEqual(all, referenceTxHits(pinnedChain)) {
		t.Fatalf("pinned records lost or duplicated:\n got=%v\nwant=%v", all, referenceTxHits(pinnedChain))
	}
	if pages != 4 {
		t.Fatalf("pages=%d, want 4 (2/2/2/1 over seven hits)", pages)
	}
}

// Sustained overlap with no hooks: reorgers alternate between the tip-5 old
// suffix and the shortened tip-4 branch while readers paginate continuously.
// Timing-independent — every first page must classify as one complete chain,
// every continuation must either continue that exact branch or fail with a
// zeroed ErrQueryChanged, and readers pinned to the retained prefix never
// fail at all. A torn answer fails under any scheduling.
func TestQueryTxsRepeatedShorteningReorgsStayConsistent(t *testing.T) {
	index := buildQueryOldChain(t)
	oldHits := referenceTxHits(queryOldChainBlocks())
	newHits := referenceTxHits(queryNewChainBlocks())
	oldSuffix := queryOldSuffixBlocks()
	newBranch := queryNewShortBranch()

	const pageSize = 2
	const reorgers = 2
	const rounds = 40
	const tipReaders = 4
	const runs = 60
	const pinnedReaders = 2

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

	for r := 0; r < tipReaders; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for run := 0; run < runs; run++ {
				select {
				case <-stop:
					return
				default:
				}
				first, err := index.QueryTxs(TxQuery{PageSize: pageSize})
				if err != nil {
					t.Errorf("first page failed: %v", err)
					return
				}
				var branchHits []TxHit
				switch first.ToHeight {
				case 5:
					branchHits = oldHits
				case 4:
					branchHits = newHits
				default:
					t.Errorf("first page pinned unknown tip %d", first.ToHeight)
					return
				}
				total, blocks, to := first.TotalMatches, first.MatchedBlocks, first.ToHeight
				if total != int64(len(branchHits)) {
					t.Errorf("first page total=%d for tip %d, hits on that branch=%d", total, to, len(branchHits))
					return
				}
				if err := classifyTxPage(first,
					referenceTxPage(queryOldChainBlocks(), pageSize, 0),
					referenceTxPage(queryNewChainBlocks(), pageSize, 0),
					oldHits, newHits); err != nil {
					t.Errorf("%v", err)
					return
				}
				seen := append([]TxHit{}, first.Hits...)
				if !reflect.DeepEqual(first.Hits, branchHits[:len(first.Hits)]) {
					t.Errorf("first page records do not match the pinned branch:\n%+v", first.Hits)
					return
				}
				query := TxQuery{PageSize: pageSize, Cursor: first.NextCursor}
				for query.Cursor != "" {
					page, err := index.QueryTxs(query)
					if errors.Is(err, ErrQueryChanged) {
						if !reflect.DeepEqual(page, TxPage{}) {
							t.Errorf("changed continuation leaked %+v", page)
						}
						break // the pinned branch was replaced; restart from a first page
					}
					if err != nil {
						t.Errorf("continuation failed: %v", err)
						return
					}
					if page.TotalMatches != total || page.MatchedBlocks != blocks || page.ToHeight != to {
						t.Errorf("statistics drifted mid-pagination: %+v", page)
						return
					}
					wantSlice := branchHits[len(seen):]
					if len(wantSlice) > pageSize {
						wantSlice = wantSlice[:pageSize]
					}
					if !reflect.DeepEqual(page.Hits, wantSlice) {
						t.Errorf("continuation records do not match the pinned branch:\n%+v", page.Hits)
						return
					}
					seen = append(seen, page.Hits...)
					query.Cursor = page.NextCursor
					if query.Cursor == "" && len(seen) != len(branchHits) {
						t.Errorf("cursor chain ended after %d hits, branch has %d", len(seen), len(branchHits))
						return
					}
				}
			}
		}()
	}

	// Readers pinned to [1,2] must never be affected: every reorg replaces
	// heights 3+ and both branches always cover height 2.
	for r := 0; r < pinnedReaders; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for run := 0; run < runs; run++ {
				select {
				case <-stop:
					return
				default:
				}
				query := TxQuery{From: 1, To: 2, PageSize: pageSize}
				var all []TxHit
				for {
					page, err := index.QueryTxs(query)
					if err != nil {
						t.Errorf("pinned-range query must never fail across suffix-only reorgs: %v", err)
						return
					}
					if page.TotalMatches != 3 || page.MatchedBlocks != 2 || page.ToHeight != 2 {
						t.Errorf("pinned statistics drifted: %+v", page)
						return
					}
					all = append(all, page.Hits...)
					if page.NextCursor == "" {
						break
					}
					query.Cursor = page.NextCursor
				}
				want := []TxHit{
					{Height: 1, BlockHash: "h1", TxID: "x", Position: 0},
					{Height: 1, BlockHash: "h1", TxID: "x", Position: 1},
					{Height: 2, BlockHash: "h2", TxID: "y", Position: 0},
				}
				if !reflect.DeepEqual(all, want) {
					t.Errorf("pinned records lost, duplicated, or contaminated: %v", all)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(stop)

	// Leave the index deterministically on the shortened new chain and verify
	// the complete answer against the oracle.
	if _, err := index.Reorg(newBranch); err != nil {
		t.Fatalf("final reorg failed: %v", err)
	}
	final, err := index.QueryTxs(TxQuery{PageSize: pageSize})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(final), viewOf(referenceTxPage(queryNewChainBlocks(), pageSize, 0))) {
		t.Fatalf("final first page is not the complete new-chain answer:\n%+v", final)
	}
	var all []TxHit
	query := TxQuery{PageSize: pageSize, Cursor: final.NextCursor}
	all = append(all, final.Hits...)
	for query.Cursor != "" {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("final pagination failed: %v", err)
		}
		all = append(all, page.Hits...)
		query.Cursor = page.NextCursor
	}
	if !reflect.DeepEqual(all, newHits) {
		t.Fatalf("final chain pagination=%v, want %v", all, newHits)
	}
}
