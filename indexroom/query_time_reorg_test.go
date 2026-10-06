package indexroom

import (
	"fmt"
	"reflect"
	"testing"
)

// This file guards QueryTxs with a timestamp window against the subtlest
// reorg shape: the alternate branch keeps every block hash, every parent
// link, and every transaction list of the replaced range, and rewrites ONLY
// block timestamps. Occurrences that were inside the window leave it and
// previously excluded occurrences enter it, while the chain tip, the height
// range, and the raw transaction content stay exactly as they were. A first
// page overlapping such a reorg must still answer from ONE complete chain —
// old times or new times — with hits, TotalMatches, MatchedBlocks, and the
// pinned ToHeight all describing that same state; a cursor minted against
// the old times must die with ErrQueryChanged once the new times commit,
// because the range fingerprint hashes every timestamp.
//
// The scenario (filter TxIDs ["m"], window [0, 100), page size 2):
//
//	Retained ancestor (both chains), time 10 — inside the window:
//	  h1 parent g  txs [m, m]     // same id twice in one block: two
//	                              // occurrences, one matched block
//	Old times, tip 5:             New times, tip 5 (same hashes/txs):
//	  h2 txs [m]     time MISSING   h2 txs [m]     time 0   (real zero)
//	  h3 txs [m]     time 50        h3 txs [m]     time 500
//	  h4 txs [x,m,x] time 300       h4 txs [x,m,x] time 20
//	  h5 txs [m, m]  time 90        h5 txs [m, m]  time MISSING
//
// Old-chain answer over [0,100): h1, h3, h5 survive -> 5 occurrences
// (h1 and h5 each hold the id twice) over 3 matched blocks, tip 5.
// New-chain answer over [0,100): h1, h2, h4 survive -> 4 occurrences
// over 3 matched blocks, tip 5. Height 2 enters only because a REAL zero
// timestamp is not a missing one; height 5 leaves by becoming missing.
// Timestamps decrease with height on the new chain (0 at h2, 500 at h3,
// 20 at h4), yet hits must still come out by height and block position.

// timeReorgWindow is the half-open window [0, 100): it includes a genuine
// zero timestamp, which is what keeps the missing-vs-zero distinction under
// test. Fresh pointers per call mirror how callers build their queries.
func timeReorgWindow() (start, end *int64) {
	return intptr(0), intptr(100)
}

func timeReorgQuery() TxQuery {
	start, end := timeReorgWindow()
	return TxQuery{TxIDs: []string{"m"}, TimeStart: start, TimeEnd: end, PageSize: 2}
}

func timeReorgRetainedBlock() Block {
	return Block{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"m", "m"}, Time: intptr(10)}
}

// timeReorgOldSuffix: heights 2..5 with the original timestamps.
func timeReorgOldSuffix() []Block {
	return []Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"m"}, Time: nil},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"m"}, Time: intptr(50)},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"x", "m", "x"}, Time: intptr(300)},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"m", "m"}, Time: intptr(90)},
	}
}

// timeReorgNewSuffix: identical heights, hashes, parents, and transaction
// lists; only the timestamps changed — missing->0, 50->500, 300->20, 90->
// missing — moving h2 and h4 into the window and h3 and h5 out of it.
func timeReorgNewSuffix() []Block {
	return []Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"m"}, Time: intptr(0)},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"m"}, Time: intptr(500)},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"x", "m", "x"}, Time: intptr(20)},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"m", "m"}, Time: nil},
	}
}

func timeReorgOldChain() []Block {
	return append([]Block{timeReorgRetainedBlock()}, timeReorgOldSuffix()...)
}

func timeReorgNewChain() []Block {
	return append([]Block{timeReorgRetainedBlock()}, timeReorgNewSuffix()...)
}

func buildTimeReorgOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range timeReorgOldChain() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

// referenceWindowedHits is an independent oracle: it derives the complete
// ordered occurrence list for one window and identifier set from an
// explicitly supplied chain, not from the Index under test, so a shared
// QueryTxs bug cannot make an expectation match itself.
func referenceWindowedHits(chain []Block, window timeWindow, ids ...string) []TxHit {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	hits := []TxHit{}
	for _, block := range chain {
		if !window.contains(block.Time) {
			continue
		}
		for position, tx := range block.Txs {
			if len(want) > 0 && !want[tx] {
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

func referenceWindowedMatchedBlocks(chain []Block, window timeWindow, ids ...string) int64 {
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var blocks int64
	for _, block := range chain {
		if !window.contains(block.Time) {
			continue
		}
		for _, tx := range block.Txs {
			if len(want) > 0 && !want[tx] {
				continue
			}
			blocks++ // one per block, however many occurrences it holds
			break
		}
	}
	return blocks
}

// referenceWindowedPage is one page of the complete windowed answer for
// chain (heights 1..len(chain)) sliced at offset, with the whole-range
// statistics the page must carry. The continuation cursor is deliberately
// excluded: it is opaque and is validated separately by actually following
// it.
func referenceWindowedPage(chain []Block, window timeWindow, ids []string, pageSize int, offset int64) TxPage {
	full := referenceWindowedHits(chain, window, ids...)
	rest := full[offset:]
	end := pageSize
	if end > len(rest) {
		end = len(rest)
	}
	return TxPage{
		Hits:          append([]TxHit{}, rest[:end]...),
		TotalMatches:  int64(len(full)),
		MatchedBlocks: referenceWindowedMatchedBlocks(chain, window, ids...),
		ToHeight:      int64(len(chain)),
	}
}

// Hand-derived complete answers, so the guarantee rests on the literal
// content of each chain rather than on oracle-vs-DeepEqual alone. Both lists
// are ordered by height then block position even though the surviving
// timestamps are not monotonic (new chain: 10, 10, 0, 20).
func literalTimeReorgOldHits() []TxHit {
	return []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "m", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "m", Position: 1},
		{Height: 3, BlockHash: "h3", TxID: "m", Position: 0},
		{Height: 5, BlockHash: "h5", TxID: "m", Position: 0},
		{Height: 5, BlockHash: "h5", TxID: "m", Position: 1},
	}
}

func literalTimeReorgNewHits() []TxHit {
	return []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "m", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "m", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "m", Position: 0},
		{Height: 4, BlockHash: "h4", TxID: "m", Position: 1},
	}
}

// The fixture itself: the reorg rewrites nothing but timestamps, the two
// complete windowed answers are observably different in both records and
// statistics, and the missing-vs-zero distinction decides membership at
// heights 2 and 5. Cross-checked against the live index on both chains.
func TestQueryTxsTimeOnlyReorgFixtureSemantics(t *testing.T) {
	oldChain, newChain := timeReorgOldChain(), timeReorgNewChain()
	start, end := timeReorgWindow()
	window, err := normalizeTimeWindow(start, end)
	if err != nil {
		t.Fatalf("fixture window rejected: %v", err)
	}

	// Setup invariant: per height the two chains carry identical hashes,
	// parents, and transaction lists; only the timestamp differs, and it
	// differs at every replaced height so the reorg is meaningful.
	if len(oldChain) != len(newChain) {
		t.Fatalf("chain lengths differ: %d vs %d", len(oldChain), len(newChain))
	}
	for i := range oldChain {
		oldBlock, newBlock := oldChain[i], newChain[i]
		if oldBlock.Hash != newBlock.Hash || oldBlock.Parent != newBlock.Parent ||
			!reflect.DeepEqual(oldBlock.Txs, newBlock.Txs) {
			t.Fatalf("height %d: reorg must keep hash/parent/txs, got %+v vs %+v",
				oldBlock.Height, oldBlock, newBlock)
		}
		if i == 0 {
			if !reflect.DeepEqual(oldBlock.Time, newBlock.Time) {
				t.Fatal("retained ancestor timestamp must stay identical")
			}
			continue
		}
		oldNil, newNil := oldBlock.Time == nil, newBlock.Time == nil
		if oldNil == newNil && (oldNil || *oldBlock.Time == *newBlock.Time) {
			t.Fatalf("height %d: timestamp must change across the reorg", oldBlock.Height)
		}
	}

	// The oracle matches the hand-derived answers on both chains: h1's two
	// m's are two occurrences but one matched block, h4's m keeps position 1
	// among the non-matching x's, and ordering follows height, never time.
	if got := referenceWindowedHits(oldChain, window, "m"); !reflect.DeepEqual(got, literalTimeReorgOldHits()) {
		t.Fatalf("old oracle hits=%v, want %v", got, literalTimeReorgOldHits())
	}
	if got := referenceWindowedHits(newChain, window, "m"); !reflect.DeepEqual(got, literalTimeReorgNewHits()) {
		t.Fatalf("new oracle hits=%v, want %v", got, literalTimeReorgNewHits())
	}
	if got := referenceWindowedMatchedBlocks(oldChain, window, "m"); got != 3 {
		t.Fatalf("old matched blocks=%d, want 3 (h1's duplicates count once)", got)
	}
	if got := referenceWindowedMatchedBlocks(newChain, window, "m"); got != 3 {
		t.Fatalf("new matched blocks=%d, want 3", got)
	}

	index := buildTimeReorgOldChain(t)
	fullPage := timeReorgQuery()
	fullPage.PageSize = 100 // one page holds every occurrence of either chain
	live, err := index.QueryTxs(fullPage)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live.Hits, literalTimeReorgOldHits()) ||
		live.TotalMatches != 5 || live.MatchedBlocks != 3 || live.ToHeight != 5 {
		t.Fatalf("live old-chain query=%+v, want 5/3/tip5 with literal old records", live)
	}

	// The missing-vs-zero distinction flips membership across the reorg: a
	// window covering only zero finds nothing on the old chain (h2 has no
	// timestamp) and exactly h2 on the new chain (h2 has a REAL zero).
	zeroOnly := TxQuery{TxIDs: []string{"m"}, TimeStart: intptr(0), TimeEnd: intptr(1)}
	page, err := index.QueryTxs(zeroOnly)
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalMatches != 0 || len(page.Hits) != 0 {
		t.Fatalf("old chain under [0,1): %+v, want empty (missing time never matches)", page)
	}

	dropped, err := index.Reorg(timeReorgNewSuffix())
	if err != nil {
		t.Fatalf("time-only reorg refused: %v", err)
	}
	if want := []int64{2, 3, 4, 5}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped=%v, want %v (only timestamps changed, still replacements)", dropped, want)
	}
	if index.Tip != 5 {
		t.Fatalf("tip=%d, want unchanged tip 5", index.Tip)
	}

	live, err = index.QueryTxs(fullPage)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(live.Hits, literalTimeReorgNewHits()) ||
		live.TotalMatches != 4 || live.MatchedBlocks != 3 || live.ToHeight != 5 {
		t.Fatalf("live new-chain query=%+v, want 4/3/tip5 with literal new records", live)
	}
	page, err = index.QueryTxs(zeroOnly)
	if err != nil {
		t.Fatal(err)
	}
	wantZero := []TxHit{{Height: 2, BlockHash: "h2", TxID: "m", Position: 0}}
	if !reflect.DeepEqual(page.Hits, wantZero) || page.TotalMatches != 1 || page.MatchedBlocks != 1 {
		t.Fatalf("new chain under [0,1): %+v, want only the real-zero h2", page)
	}
}

// Ordering 1: the first page is holding the lock on the complete OLD
// timestamps when the time-only reorg starts. It must finish on the old
// chain — hits, statistics, and pinned height all from the old times — and
// its cursor must die with a zeroed ErrQueryChanged once the new times
// commit, never stitching new-time hits onto the old page.
func TestQueryTxsTimeOnlyReorgFirstPageInFlightObservesCompleteOldChain(t *testing.T) {
	index := buildTimeReorgOldChain(t)
	h := installQueryReorgHooks(t, true, false, 5)
	oldChain, newChain := timeReorgOldChain(), timeReorgNewChain()
	start, end := timeReorgWindow()
	window, err := normalizeTimeWindow(start, end)
	if err != nil {
		t.Fatal(err)
	}

	result := make(chan TxPage, 1)
	errCh := make(chan error, 1)
	go func() {
		page, err := index.QueryTxs(timeReorgQuery())
		if err != nil {
			errCh <- err
			return
		}
		result <- page
	}()

	<-h.firstParked // query holds index.mu and has pinned tip 5 under the old times
	reorgDone := make(chan struct{})
	go func() {
		if _, err := index.Reorg(timeReorgNewSuffix()); err != nil {
			errCh <- err
		}
		close(reorgDone)
	}()

	close(h.releaseFirst) // query scans and finishes first; the reorg cannot apply yet
	var got TxPage
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("concurrent call failed: %v", err)
	}
	want := referenceWindowedPage(oldChain, window, []string{"m"}, 2, 0)
	if err := classifyTxPage(got, want, referenceWindowedPage(newChain, window, []string{"m"}, 2, 0),
		referenceWindowedHits(oldChain, window, "m"), referenceWindowedHits(newChain, window, "m")); err != nil {
		t.Fatalf("in-flight first page did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Fatalf("query holding the lock across reorg start must see old times:\n%+v", got)
	}
	if !reflect.DeepEqual(got.Hits, literalTimeReorgOldHits()[:2]) {
		t.Fatalf("old first page hits=%v, want literal old records", got.Hits)
	}
	if got.NextCursor == "" {
		t.Fatal("old first page over 5 matches must carry a cursor")
	}

	close(h.releaseReorg) // now let the parked reorg commit the new timestamps
	<-reorgDone
	if index.Tip != 5 {
		t.Fatalf("tip after reorg=%d, want unchanged 5", index.Tip)
	}

	// The cursor pins the OLD timestamps (tip 5, offset 2, old fingerprint
	// including every old time); on the new times it must fail rather than
	// resume with records the new window selects.
	oldFingerprint := buildTimeReorgOldChain(t)
	payload := decodePayload(t, index, got.NextCursor)
	if payload.To != 5 || payload.Off != 2 {
		t.Fatalf("cursor pins to=%d off=%d, want 5/2", payload.To, payload.Off)
	}
	if payload.FP != fmt.Sprintf("%x", oldFingerprint.fingerprintLocked(1, 5)) {
		t.Fatal("cursor fingerprint does not match the complete old-time chain")
	}
	query := timeReorgQuery()
	query.Cursor = got.NextCursor
	page, err := index.QueryTxs(query)
	requireChangedZeroed(t, page, err)
}

// Ordering 2: the time-only reorg has fully committed (new timestamps in
// effect, Reorg still holding the lock) when the first page starts. It must
// wait and then answer on the complete NEW chain, and its cursor must
// paginate the new windowed answer to the end with pinned statistics.
func TestQueryTxsTimeOnlyReorgFirstPageAfterCommitObservesCompleteNewChain(t *testing.T) {
	index := buildTimeReorgOldChain(t)
	h := installQueryReorgHooks(t, true, false, 5)
	oldChain, newChain := timeReorgOldChain(), timeReorgNewChain()
	start, end := timeReorgWindow()
	window, err := normalizeTimeWindow(start, end)
	if err != nil {
		t.Fatal(err)
	}

	reorgErr := make(chan error, 1)
	go func() {
		_, err := index.Reorg(timeReorgNewSuffix())
		reorgErr <- err
	}()
	<-h.reorgParked // new timestamps applied (tip still 5), Reorg still holds the lock

	result := make(chan TxPage, 1)
	go func() {
		page, err := index.QueryTxs(timeReorgQuery())
		if err != nil {
			t.Errorf("query during reorg failed: %v", err)
			return
		}
		result <- page
	}()

	close(h.releaseReorg) // reorg returns; the first lock the query can get sees the new times
	if err := <-reorgErr; err != nil {
		t.Fatalf("reorg failed: %v", err)
	}
	close(h.releaseFirst) // the parked first page can now scan and finish

	got := <-result
	want := referenceWindowedPage(newChain, window, []string{"m"}, 2, 0)
	if err := classifyTxPage(got, referenceWindowedPage(oldChain, window, []string{"m"}, 2, 0), want,
		referenceWindowedHits(oldChain, window, "m"), referenceWindowedHits(newChain, window, "m")); err != nil {
		t.Fatalf("first page across reorg commit did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(viewOf(got), viewOf(want)) {
		t.Fatalf("query after reorg commit must see the complete new-time chain:\n%+v", got)
	}
	if !reflect.DeepEqual(got.Hits, literalTimeReorgNewHits()[:2]) {
		t.Fatalf("new first page hits=%v, want literal new records", got.Hits)
	}
	if got.NextCursor == "" {
		t.Fatal("new first page over 4 matches must carry a cursor")
	}

	// The cursor pins the NEW timestamps; following it while the chain stays
	// put must drain exactly the new windowed occurrences — no duplicates,
	// no omissions, statistics pinned to the first page, no trailing cursor.
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
	if payload.FP != fmt.Sprintf("%x", newFingerprint.fingerprintLocked(1, 5)) {
		t.Fatal("cursor fingerprint does not match the complete new-time chain")
	}

	query := timeReorgQuery()
	query.Cursor = got.NextCursor
	all := append([]TxHit{}, got.Hits...)
	for pageNo := 1; ; pageNo++ {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("new-chain pagination broke at page %d: %v", pageNo, err)
		}
		if page.TotalMatches != 4 || page.MatchedBlocks != 3 || page.ToHeight != 5 {
			t.Fatalf("page %d statistics drifted from the pinned first page: %+v", pageNo, page)
		}
		all = append(all, page.Hits...)
		if page.NextCursor == "" {
			break
		}
		query.Cursor = page.NextCursor
	}
	if !reflect.DeepEqual(all, literalTimeReorgNewHits()) {
		t.Fatalf("new-chain pagination=%v, want %v", all, literalTimeReorgNewHits())
	}
}

// A cursor minted under the old timestamps is dead once the time-only reorg
// commits, even though every hash, parent, and transaction in the pinned
// range is unchanged: the continuation returns ErrQueryChanged with no hits,
// no follow-up cursor, and no statistics — it never appends records the new
// window selects onto the old page. The index itself stays usable.
func TestQueryTxsTimeOnlyReorgContinuationIsChangedAndZeroed(t *testing.T) {
	index := buildTimeReorgOldChain(t)
	first, err := index.QueryTxs(timeReorgQuery())
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" || first.ToHeight != 5 || first.TotalMatches != 5 || first.MatchedBlocks != 3 {
		t.Fatalf("unexpected first page: %+v", first)
	}

	dropped, err := index.Reorg(timeReorgNewSuffix())
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 4 || index.Tip != 5 {
		t.Fatalf("unexpected reorg outcome: dropped=%v tip=%d", dropped, index.Tip)
	}

	query := timeReorgQuery()
	query.Cursor = first.NextCursor
	page, err := index.QueryTxs(query)
	requireChangedZeroed(t, page, err)

	// A fresh first page starts over on the complete new-time chain.
	fresh, err := index.QueryTxs(timeReorgQuery())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fresh.Hits, literalTimeReorgNewHits()[:2]) ||
		fresh.TotalMatches != 4 || fresh.MatchedBlocks != 3 || fresh.ToHeight != 5 {
		t.Fatalf("fresh page after reorg=%+v, want complete new-time answer", fresh)
	}
}
