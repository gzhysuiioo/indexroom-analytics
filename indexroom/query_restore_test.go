package indexroom

import (
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This file guards QueryTxs first pages against a snapshot Restore that
// replaces the whole main chain with a SHORTER one. While the snapshot is
// still being read, a first-page query must complete on the complete old
// chain, untouched by the snapshot fragments already consumed. A first page
// that has pinned the old tip and holds the lock when the restore's
// replacement becomes ready must still finish on the complete old chain. A
// first page issued once the new chain is installed must observe only the
// complete new chain: the resolved upper height follows the new tip, no
// transaction from a removed height appears, and no old-chain statistic is
// stitched onto new-chain records.
//
// Unlike the reorg regression (query_reorg_test.go), every query here
// filters by transaction identifier and the page is smaller than either
// chain's match total, so the page's record count, the whole-range match
// count, and the matched-block count are three different numbers on both
// chains — a page whose records are right but whose statistics were taken
// from the other chain cannot accidentally look consistent.
//
// The scenario (filter TxIDs ["m"], PageSize 2, To left at zero so the first
// page pins the observed chain tip):
//
//	Old main chain, tip 5:
//	  h1 txs [m, x, m] // m twice in one block: two occurrences, one block
//	  h2 txs [n]       // no matching transaction
//	  h3 txs [q, m]
//	  h4 txs [z]       // no matching transaction
//	  h5 txs [m, w, m] // m twice again
//	Version-1 snapshot replacing it, tip 4 (every overlapping height
//	carries different transactions):
//	  j1 txs [m]
//	  j2 txs [n, m, m] // m twice; h2 at this height had no match at all
//	  j3 txs [p]       // no matching transaction
//	  j4 txs [m, q]
//
// Old-chain answer (range 1..5): 5 occurrences over 3 matched blocks, tip 5;
// first page [h1/m@0, h1/m@2].
// New-chain answer (range 1..4): 4 occurrences over 3 matched blocks, tip 4;
// first page [j1/m@0, j2/m@1].
//
// The two complete answers differ in the upper height, the match total, and
// every record, so any mixture — old statistics over new records, new-chain
// records under the old tip, a height-5 transaction surviving the restore —
// is visible. On each chain 5/3/2 (old) and 4/3/2 (new) are all distinct, so
// occurrence count, matched-block count, and page record count can never be
// silently swapped for one another.

// queryRestoreTxIDs is the transaction filter shared by every query in this
// file: a single identifier that occurs twice inside one block on both
// chains, so duplicates must be preserved per occurrence.
var queryRestoreTxIDs = []string{"m"}

// queryRestorePageSize is smaller than both chains' match totals (5 and 4),
// so every first page here mints a continuation cursor.
const queryRestorePageSize = 2

func queryRestoreOldChainBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"m", "x", "m"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"n"}},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"q", "m"}},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"z"}},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"m", "w", "m"}},
	}
}

func queryRestoreNewChainBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "j1", Parent: "g", Txs: []string{"m"}},
		{Height: 2, Hash: "j2", Parent: "j1", Txs: []string{"n", "m", "m"}},
		{Height: 3, Hash: "j3", Parent: "j2", Txs: []string{"p"}},
		{Height: 4, Hash: "j4", Parent: "j3", Txs: []string{"m", "q"}},
	}
}

// queryRestoreNewSnapshot is the version-1 wire form of
// queryRestoreNewChainBlocks: a valid snapshot of the shorter replacement
// chain.
const queryRestoreNewSnapshot = `{"version":1,"tip":4,"blocks":[` +
	`{"height":1,"hash":"j1","parent":"g","txs":["m"]},` +
	`{"height":2,"hash":"j2","parent":"j1","txs":["n","m","m"]},` +
	`{"height":3,"hash":"j3","parent":"j2","txs":["p"]},` +
	`{"height":4,"hash":"j4","parent":"j3","txs":["m","q"]}` +
	`]}`

// queryRestoreQuery is the single first-page query shape exercised here: no
// upper bound (the first page pins the observed tip), the shared transaction
// filter, and a page smaller than either chain's match total.
func queryRestoreQuery() TxQuery {
	return TxQuery{TxIDs: queryRestoreTxIDs, PageSize: queryRestorePageSize}
}

func buildQueryRestoreOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range queryRestoreOldChainBlocks() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

// queryRestoreHits is an independent oracle: the complete ordered occurrence
// list for the shared filter, derived from an explicitly supplied chain, not
// from the Index under test. Duplicate identifiers inside one block yield one
// hit per position.
func queryRestoreHits(chain []Block) []TxHit {
	hits := []TxHit{}
	for _, block := range chain {
		for position, tx := range block.Txs {
			if tx != queryRestoreTxIDs[0] {
				continue
			}
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

// queryRestoreMatchedBlocks counts the blocks of chain holding at least one
// matching transaction; blocks without a match contribute nothing.
func queryRestoreMatchedBlocks(chain []Block) int64 {
	var blocks int64
	for _, block := range chain {
		for _, tx := range block.Txs {
			if tx == queryRestoreTxIDs[0] {
				blocks++
				break
			}
		}
	}
	return blocks
}

// queryRestorePage is one page of the complete filtered answer for chain
// (heights 1..len(chain)) sliced at offset, with the whole-range statistics
// the page must carry. The continuation cursor is deliberately excluded; it
// is validated separately by decoding it.
func queryRestorePage(chain []Block, pageSize int, offset int64) TxPage {
	full := queryRestoreHits(chain)
	rest := full[offset:]
	end := pageSize
	if end > len(rest) {
		end = len(rest)
	}
	return TxPage{
		Hits:          append([]TxHit{}, rest[:end]...),
		TotalMatches:  int64(len(full)),
		MatchedBlocks: queryRestoreMatchedBlocks(chain),
		ToHeight:      int64(len(chain)),
	}
}

// Hand-derived first pages, so the guarantee rests on the literal
// transactions of each chain rather than on oracle-vs-DeepEqual alone.
func literalQueryRestoreOldFirstPage() TxPage {
	return TxPage{
		Hits: []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "m", Position: 0},
			{Height: 1, BlockHash: "h1", TxID: "m", Position: 2},
		},
		TotalMatches:  5,
		MatchedBlocks: 3,
		ToHeight:      5,
	}
}

func literalQueryRestoreNewFirstPage() TxPage {
	return TxPage{
		Hits: []TxHit{
			{Height: 1, BlockHash: "j1", TxID: "m", Position: 0},
			{Height: 2, BlockHash: "j2", TxID: "m", Position: 1},
		},
		TotalMatches:  4,
		MatchedBlocks: 3,
		ToHeight:      4,
	}
}

// classifyQueryRestorePage accepts got only when its visible contents are the
// first-page view of exactly one of the two complete chains.
func classifyQueryRestorePage(got TxPage) error {
	return classifyTxPage(got,
		queryRestorePage(queryRestoreOldChainBlocks(), queryRestorePageSize, 0),
		queryRestorePage(queryRestoreNewChainBlocks(), queryRestorePageSize, 0),
		queryRestoreHits(queryRestoreOldChainBlocks()),
		queryRestoreHits(queryRestoreNewChainBlocks()))
}

// The fixture properties the regression rests on, asserted directly for each
// chain and cross-checked against the live index before and after a restore.
func TestQueryTxsRestoreFixtureSemanticsOnBothChains(t *testing.T) {
	oldChain, newChain := queryRestoreOldChainBlocks(), queryRestoreNewChainBlocks()
	oldHits, newHits := queryRestoreHits(oldChain), queryRestoreHits(newChain)

	// The duplicate identifier inside h1 yields two occurrences at their
	// exact positions; j2's pair sits at positions 1 and 2.
	for _, want := range []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "m", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "m", Position: 2},
		{Height: 5, BlockHash: "h5", TxID: "m", Position: 0},
		{Height: 5, BlockHash: "h5", TxID: "m", Position: 2},
	} {
		if !containsHit(oldHits, want) {
			t.Fatalf("old chain misses literal occurrence %+v in %v", want, oldHits)
		}
	}
	for _, want := range []TxHit{
		{Height: 2, BlockHash: "j2", TxID: "m", Position: 1},
		{Height: 2, BlockHash: "j2", TxID: "m", Position: 2},
	} {
		if !containsHit(newHits, want) {
			t.Fatalf("new chain misses literal occurrence %+v in %v", want, newHits)
		}
	}

	// Occurrence count, matched-block count, and page record count are three
	// different numbers on both chains, and the two chains disagree on the
	// match total and the tip: no field can stand in for another.
	if len(oldHits) != 5 || queryRestoreMatchedBlocks(oldChain) != 3 {
		t.Fatalf("old chain: matches=%d blocks=%d, want 5/3", len(oldHits), queryRestoreMatchedBlocks(oldChain))
	}
	if len(newHits) != 4 || queryRestoreMatchedBlocks(newChain) != 3 {
		t.Fatalf("new chain: matches=%d blocks=%d, want 4/3", len(newHits), queryRestoreMatchedBlocks(newChain))
	}
	if queryRestorePageSize >= len(oldHits) || queryRestorePageSize >= len(newHits) {
		t.Fatalf("page size %d must be smaller than both match totals", queryRestorePageSize)
	}

	// Blocks without any matching transaction exist on both chains and
	// contribute neither hits nor matched blocks.
	for _, hash := range []string{"h2", "h4"} {
		for _, hit := range oldHits {
			if hit.BlockHash == hash {
				t.Fatalf("non-matching block %s produced a hit: %+v", hash, hit)
			}
		}
	}
	for _, hit := range newHits {
		if hit.BlockHash == "j3" {
			t.Fatalf("non-matching block j3 produced a hit: %+v", hit)
		}
	}

	// Every overlapping height carries different transactions on the two
	// chains, and no record above the new tip can belong to the new chain.
	for height := int64(1); height <= 4; height++ {
		if !reflect.DeepEqual(oldChain[height-1].Txs, newChain[height-1].Txs) {
			continue
		}
		t.Fatalf("setup invariant broken: height %d carries identical transactions on both chains", height)
	}
	for _, hit := range newHits {
		if hit.Height > 4 {
			t.Fatalf("new-chain hit above its tip: %+v", hit)
		}
	}
	requireOrdered(t, oldHits)
	requireOrdered(t, newHits)

	// The oracle must match the literal hand-derived pages.
	if got, want := queryRestorePage(oldChain, queryRestorePageSize, 0), literalQueryRestoreOldFirstPage(); !reflect.DeepEqual(got, want) {
		t.Fatalf("old-chain oracle page=%+v\nwant=%+v", got, want)
	}
	if got, want := queryRestorePage(newChain, queryRestorePageSize, 0), literalQueryRestoreNewFirstPage(); !reflect.DeepEqual(got, want) {
		t.Fatalf("new-chain oracle page=%+v\nwant=%+v", got, want)
	}

	// The live index matches the oracle on both complete chain states.
	index := buildQueryRestoreOldChain(t)
	before, err := index.QueryTxs(queryRestoreQuery())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(before), viewOf(literalQueryRestoreOldFirstPage())) {
		t.Fatalf("pre-restore first page=%+v, want literal old page", before)
	}
	if before.NextCursor == "" {
		t.Fatal("old first page over 5 matches must carry a cursor")
	}

	if err := index.Restore(strings.NewReader(queryRestoreNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	if index.Tip != 4 {
		t.Fatalf("tip=%d, want restored tip 4", index.Tip)
	}
	if _, ok := index.Blocks[5]; ok {
		t.Fatal("removed height 5 still present after restore")
	}
	for _, gone := range []string{"h1", "h2", "h3", "h4", "h5"} {
		if _, ok := index.ByHash[gone]; ok {
			t.Fatalf("replaced hash %s still indexed", gone)
		}
	}

	after, err := index.QueryTxs(queryRestoreQuery())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(after), viewOf(literalQueryRestoreNewFirstPage())) {
		t.Fatalf("post-restore first page not wholly on the new chain:\n%+v", after)
	}
	for _, hit := range after.Hits {
		if hit.Height > 4 {
			t.Fatalf("transaction from a removed height survived the restore: %+v", hit)
		}
	}
}

// While the snapshot is still being read, the restore holds no lock: a
// first-page query issued at that moment completes and answers from the
// complete old chain, even though the snapshot's first block has already
// been consumed. Once the read finishes and the replacement lands, the same
// query answers from the complete new chain.
func TestQueryTxsFirstPageWhileRestoreStillReading(t *testing.T) {
	index := buildQueryRestoreOldChain(t)
	// Park the read just before the second block object: the first block of
	// the snapshot has been fully consumed, the replacement is nowhere near
	// applied.
	reader := newGatedReader(t, queryRestoreNewSnapshot, `{"height":2`)

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(reader)
	}()
	<-reader.entered // restore is parked mid-stream, holding no lock

	page, err := index.QueryTxs(queryRestoreQuery())
	if err != nil {
		t.Fatalf("first page during snapshot read failed: %v", err)
	}
	if err := classifyQueryRestorePage(page); err != nil {
		t.Fatalf("first page during snapshot read is not one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(page), viewOf(literalQueryRestoreOldFirstPage())) {
		t.Fatalf("first page during snapshot read must see the complete old chain:\n%+v", page)
	}
	if page.ToHeight != 5 || page.TotalMatches != 5 || page.MatchedBlocks != 3 {
		t.Fatalf("snapshot fragments leaked into the in-read page: %+v", page)
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
	page, err = index.QueryTxs(queryRestoreQuery())
	if err != nil {
		t.Fatal(err)
	}
	if err := classifyQueryRestorePage(page); err != nil {
		t.Fatalf("first page after completed restore is not one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(page), viewOf(literalQueryRestoreNewFirstPage())) {
		t.Fatalf("first page after completed restore must see the complete new chain:\n%+v", page)
	}
}

// A first page that has pinned the OLD tip (5) and still holds the lock when
// the restore's replacement is ready must finish on the complete old chain —
// records, match total, matched-block count, and the resolved upper height
// all come from the old chain; the restore applies only afterwards.
func TestQueryTxsFirstPageInFlightObservesCompleteOldChainBeforeRestore(t *testing.T) {
	index := buildQueryRestoreOldChain(t)

	queryParked := make(chan struct{})
	releaseQuery := make(chan struct{})
	var hookOnce sync.Once
	queryTxsHookLocked = func(from, to int64, continuation bool) {
		if continuation {
			t.Errorf("unexpected continuation in a first-page test")
			return
		}
		if from != 1 || to != 5 {
			t.Errorf("first page pinned range [%d,%d], want [1,5] on the old chain", from, to)
		}
		hookOnce.Do(func() { close(queryParked) })
		<-releaseQuery
	}
	t.Cleanup(func() { queryTxsHookLocked = nil })

	result := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, err := index.QueryTxs(queryRestoreQuery())
		if err != nil {
			errCh <- err
			return
		}
		result <- page
	}()
	<-queryParked // query holds index.mu and has pinned tip 5

	restoreDone := make(chan error, 1)
	go func() {
		// strings.Reader is fully readable, so this restore parses the whole
		// snapshot and then blocks on index.mu behind the parked query.
		restoreDone <- index.Restore(strings.NewReader(queryRestoreNewSnapshot))
	}()

	close(releaseQuery) // the query scans and finishes on the old chain first
	var got TxPage
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("concurrent query failed: %v", err)
	}
	if err := classifyQueryRestorePage(got); err != nil {
		t.Fatalf("in-flight first page did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(literalQueryRestoreOldFirstPage())) {
		t.Fatalf("query holding the lock across restore commit must see the old chain:\n%+v", got)
	}
	if got.NextCursor == "" {
		t.Fatal("old first page over 5 matches must carry a cursor")
	}
	// The cursor minted against the old chain pins tip 5 at offset 2.
	payload := decodePayload(t, index, got.NextCursor)
	if payload.To != 5 || payload.Off != 2 {
		t.Fatalf("cursor pins to=%d off=%d, want 5/2", payload.To, payload.Off)
	}

	queryTxsHookLocked = nil // the rendezvous is spent; later queries run free
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	if index.Tip != 4 {
		t.Fatalf("tip after restore=%d, want 4", index.Tip)
	}

	// The cursor issued by the old-chain first page is dead: continuing it
	// must fail with ErrQueryChanged and a fully zeroed page rather than
	// resume on the restored chain.
	page, err := index.QueryTxs(TxQuery{TxIDs: queryRestoreTxIDs, PageSize: queryRestorePageSize, Cursor: got.NextCursor})
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("old cursor across restore: err=%v page=%+v, want ErrQueryChanged", err, page)
	}
	if !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("changed continuation must return a zeroed page, got %+v", page)
	}

	// A fresh first page under the same conditions answers wholly from the
	// shorter new chain.
	fresh, err := index.QueryTxs(queryRestoreQuery())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(fresh), viewOf(literalQueryRestoreNewFirstPage())) {
		t.Fatalf("first page after restore must see the complete new chain:\n%+v", fresh)
	}
}

// A first page issued while a successful restore has installed the new chain
// but not yet returned must wait and then observe the complete NEW chain:
// the resolved upper height follows the new tip, no removed-height
// transaction appears, and the statistics are the new chain's own.
func TestQueryTxsFirstPageIssuedAtRestoreCommitSeesNewChain(t *testing.T) {
	index := buildQueryRestoreOldChain(t)

	restoreParked := make(chan struct{})
	releaseRestore := make(chan struct{})
	var hookOnce sync.Once
	restoreAppliedHookLocked = func(newTip int64) {
		if newTip != 4 {
			t.Errorf("restore hook saw newTip=%d, want 4", newTip)
		}
		hookOnce.Do(func() { close(restoreParked) })
		<-releaseRestore
	}
	t.Cleanup(func() { restoreAppliedHookLocked = nil })

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(strings.NewReader(queryRestoreNewSnapshot))
	}()
	<-restoreParked // new chain installed (tip 4), Restore still holds the lock

	result := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, err := index.QueryTxs(queryRestoreQuery())
		if err != nil {
			errCh <- err
			return
		}
		result <- page
	}()
	select {
	case page := <-result:
		t.Fatalf("first page returned while the restore still held the lock: %+v", page)
	case err := <-errCh:
		t.Fatalf("first page failed while the restore still held the lock: %v", err)
	default:
	}

	close(releaseRestore)
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	var got TxPage
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("first page across restore commit failed: %v", err)
	}
	if err := classifyQueryRestorePage(got); err != nil {
		t.Fatalf("first page across restore commit did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(literalQueryRestoreNewFirstPage())) {
		t.Fatalf("first page after restore commit must see the complete new chain:\n%+v", got)
	}
	if got.ToHeight != 4 || got.TotalMatches != 4 || got.MatchedBlocks != 3 {
		t.Fatalf("old-chain statistics leaked into the new-chain page: %+v", got)
	}
	for _, hit := range got.Hits {
		if hit.Height > got.ToHeight {
			t.Fatalf("hit above the pinned new tip: %+v", hit)
		}
	}
	if got.NextCursor == "" {
		t.Fatal("new first page over 4 matches must carry a cursor")
	}
	// The cursor pins the NEW chain (tip 4, offset 2): following it drains
	// exactly the remaining new-chain occurrences, nothing from the old one.
	payload := decodePayload(t, index, got.NextCursor)
	if payload.To != 4 || payload.Off != 2 {
		t.Fatalf("cursor pins to=%d off=%d, want 4/2", payload.To, payload.Off)
	}
	rest, err := index.QueryTxs(TxQuery{TxIDs: queryRestoreTxIDs, PageSize: queryRestorePageSize, Cursor: got.NextCursor})
	if err != nil {
		t.Fatalf("new-chain continuation failed: %v", err)
	}
	all := append(append([]TxHit{}, got.Hits...), rest.Hits...)
	if rest.NextCursor != "" {
		t.Fatalf("4 matches over page size 2 must end after the second page, got another cursor")
	}
	if !reflect.DeepEqual(all, queryRestoreHits(queryRestoreNewChainBlocks())) {
		t.Fatalf("new-chain pagination=%v, want %v", all, queryRestoreHits(queryRestoreNewChainBlocks()))
	}
	if rest.TotalMatches != 4 || rest.MatchedBlocks != 3 || rest.ToHeight != 4 {
		t.Fatalf("continuation statistics drifted from the pinned first page: %+v", rest)
	}
}

// Sustained overlap with no hooks: restores alternate between the old-chain
// snapshot and the shorter new-chain snapshot while readers issue first-page
// queries continuously. Timing-independent — every first page must classify
// as one complete chain, its records must be the exact ordered prefix of that
// chain's occurrence list, its statistics must be that chain's own, and the
// cursor it mints must pin that chain's tip. A page mixing old statistics
// with new records (or vice versa) fails under any scheduling.
func TestQueryTxsRepeatedRestoresStayConsistent(t *testing.T) {
	index := buildQueryRestoreOldChain(t)
	oldSnapshot := exportString(t, index) // version 1: no timestamps anywhere
	oldChain, newChain := queryRestoreOldChainBlocks(), queryRestoreNewChainBlocks()
	oldHits, newHits := queryRestoreHits(oldChain), queryRestoreHits(newChain)

	const restorers = 2
	const rounds = 30
	const readers = 4
	const reads = 60

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
				snapshot := queryRestoreNewSnapshot
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
				page, err := index.QueryTxs(queryRestoreQuery())
				if err != nil {
					t.Errorf("first page failed: %v", err)
					return
				}
				var branchHits []TxHit
				var branchBlocks int64
				switch page.ToHeight {
				case 5:
					branchHits, branchBlocks = oldHits, queryRestoreMatchedBlocks(oldChain)
				case 4:
					branchHits, branchBlocks = newHits, queryRestoreMatchedBlocks(newChain)
				default:
					t.Errorf("first page pinned unknown tip %d", page.ToHeight)
					return
				}
				if page.TotalMatches != int64(len(branchHits)) || page.MatchedBlocks != branchBlocks {
					t.Errorf("statistics %d/%d do not belong to the chain pinned at tip %d (%d/%d)",
						page.TotalMatches, page.MatchedBlocks, page.ToHeight, len(branchHits), branchBlocks)
					return
				}
				if err := classifyQueryRestorePage(page); err != nil {
					t.Errorf("%v", err)
					return
				}
				if !reflect.DeepEqual(page.Hits, branchHits[:len(page.Hits)]) {
					t.Errorf("first page records are not the ordered prefix of the pinned chain:\n%+v", page.Hits)
					return
				}
				if page.NextCursor == "" {
					t.Errorf("first page over %d matches must carry a cursor", len(branchHits))
					return
				}
				payload, err := index.decodeCursor(page.NextCursor)
				if err != nil {
					t.Errorf("cursor not decodable on its own index: %v", err)
					return
				}
				if payload.To != page.ToHeight || payload.Off != int64(len(page.Hits)) {
					t.Errorf("cursor pins to=%d off=%d, want %d/%d",
						payload.To, payload.Off, page.ToHeight, len(page.Hits))
					return
				}
			}
		}()
	}
	wg.Wait()
	close(stop)

	// Leave the index deterministically on the shorter new chain and verify
	// the complete answer against the oracle.
	if err := index.Restore(strings.NewReader(queryRestoreNewSnapshot)); err != nil {
		t.Fatalf("final restore failed: %v", err)
	}
	final, err := index.QueryTxs(queryRestoreQuery())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(viewOf(final), viewOf(literalQueryRestoreNewFirstPage())) {
		t.Fatalf("final first page is not the complete new-chain answer:\n%+v", final)
	}
}
