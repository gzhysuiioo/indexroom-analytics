package indexroom

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This file guards the QueryTxs FIRST page against a snapshot Restore that
// successfully replaces the whole main chain with a shorter one. The existing
// Restore/query overlap tests cover QueryTimeStats, and the existing
// QueryTxs overlap tests cover Reorg with an unfiltered query whose totals,
// matched-block count, and page length happen to coincide; neither can tell a
// page whose records are right but whose statistics were drawn from the other
// chain. These tests pin the first page, with a transaction filter, across
// every phase of a successful restore:
//
//   - while the snapshot is still being read (Restore holds no lock): the page
//     completes on the complete OLD chain, untouched by already-read fragments;
//   - once the page has resolved the old tip while the restore replacement is
//     waiting: records, TotalMatches, MatchedBlocks, and the pinned ToHeight
//     all come from the OLD chain;
//   - once the restore has installed the shorter chain: the same first-page
//     query comes wholly from the NEW chain — ToHeight follows the new tip, no
//     removed-height record survives, and old statistics are never stitched
//     onto new records.
//
// The scenario (To left at zero so the first page pins the observed tip; the
// filter is the id set {x,y}; page size 3, below both chains' match totals):
//
//	Old main chain, tip 6 (already ingested):
//	  h1 parent g  txs [x, q, x, w]    // x twice in one block: two occurrences
//	  h2 parent h1 txs [z]             // block with no matching tx
//	  h3 parent h2 txs [w, y, q]       // y among non-matching filler
//	  h4 parent h3 txs [r, r, x, y, y] // y twice; filler before the match
//	  h5 parent h4 txs [y, z]          // only-old height once the chain shrinks
//	  h6 parent h5 txs [p, q]          // trailing block with no matching tx
//	Version-1 snapshot replacing it, tip 4 (shorter; overlap content differs):
//	  j1 parent g  txs [x, y, z]
//	  j2 parent j1 txs [p, p]          // block with no matching tx
//	  j3 parent j2 txs [q, q, w]       // block with no matching tx
//	  j4 parent j3 txs [y, y, s, x]    // y twice in one block
//
// Old-chain answer (range 1..6): 7 matching occurrences over 4 matched
// blocks (h1, h3, h4, h5), pinned ToHeight 6. First page of 3:
//
//	{x h1 #0}, {x h1 #2}, {y h3 #1}
//
// New-chain answer (range 1..4): 5 matching occurrences over 2 matched
// blocks (j1, j4), pinned ToHeight 4. First page of 3:
//
//	{x j1 #0}, {y j1 #1}, {y j4 #0}
//
// The three counts are deliberately all different — page length 3, totals
// 7 vs 5, matched blocks 4 vs 2 — so "records are present and ordered" can
// never stand in for the statistics, and the statistics cover the WHOLE
// pinned range, not the records shown on this page. Every block hash
// differs between the chains, the overlap heights carry different
// transactions, and removed heights 5..6 hold an only-old match (y at h5):
// the two complete answers and any stitched mixture are distinguishable
// record by record.

var (
	restoreTxQuery    = TxQuery{TxIDs: []string{"x", "y"}, PageSize: restoreTxPageSize}
	restoreTxPageSize = 3
)

func restoreQueryOldChainBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"x", "q", "x", "w"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"z"}},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"w", "y", "q"}},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"r", "r", "x", "y", "y"}},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"y", "z"}},
		{Height: 6, Hash: "h6", Parent: "h5", Txs: []string{"p", "q"}},
	}
}

func restoreQueryNewChainBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "j1", Parent: "g", Txs: []string{"x", "y", "z"}},
		{Height: 2, Hash: "j2", Parent: "j1", Txs: []string{"p", "p"}},
		{Height: 3, Hash: "j3", Parent: "j2", Txs: []string{"q", "q", "w"}},
		{Height: 4, Hash: "j4", Parent: "j3", Txs: []string{"y", "y", "s", "x"}},
	}
}

// restoreTxNewSnapshot is the version-1 wire form of restoreQueryNewChainBlocks.
const restoreTxNewSnapshot = `{"version":1,"tip":4,"blocks":[` +
	`{"height":1,"hash":"j1","parent":"g","txs":["x","y","z"]},` +
	`{"height":2,"hash":"j2","parent":"j1","txs":["p","p"]},` +
	`{"height":3,"hash":"j3","parent":"j2","txs":["q","q","w"]},` +
	`{"height":4,"hash":"j4","parent":"j3","txs":["y","y","s","x"]}` +
	`]}`

func buildRestoreQueryOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range restoreQueryOldChainBlocks() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

// restoreFilteredHits is the filtered oracle: every matching occurrence in
// chain in height/position order, plus the number of blocks holding a match.
// It is derived from the supplied chain, not from the Index under test.
func restoreFilteredHits(chain []Block, ids []string) (hits []TxHit, blocks int64) {
	filter := make(map[string]struct{}, len(ids))
	for _, id := range ids {
		filter[id] = struct{}{}
	}
	hits = []TxHit{}
	for _, block := range chain {
		matched := false
		for position, tx := range block.Txs {
			if _, ok := filter[tx]; !ok {
				continue
			}
			hits = append(hits, TxHit{
				Height:    block.Height,
				BlockHash: block.Hash,
				TxID:      tx,
				Position:  position,
			})
			matched = true
		}
		if matched {
			blocks++
		}
	}
	return hits, blocks
}

// restoreReferenceTxPage is one first/continuing page of the complete
// filtered answer for chain (heights 1..len(chain)) sliced at offset, with
// the whole-range statistics the page must carry. The cursor is excluded;
// callers exercise it by actually following it.
func restoreReferenceTxPage(chain []Block, pageSize int, offset int64) TxPage {
	full, blocks := restoreFilteredHits(chain, restoreTxQuery.TxIDs)
	rest := full[offset:]
	end := pageSize
	if end > len(rest) {
		end = len(rest)
	}
	return TxPage{
		Hits:          append([]TxHit{}, rest[:end]...),
		TotalMatches:  int64(len(full)),
		MatchedBlocks: blocks,
		ToHeight:      int64(len(chain)),
	}
}

// Hand-derived first pages, so the guarantee rests on the literal blocks of
// each chain rather than on an oracle-vs-DeepEqual alone.
func literalRestoreTxOldFirstPage() TxPage {
	return TxPage{
		Hits: []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "x", Position: 0},
			{Height: 1, BlockHash: "h1", TxID: "x", Position: 2},
			{Height: 3, BlockHash: "h3", TxID: "y", Position: 1},
		},
		TotalMatches:  7,
		MatchedBlocks: 4,
		ToHeight:      6,
	}
}

func literalRestoreTxNewFirstPage() TxPage {
	return TxPage{
		Hits: []TxHit{
			{Height: 1, BlockHash: "j1", TxID: "x", Position: 0},
			{Height: 1, BlockHash: "j1", TxID: "y", Position: 1},
			{Height: 4, BlockHash: "j4", TxID: "y", Position: 0},
		},
		TotalMatches:  5,
		MatchedBlocks: 2,
		ToHeight:      4,
	}
}

// The counting and ordering semantics the regression rests on, asserted for
// each chain and cross-checked against the live index before and after the
// restore.
func TestRestoreQueryTxsFixtureSemanticsOnBothChains(t *testing.T) {
	oldChain := restoreQueryOldChainBlocks()
	newChain := restoreQueryNewChainBlocks()

	oldHits, oldBlocks := restoreFilteredHits(oldChain, restoreTxQuery.TxIDs)
	wantOld := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "x", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "x", Position: 2},
		{Height: 3, BlockHash: "h3", TxID: "y", Position: 1},
		{Height: 4, BlockHash: "h4", TxID: "x", Position: 2},
		{Height: 4, BlockHash: "h4", TxID: "y", Position: 3},
		{Height: 4, BlockHash: "h4", TxID: "y", Position: 4},
		{Height: 5, BlockHash: "h5", TxID: "y", Position: 0},
	}
	if !reflect.DeepEqual(oldHits, wantOld) {
		t.Fatalf("old filtered hits=%v, want %v", oldHits, wantOld)
	}
	if oldBlocks != 4 {
		t.Fatalf("old matched blocks=%d, want 4 (h1,h3,h4,h5)", oldBlocks)
	}

	newHits, newBlocks := restoreFilteredHits(newChain, restoreTxQuery.TxIDs)
	wantNew := []TxHit{
		{Height: 1, BlockHash: "j1", TxID: "x", Position: 0},
		{Height: 1, BlockHash: "j1", TxID: "y", Position: 1},
		{Height: 4, BlockHash: "j4", TxID: "y", Position: 0},
		{Height: 4, BlockHash: "j4", TxID: "y", Position: 1},
		{Height: 4, BlockHash: "j4", TxID: "x", Position: 3},
	}
	if !reflect.DeepEqual(newHits, wantNew) {
		t.Fatalf("new filtered hits=%v, want %v", newHits, wantNew)
	}
	if newBlocks != 2 {
		t.Fatalf("new matched blocks=%d, want 2 (j1,j4)", newBlocks)
	}

	// Setup invariants that make old/new/mixture answers distinguishable.
	//
	// Every overlap height carries different content under a different hash;
	// a removed height holds an only-old match, and the shorter chain carries
	// only-new matches. Restore replaces height 1 as well, so the chains
	// share no block hash and no record is explainable by both chains.
	if containsHit(newHits, TxHit{Height: 5, BlockHash: "h5", TxID: "y", Position: 0}) {
		t.Fatal("removed-height match y@h5 must not exist on the new chain")
	}
	if !containsHit(oldHits, TxHit{Height: 5, BlockHash: "h5", TxID: "y", Position: 0}) {
		t.Fatal("old chain must keep y@h5, the match confined to a removed height")
	}
	if !containsHit(newHits, TxHit{Height: 4, BlockHash: "j4", TxID: "y", Position: 1}) {
		t.Fatal("new chain must keep its only-new y@j4#1 occurrence")
	}
	if containsHit(oldHits, TxHit{Height: 4, BlockHash: "j4", TxID: "y", Position: 1}) {
		t.Fatal("j4 occurrence must not exist on the old chain")
	}
	// Duplicate matching ids survive with multiplicity, exact position, and
	// the block hash of the observed chain.
	if !containsHit(oldHits, TxHit{Height: 1, BlockHash: "h1", TxID: "x", Position: 2}) ||
		!containsHit(newHits, TxHit{Height: 4, BlockHash: "j4", TxID: "y", Position: 1}) {
		t.Fatal("duplicate in-block matching ids must be kept by occurrence")
	}
	requireOrdered(t, oldHits)
	requireOrdered(t, newHits)

	// The three counts must not coincide on either chain, so correct records
	// cannot mask statistics drawn from the wrong chain.
	pageLen := int64(restoreTxPageSize)
	for name, totals := range map[string][2]int64{
		"old": {int64(len(oldHits)), oldBlocks},
		"new": {int64(len(newHits)), newBlocks},
	} {
		if totals[0] == totals[1] || totals[0] == pageLen || totals[1] == pageLen {
			t.Fatalf("%s chain setup: total=%d blocks=%d page=%d must all differ",
				name, totals[0], totals[1], pageLen)
		}
	}
	if int64(len(oldHits)) == int64(len(newHits)) || oldBlocks == newBlocks {
		t.Fatal("old and new totals must differ so the complete answers are distinct")
	}
	// First pages leave matches unpaginated on both chains, and statistics
	// span the whole range rather than the page.
	if int64(len(literalRestoreTxOldFirstPage().Hits)) != pageLen ||
		int64(len(literalRestoreTxNewFirstPage().Hits)) != pageLen {
		t.Fatal("test setup: both literal first pages must be exactly one page long")
	}

	// Oracle pages must equal the hand-derived literal pages.
	if got := restoreReferenceTxPage(oldChain, restoreTxPageSize, 0); !reflect.DeepEqual(got, literalRestoreTxOldFirstPage()) {
		t.Fatalf("old oracle page=%+v", got)
	}
	if got := restoreReferenceTxPage(newChain, restoreTxPageSize, 0); !reflect.DeepEqual(got, literalRestoreTxNewFirstPage()) {
		t.Fatalf("new oracle page=%+v", got)
	}

	// The oracle must match the live index on both complete chain states.
	index := buildRestoreQueryOldChain(t)
	live, err := index.QueryTxs(restoreTxQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(live), viewOf(literalRestoreTxOldFirstPage())) {
		t.Fatalf("live old-chain query=%+v, want literal old first page", live)
	}
	if live.NextCursor == "" {
		t.Fatal("old first page over 7 matches with page size 3 must carry a cursor")
	}

	if err := index.Restore(strings.NewReader(restoreTxNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	if index.Tip != 4 {
		t.Fatalf("tip after restore=%d, want 4", index.Tip)
	}
	live, err = index.QueryTxs(restoreTxQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(live), viewOf(literalRestoreTxNewFirstPage())) {
		t.Fatalf("live new-chain query=%+v, want literal new first page", live)
	}
	if live.NextCursor == "" {
		t.Fatal("new first page over 5 matches with page size 3 must carry a cursor")
	}
}

// Plain before/after behavior through the public API: after a successful
// restore the first page reflects only the snapshot chain.
func TestQueryTxsBeforeAndAfterRestore(t *testing.T) {
	index := buildRestoreQueryOldChain(t)

	before, err := index.QueryTxs(restoreTxQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(before), viewOf(literalRestoreTxOldFirstPage())) {
		t.Fatalf("pre-restore page=%+v, want old first page", before)
	}

	if err := index.Restore(strings.NewReader(restoreTxNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	if index.Tip != 4 {
		t.Fatalf("tip=%d, want restored tip 4", index.Tip)
	}

	after, err := index.QueryTxs(restoreTxQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(after), viewOf(literalRestoreTxNewFirstPage())) {
		t.Fatalf("post-restore page not wholly on the new chain:\n%+v", after)
	}
	// No residue of the replaced chain: removed heights and hashes are gone,
	// and no removed-height match leaks into the answer or the statistics.
	for _, height := range []int64{5, 6} {
		if _, ok := index.Blocks[height]; ok {
			t.Fatalf("replaced height %d still present", height)
		}
	}
	for _, gone := range []string{"h1", "h2", "h3", "h4", "h5", "h6"} {
		if _, ok := index.ByHash[gone]; ok {
			t.Fatalf("replaced hash %s still indexed", gone)
		}
	}
	for _, hit := range after.Hits {
		if hit.Height > 4 || strings.HasPrefix(hit.BlockHash, "h") {
			t.Fatalf("removed-chain record leaked into post-restore page: %+v", hit)
		}
	}
	if after.ToHeight != 4 || after.TotalMatches != 5 || after.MatchedBlocks != 2 {
		t.Fatalf("old tip or statistics leaked into post-restore page: %+v", after)
	}
}

// While the snapshot is still being read the restore holds no lock: a first
// page issued then completes on the complete OLD chain, and the fragments
// already consumed by the parser have no effect on it. Once the read
// finishes and the replacement lands, the same query answers from the new
// chain.
func TestQueryTxsWhileRestoreStillReading(t *testing.T) {
	index := buildRestoreQueryOldChain(t)
	// Park the read just before the second block object: the first snapshot
	// block has already been consumed, but the replacement is nowhere near
	// applied and Restore holds no lock.
	reader := newGatedReader(t, restoreTxNewSnapshot, `{"height":2`)

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(reader)
	}()
	<-reader.entered

	page, err := index.QueryTxs(restoreTxQuery)
	if err != nil {
		t.Fatalf("query during snapshot read failed: %v", err)
	}
	if !reflect.DeepEqual(viewOf(page), viewOf(literalRestoreTxOldFirstPage())) {
		t.Fatalf("query during snapshot read must see the complete old chain:\n%+v", page)
	}
	select {
	case err := <-restoreDone:
		t.Fatalf("restore returned while its input was still gated: %v", err)
	default:
	}

	close(reader.release)
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore after release failed: %v", err)
	}
	page, err = index.QueryTxs(restoreTxQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(page), viewOf(literalRestoreTxNewFirstPage())) {
		t.Fatalf("query after completed restore must see the complete new chain:\n%+v", page)
	}
}

// restoreQueryHooks are deterministic rendezvous for the QueryTxs/Restore
// overlap tests: a parked first page holds the lock with the tip-bound
// height already resolved; a parked restore has the new chain fully
// installed but has not returned and still holds the lock.
type restoreQueryHooks struct {
	firstParked  chan struct{}
	releaseFirst chan struct{}

	restoreParked  chan struct{}
	releaseRestore chan struct{}

	allowedRestoreTip int64
	firstOnce         sync.Once
	restoreOnce       sync.Once
}

func installRestoreQueryHooks(t *testing.T, parkFirst bool, restoreTip int64) *restoreQueryHooks {
	t.Helper()
	h := &restoreQueryHooks{
		allowedRestoreTip: restoreTip,
		restoreParked:     make(chan struct{}),
		releaseRestore:    make(chan struct{}),
	}
	if parkFirst {
		h.firstParked = make(chan struct{})
		h.releaseFirst = make(chan struct{})
	}
	queryTxsHookLocked = func(from, to int64, continuation bool) {
		if continuation || h.firstParked == nil {
			return // only first pages participate in the rendezvous
		}
		if from != 1 {
			t.Errorf("query hook resolved from=%d, want 1", from)
		}
		h.firstOnce.Do(func() { close(h.firstParked) })
		<-h.releaseFirst
	}
	restoreAppliedHookLocked = func(newTip int64) {
		if newTip != h.allowedRestoreTip {
			t.Errorf("restore hook saw newTip=%d, want %d", newTip, h.allowedRestoreTip)
		}
		h.restoreOnce.Do(func() { close(h.restoreParked) })
		<-h.releaseRestore
	}
	t.Cleanup(func() {
		queryTxsHookLocked = nil
		restoreAppliedHookLocked = nil
	})
	return h
}

// A first page that has resolved the OLD tip (6) and still holds the lock
// when the restore replacement becomes ready must finish on the complete
// old chain: records, whole-range totals, matched-block count, and pinned
// ToHeight all describe the old chain. The restore applies only afterwards;
// the same query then answers from the new chain.
func TestQueryTxsInFlightObservesCompleteOldChainBeforeRestore(t *testing.T) {
	index := buildRestoreQueryOldChain(t)
	h := installRestoreQueryHooks(t, true, 4)
	oldChain, newChain := restoreQueryOldChainBlocks(), restoreQueryNewChainBlocks()

	result := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, err := index.QueryTxs(restoreTxQuery)
		if err != nil {
			errCh <- err
			return
		}
		result <- page
	}()

	<-h.firstParked // query holds index.mu and has pinned the old tip 6
	restoreDone := make(chan error, 1)
	go func() {
		// A fully readable snapshot parses end-to-end and then blocks on
		// index.mu behind the parked first page.
		restoreDone <- index.Restore(strings.NewReader(restoreTxNewSnapshot))
	}()

	close(h.releaseFirst) // the query scans and finishes on the old chain first
	var got TxPage
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("concurrent query failed: %v", err)
	}
	want := restoreReferenceTxPage(oldChain, restoreTxPageSize, 0)
	if err := classifyTxPage(got, want, restoreReferenceTxPage(newChain, restoreTxPageSize, 0),
		mustRestoreFilteredHits(t, oldChain), mustRestoreFilteredHits(t, newChain)); err != nil {
		t.Fatalf("in-flight first page did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Fatalf("query holding the lock across restore commit must see the old chain:\n%+v", got)
	}
	if !reflect.DeepEqual(got.Hits, literalRestoreTxOldFirstPage().Hits) {
		t.Fatalf("old first page hits=%v, want literal old records", got.Hits)
	}
	if got.NextCursor == "" {
		t.Fatal("old first page over 7 matches must carry a cursor")
	}

	close(h.releaseRestore) // the parked restore can now commit and return
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if index.Tip != 4 {
		t.Fatalf("tip after restore=%d, want 4", index.Tip)
	}

	after, err := index.QueryTxs(restoreTxQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(after), viewOf(literalRestoreTxNewFirstPage())) {
		t.Fatalf("query after restore must see the complete new chain:\n%+v", after)
	}
}

// A first page issued while a successful restore has already installed the
// shorter chain but has not yet returned must wait and then observe the
// complete NEW chain: ToHeight follows the new tip (4), every record (with
// its block hash and position) is a new-chain record, and totals count the
// new range only — never old statistics stitched onto new records.
func TestQueryTxsIssuedAtRestoreCommitSeesNewChain(t *testing.T) {
	index := buildRestoreQueryOldChain(t)
	h := installRestoreQueryHooks(t, false, 4)
	oldChain, newChain := restoreQueryOldChainBlocks(), restoreQueryNewChainBlocks()

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(strings.NewReader(restoreTxNewSnapshot))
	}()
	<-h.restoreParked // new chain installed (tip 4), Restore still holds the lock

	result := make(chan TxPage, 1)
	go func() {
		page, err := index.QueryTxs(restoreTxQuery)
		if err != nil {
			t.Errorf("query during restore commit failed: %v", err)
			return
		}
		result <- page
	}()
	select {
	case got := <-result:
		t.Fatalf("query returned while the restore still held the lock: %+v", got)
	default:
	}

	close(h.releaseRestore)
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	got := <-result
	want := restoreReferenceTxPage(newChain, restoreTxPageSize, 0)
	if err := classifyTxPage(got, restoreReferenceTxPage(oldChain, restoreTxPageSize, 0), want,
		mustRestoreFilteredHits(t, oldChain), mustRestoreFilteredHits(t, newChain)); err != nil {
		t.Fatalf("query across restore commit did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Fatalf("query after restore commit must see the complete new chain:\n%+v", got)
	}
	if !reflect.DeepEqual(got.Hits, literalRestoreTxNewFirstPage().Hits) {
		t.Fatalf("new first page hits=%v, want literal new records", got.Hits)
	}
	if got.ToHeight != 4 || got.TotalMatches != 5 || got.MatchedBlocks != 2 {
		t.Fatalf("new page must carry new-chain statistics and tip: %+v", got)
	}
	for _, hit := range got.Hits {
		if hit.Height > 4 || strings.HasPrefix(hit.BlockHash, "h") {
			t.Fatalf("removed-height record leaked into the restore-commit page: %+v", hit)
		}
	}

	// The cursor mints against the new chain and paginates exactly its
	// filtered occurrences, statistics pinned to the first page throughout.
	query := restoreTxQuery
	query.Cursor = got.NextCursor
	all := append([]TxHit{}, got.Hits...)
	for pageNo := 1; ; pageNo++ {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("new-chain pagination broke at page %d: %v", pageNo, err)
		}
		if page.TotalMatches != 5 || page.MatchedBlocks != 2 || page.ToHeight != 4 {
			t.Fatalf("page %d statistics drifted from the pinned first page: %+v", pageNo, page)
		}
		all = append(all, page.Hits...)
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	if wantAll, _ := restoreFilteredHits(newChain, restoreTxQuery.TxIDs); !reflect.DeepEqual(all, wantAll) {
		t.Fatalf("new-chain pagination=%v, want %v", all, wantAll)
	}
}

func mustRestoreFilteredHits(t *testing.T, chain []Block) []TxHit {
	t.Helper()
	hits, _ := restoreFilteredHits(chain, restoreTxQuery.TxIDs)
	return hits
}

// The complete-chain classifier must reject exactly the mixtures this
// regression targets: right-looking records under the other chain's tip or
// statistics — including a correctly ordered page stitched from both chains
// — while accepting both pure answers.
func TestClassifyRestoreTxPageRejectsMixtures(t *testing.T) {
	oldRef := literalRestoreTxOldFirstPage()
	newRef := literalRestoreTxNewFirstPage()
	oldAll := mustRestoreFilteredHits(t, restoreQueryOldChainBlocks())
	newAll := mustRestoreFilteredHits(t, restoreQueryNewChainBlocks())
	classify := func(page TxPage) error { return classifyTxPage(page, oldRef, newRef, oldAll, newAll) }

	if err := classify(oldRef); err != nil {
		t.Fatalf("pure old page rejected: %v", err)
	}
	if err := classify(newRef); err != nil {
		t.Fatalf("pure new page rejected: %v", err)
	}

	// New-chain records and statistics under the stale OLD tip height: looks
	// internally counted, but claims removed heights 5..6 are in range.
	mixed := newRef
	mixed.ToHeight = oldRef.ToHeight
	if err := classify(mixed); err == nil {
		t.Fatal("new-chain page under the old tip height was accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("new-records/old-tip mixture not reported as MIXED:\n%v", err)
	}

	// Old-chain records and tip with the NEW chain's whole-range statistics:
	// the records are right for the old chain but totals/blocks came from the
	// shorter one — the exact "records correct, statistics from the wrong
	// chain" defect the fixture exists to expose.
	mixed = oldRef
	mixed.TotalMatches = newRef.TotalMatches
	mixed.MatchedBlocks = newRef.MatchedBlocks
	if err := classify(mixed); err == nil {
		t.Fatal("old records with new-chain statistics were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("old-records/new-stats mixture not reported as MIXED:\n%v", err)
	}

	// New-chain records and tip with the OLD chain's whole-range statistics.
	mixed = newRef
	mixed.TotalMatches = oldRef.TotalMatches
	mixed.MatchedBlocks = oldRef.MatchedBlocks
	if err := classify(mixed); err == nil {
		t.Fatal("new records with old-chain statistics were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("new-records/old-stats mixture not reported as MIXED:\n%v", err)
	}

	// Old-chain page that nevertheless claims the shortened NEW tip: it
	// counts removed height 5 while promising ToHeight 4.
	mixed = oldRef
	mixed.ToHeight = newRef.ToHeight
	if err := classify(mixed); err == nil {
		t.Fatal("old records under the new tip height were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("old-records/new-tip mixture not reported as MIXED:\n%v", err)
	}

	// An ordered-looking page that stitches records from both chains:
	// x@h1 exists only on the old chain (restore replaces height 1 too, so
	// even the height-1 hashes differ), y@h3 is old-only, and y@j4 is
	// new-only. Height and position ordering is intact, so ordering-only
	// checks accept it.
	mixed = oldRef
	mixed.Hits = []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "x", Position: 0}, // old only
		{Height: 3, BlockHash: "h3", TxID: "y", Position: 1}, // old only
		{Height: 4, BlockHash: "j4", TxID: "y", Position: 0}, // new only
	}
	if err := classify(mixed); err == nil {
		t.Fatal("stitched hits from both chains were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("stitched-records mixture not reported as MIXED:\n%v", err)
	}

	// Plain corruption: statistics belonging to neither chain.
	corrupt := newRef
	corrupt.MatchedBlocks = 99
	if err := classify(corrupt); err == nil {
		t.Fatal("corrupt statistics matching neither chain were accepted")
	} else if !strings.Contains(err.Error(), "CORRUPT") {
		t.Fatalf("corrupt page not reported as CORRUPT:\n%v", err)
	}
}

// Sustained overlap, no hooks: restores alternate between the old-chain
// snapshot and the shorter new-chain snapshot while readers issue first
// pages continuously. Every page must classify as one complete chain, with
// branch identity confirmed record by record; success never depends on
// which side of a restore a query happened to land. This supplements the
// deterministic rendezvous tests above rather than replacing them.
func TestQueryTxsRepeatedRestoresStayConsistent(t *testing.T) {
	index := buildRestoreQueryOldChain(t)
	oldChain := restoreQueryOldChainBlocks()
	newChain := restoreQueryNewChainBlocks()
	oldSnapshot := exportString(t, index) // version 1: none of the blocks carry times
	if !strings.Contains(oldSnapshot, `"version":1`) {
		t.Fatalf("test setup: old snapshot must be version 1: %s", oldSnapshot)
	}
	oldRef := restoreReferenceTxPage(oldChain, restoreTxPageSize, 0)
	newRef := restoreReferenceTxPage(newChain, restoreTxPageSize, 0)
	oldAll := mustRestoreFilteredHits(t, oldChain)
	newAll := mustRestoreFilteredHits(t, newChain)

	const restorers = 2
	const rounds = 40
	const readers = 4
	const reads = 80

	stop := make(chan struct{})
	var wg sync.WaitGroup
	for r := 0; r < restorers; r++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				select {
				case <-stop:
					return
				default:
				}
				snapshot := restoreTxNewSnapshot
				if (i+seed)%2 == 0 {
					snapshot = oldSnapshot
				}
				if err := index.Restore(strings.NewReader(snapshot)); err != nil {
					t.Errorf("restore failed: %v", err)
					return
				}
			}
		}(r)
	}
	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < reads; i++ {
				select {
				case <-stop:
					return
				default:
				}
				page, err := index.QueryTxs(restoreTxQuery)
				if err != nil {
					t.Errorf("first page failed: %v", err)
					return
				}
				var branchHits []TxHit
				switch page.ToHeight {
				case 6:
					branchHits = oldAll
				case 4:
					branchHits = newAll
				default:
					t.Errorf("first page pinned unknown tip %d", page.ToHeight)
					return
				}
				if err := classifyTxPage(page, oldRef, newRef, oldAll, newAll); err != nil {
					t.Errorf("%v", err)
					return
				}
				if !reflect.DeepEqual(page.Hits, branchHits[:len(page.Hits)]) {
					t.Errorf("first page records do not match the pinned branch:\n%+v", page.Hits)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)

	// Leave the index deterministically on the shorter new chain and verify
	// the complete filtered answer, cursor pagination included.
	if err := index.Restore(strings.NewReader(restoreTxNewSnapshot)); err != nil {
		t.Fatalf("final restore failed: %v", err)
	}
	first, err := index.QueryTxs(restoreTxQuery)
	if err != nil {
		t.Fatal(err)
	}
	if err := classifyTxPage(first, oldRef, newRef, oldAll, newAll); err != nil {
		t.Fatalf("final first page is not one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(first), viewOf(newRef)) {
		t.Fatalf("final first page is not the complete new-chain answer:\n%+v", first)
	}
	query := restoreTxQuery
	query.Cursor = first.NextCursor
	all := append([]TxHit{}, first.Hits...)
	for query.Cursor != "" {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("final pagination failed: %v", err)
		}
		if page.TotalMatches != 5 || page.MatchedBlocks != 2 || page.ToHeight != 4 {
			t.Fatalf("final pagination statistics drifted: %+v", page)
		}
		all = append(all, page.Hits...)
		query.Cursor = page.NextCursor
	}
	if !reflect.DeepEqual(all, newAll) {
		t.Fatalf("final chain pagination=%v, want %v", all, newAll)
	}
}

// A first page pinned to the OLD chain before the restore keeps its semantics
// under guard: here the snapshot replaces the whole chain, so the pinned
// range [1,6] is no longer covered and the old cursor must fail with
// ErrQueryChanged and a zeroed page rather than resume on mixed data.
func TestQueryTxsCursorAfterWholeChainRestoreIsChangedAndZeroed(t *testing.T) {
	index := buildRestoreQueryOldChain(t)
	first, err := index.QueryTxs(restoreTxQuery)
	if err != nil {
		t.Fatal(err)
	}
	if first.ToHeight != 6 || first.TotalMatches != 7 || first.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", first)
	}

	if err := index.Restore(strings.NewReader(restoreTxNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	page, err := index.QueryTxs(TxQuery{
		TxIDs:    restoreTxQuery.TxIDs,
		PageSize: restoreTxPageSize,
		Cursor:   first.NextCursor,
	})
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("old cursor across restore: err=%v page=%+v, want ErrQueryChanged", err, page)
	}
	if !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("changed continuation must return a zeroed page, got %+v", page)
	}
}
