package indexroom

import (
	"bytes"
	"errors"
	"reflect"
	"sync"
	"testing"
)

// This file is the regression net for an Append interleaving with a Reorg
// whose branch is rooted BELOW the append's parent — the append points at a
// tip that the reorg is about to discard. A successful Append must not pin its
// block onto the chain: acceptance only means the block extended the tip at
// the instant it was stored. Likewise, once the reorg wins, the stale parent
// hash can never attach the candidate to the new main chain.
//
// The fixture (heights 1..3 already on the main chain):
//
//	h1 parent genesis txs [keep-tx]              // retained ancestor
//	h2 parent h1      txs [old-tx-2]             // old chain
//	h3 parent h2      txs [old-tx-3]             // old chain, current tip
//
// Racing calls:
//
//	Append: h4 height 4, parent h3, txs [append-tx]
//	Reorg : r2 height 2, parent h1, txs [new-tx, new-tx]
//
// Two serializations are possible and both are required to behave:
//
//   - Append first: Append succeeds (nil); Reorg succeeds and reports the
//     dropped OLD heights in ascending order, [2,3,4] — the just-appended
//     height 4 is discarded together with the old suffix it extended.
//   - Reorg first: Reorg succeeds reporting [2,3]; the Append then fails with
//     a NON-nil ordinary ingestion error (never ErrInvalidArgument, which is
//     reserved for query-argument problems), and must leave no height-4 block,
//     no h4 hash record, and no indexed append-tx occurrence.
//
// Either way the final main chain is h1 then r2, tip 2: h2, h3, h4 are
// unreadable by height, the hash table only describes the retained chain, and
// QueryTxs answers from that chain alone — old-chain and appended txs get
// successful empty results, the new tx hits twice at height 2 positions 0 and
// 1 (TotalMatches 2, MatchedBlocks 1), and without a filter there are three
// hits across two matched blocks.
//
// The two orderings are exercised DETERMINISTICALLY with the in-lock
// rendezvous hooks (appendStoredHookLocked / appendRejectedHookLocked /
// reorgAppliedHookLocked): one call is parked with its critical section
// provably committed while the other call already exists and is contending
// for index.mu, so the serialization under test is constructed rather than
// hoped for. The free-running stress test on top additionally demonstrates
// that plain concurrency really does reach both serializations; sequential
// baseline checks alone would not establish the interleaved behaviour.

// interleavingBaseChain is the already-ingested h1..h3 chain.
func interleavingBaseChain() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"keep-tx"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"old-tx-2"}},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"old-tx-3"}},
	}
}

// interleavingAppendBlock is the height-4 candidate parented at the old tip.
func interleavingAppendBlock() Block {
	return Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"append-tx"}}
}

// interleavingReorgBranch is the shortening branch rooted at retained h1.
func interleavingReorgBranch() []Block {
	return []Block{
		{Height: 2, Hash: "r2", Parent: "h1", Txs: []string{"new-tx", "new-tx"}},
	}
}

func buildInterleavingChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range interleavingBaseChain() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

// appendReorgHooks are the one-shot rendezvous points shared by the two
// deterministic interleaving tests. Every parked call proves which chain
// state its decision was made against: a parked accepted append has h4
// committed on the old tip, a parked reorg has r2 committed at tip 2, and a
// parked rejected append has just been refused against the new chain.
type appendReorgHooks struct {
	appendParked  chan struct{}
	releaseAppend chan struct{}
	rejectParked  chan struct{}
	releaseReject chan struct{}
	reorgParked   chan struct{}
	releaseReorg  chan struct{}

	appendOnce sync.Once
	rejectOnce sync.Once
	reorgOnce  sync.Once
}

// installAppendReorgHooks wires the rendezvous hooks for one test: the
// successful-append hook parks only when parkSuccess is set, the rejection
// hook only when parkReject is set, and reorg completion always parks. Hook
// arguments are checked so a firing at an unexpected chain state fails the
// test rather than silently sequencing the wrong scenario.
func installAppendReorgHooks(t *testing.T, parkSuccess, parkReject bool) *appendReorgHooks {
	t.Helper()
	h := &appendReorgHooks{
		reorgParked:  make(chan struct{}),
		releaseReorg: make(chan struct{}),
	}
	if parkSuccess {
		h.appendParked = make(chan struct{})
		h.releaseAppend = make(chan struct{})
	}
	if parkReject {
		h.rejectParked = make(chan struct{})
		h.releaseReject = make(chan struct{})
	}
	appendStoredHookLocked = func(newTip int64) {
		if newTip != 4 {
			t.Errorf("append hook saw newTip=%d, want 4 (h4 stored over the old tip)", newTip)
		}
		if h.appendParked != nil {
			h.appendOnce.Do(func() { close(h.appendParked) })
			<-h.releaseAppend
		}
	}
	appendRejectedHookLocked = func(height int64) {
		if height != 4 {
			t.Errorf("rejection hook saw height=%d, want 4 (the stale h4 candidate)", height)
		}
		if h.rejectParked != nil {
			h.rejectOnce.Do(func() { close(h.rejectParked) })
			<-h.releaseReject
		}
	}
	reorgAppliedHookLocked = func(newTip int64) {
		if newTip != 2 {
			t.Errorf("reorg hook saw newTip=%d, want 2", newTip)
		}
		h.reorgOnce.Do(func() { close(h.reorgParked) })
		<-h.releaseReorg
	}
	t.Cleanup(func() {
		appendStoredHookLocked = nil
		appendRejectedHookLocked = nil
		reorgAppliedHookLocked = nil
	})
	return h
}

// requireEmptyTxResult requires a successful page carrying no occurrences:
// the lookup itself must succeed (a rejection here must never masquerade as a
// query-argument error) and must pin to the retained tip.
func requireEmptyTxResult(t *testing.T, page TxPage, err error, tx string) {
	t.Helper()
	if err != nil {
		t.Fatalf("query for %q must succeed with an empty result, got error: %v", tx, err)
	}
	want := TxPage{Hits: []TxHit{}, ToHeight: 2}
	if !reflect.DeepEqual(page, want) {
		t.Fatalf("query for %q = %+v, want successful empty page pinned to tip 2", tx, page)
	}
}

// assertRetainedChainState verifies the complete post-interleaving state,
// read only after BOTH calls have returned: tip 2, exactly h1 and r2 stored
// with the hash table agreeing, and every QueryTxs answer describing that one
// retained chain.
func assertRetainedChainState(t *testing.T, index *Index) {
	t.Helper()
	if index.Tip != 2 {
		t.Fatalf("tip=%d, want retained tip 2", index.Tip)
	}
	wantBlocks := map[int64]Block{
		1: {Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"keep-tx"}},
		2: {Height: 2, Hash: "r2", Parent: "h1", Txs: []string{"new-tx", "new-tx"}},
	}
	if !reflect.DeepEqual(map[int64]Block(index.Blocks), wantBlocks) {
		t.Fatalf("Blocks=%v\nwant only the retained chain %v", index.Blocks, wantBlocks)
	}
	wantByHash := map[string]int64{"h1": 1, "r2": 2}
	if !reflect.DeepEqual(map[string]int64(index.ByHash), wantByHash) {
		t.Fatalf("ByHash=%v, want %v (old and appended hashes must be gone)", index.ByHash, wantByHash)
	}
	// Heights above the new tip are gone (height 2 itself is kept, now r2),
	// and the old and appended block hashes resolve to nothing.
	for _, height := range []int64{3, 4} {
		if _, ok := index.Blocks[height]; ok {
			t.Fatalf("height %d must no longer be readable after the reorg", height)
		}
	}
	for _, hash := range []string{"h2", "h3", "h4"} {
		if _, ok := index.ByHash[hash]; ok {
			t.Fatalf("dropped hash %q must no longer map to a height", hash)
		}
	}

	// Old-chain transactions and the transaction carried only by the rejected
	// (or discarded) append return successful empty results.
	for _, tx := range []string{"old-tx-2", "old-tx-3", "append-tx"} {
		page, err := index.QueryTxs(TxQuery{TxIDs: []string{tx}, PageSize: MaxPageSize})
		requireEmptyTxResult(t, page, err, tx)
	}

	// The retained ancestor tx is found at its original height/position.
	keep, err := index.QueryTxs(TxQuery{TxIDs: []string{"keep-tx"}, PageSize: MaxPageSize})
	if err != nil {
		t.Fatalf("query for retained tx failed: %v", err)
	}
	if want := []TxHit{{Height: 1, BlockHash: "h1", TxID: "keep-tx", Position: 0}}; !reflect.DeepEqual(keep.Hits, want) {
		t.Fatalf("retained tx hits=%v, want %v", keep.Hits, want)
	}
	if keep.TotalMatches != 1 || keep.MatchedBlocks != 1 || keep.ToHeight != 2 || keep.NextCursor != "" {
		t.Fatalf("retained tx page stats=%+v, want 1/1/tip2 with no cursor", keep)
	}

	// The new tx occurs twice, both in the one retained replacement block.
	newPage, err := index.QueryTxs(TxQuery{TxIDs: []string{"new-tx"}, PageSize: MaxPageSize})
	if err != nil {
		t.Fatalf("query for new tx failed: %v", err)
	}
	wantNewHits := []TxHit{
		{Height: 2, BlockHash: "r2", TxID: "new-tx", Position: 0},
		{Height: 2, BlockHash: "r2", TxID: "new-tx", Position: 1},
	}
	if !reflect.DeepEqual(newPage.Hits, wantNewHits) {
		t.Fatalf("new tx hits=%v, want both in-block occurrences %v", newPage.Hits, wantNewHits)
	}
	if newPage.TotalMatches != 2 || newPage.MatchedBlocks != 1 || newPage.ToHeight != 2 || newPage.NextCursor != "" {
		t.Fatalf("new tx page stats=%+v, want 2 matches in 1 block at tip 2", newPage)
	}

	// Without a filter: the retained tx plus both new occurrences, across two
	// matched blocks, ordered by height then position.
	all, err := index.QueryTxs(TxQuery{PageSize: MaxPageSize})
	if err != nil {
		t.Fatalf("unfiltered query failed: %v", err)
	}
	wantAll := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "keep-tx", Position: 0},
		{Height: 2, BlockHash: "r2", TxID: "new-tx", Position: 0},
		{Height: 2, BlockHash: "r2", TxID: "new-tx", Position: 1},
	}
	if !reflect.DeepEqual(all.Hits, wantAll) {
		t.Fatalf("unfiltered hits=%v, want retained-chain occurrences %v", all.Hits, wantAll)
	}
	if all.TotalMatches != 3 || all.MatchedBlocks != 2 || all.ToHeight != 2 || all.NextCursor != "" {
		t.Fatalf("unfiltered stats=%+v, want 3 matches in 2 blocks at tip 2", all)
	}
}

// assertRetainedSnapshot cross-checks the observable state against an
// independently built retained chain (h1, then r2 installed through the
// public Reorg entrypoint): canonical exports must be byte-identical and a
// snapshot round-trip must preserve them.
func assertRetainedSnapshot(t *testing.T, index *Index) {
	t.Helper()
	oracle := New()
	if err := oracle.Append(cloneCandidate(interleavingBaseChain()[0])); err != nil {
		t.Fatalf("oracle setup append at 1: %v", err)
	}
	if dropped, err := oracle.Reorg(interleavingReorgBranch()); err != nil || len(dropped) != 0 {
		t.Fatalf("oracle reorg onto h1: dropped=%v err=%v, want empty dropped list", dropped, err)
	}
	if got, want := exportBytes(t, index), exportBytes(t, oracle); !reflect.DeepEqual(got, want) {
		t.Fatalf("raced index export:\n%s\nretained-chain oracle export:\n%s", got, want)
	}
	restored := New()
	if err := restored.Restore(bytes.NewReader(exportBytes(t, index))); err != nil {
		t.Fatalf("restore of raced export failed: %v", err)
	}
	if got, want := exportBytes(t, restored), exportBytes(t, oracle); !reflect.DeepEqual(got, want) {
		t.Fatalf("restored export diverges from retained-chain oracle:\n%s\nwant:\n%s", got, want)
	}
}

// TestAppendReorgInterleavingFixtureBaseline pins the fixture's own serial
// semantics before any concurrency is involved: the two branch operations are
// each valid on their own, and the two sequential orderings produce the two
// outcomes the interleaving tests then require under real overlap. This is
// setup validation, not the regression itself.
func TestAppendReorgInterleavingFixtureBaseline(t *testing.T) {
	// h4 is accepted on its own over h3.
	index := buildInterleavingChain(t)
	if err := index.Append(interleavingAppendBlock()); err != nil {
		t.Fatalf("h4 must extend the h3 tip on its own: %v", err)
	}
	if index.Tip != 4 || index.ByHash["h4"] != 4 {
		t.Fatalf("h4 not indexed: tip=%d byHash=%v", index.Tip, index.ByHash)
	}

	// Sequential order A: append then reorg reports [2,3,4].
	indexA := buildInterleavingChain(t)
	if err := indexA.Append(cloneCandidate(interleavingAppendBlock())); err != nil {
		t.Fatalf("sequential append failed: %v", err)
	}
	droppedA, err := indexA.Reorg(interleavingReorgBranch())
	if err != nil {
		t.Fatalf("sequential reorg failed: %v", err)
	}
	if want := []int64{2, 3, 4}; !reflect.DeepEqual(droppedA, want) {
		t.Fatalf("sequential order A dropped=%v, want %v", droppedA, want)
	}
	assertRetainedChainState(t, indexA)
	assertRetainedSnapshot(t, indexA)

	// Sequential order B: reorg reports [2,3], then h4 is an ordinary
	// ingestion rejection, not an argument error.
	indexB := buildInterleavingChain(t)
	droppedB, err := indexB.Reorg(interleavingReorgBranch())
	if err != nil {
		t.Fatalf("sequential reorg failed: %v", err)
	}
	if want := []int64{2, 3}; !reflect.DeepEqual(droppedB, want) {
		t.Fatalf("sequential order B dropped=%v, want %v", droppedB, want)
	}
	appendErr := indexB.Append(cloneCandidate(interleavingAppendBlock()))
	if appendErr == nil {
		t.Fatal("h4 parented at discarded h3 must be rejected after the reorg")
	}
	if errors.Is(appendErr, ErrInvalidArgument) {
		t.Fatalf("stale-parent rejection must not be ErrInvalidArgument: %v", appendErr)
	}
	if appendErr.Error() == "" {
		t.Fatal("ingestion rejection must carry a non-empty reason")
	}
	assertRetainedChainState(t, indexB)
	assertRetainedSnapshot(t, indexB)
}

// TestAppendAcceptedThenReorgDiscardsJustAppendedHeight: the Append call has
// already stored h4 on the old tip (proved by the post-store rendezvous while
// it holds the lock) when the Reorg call is started and contends for
// index.mu. Append must succeed; Reorg must then succeed and discard heights
// [2,3,4] in ascending order — including the height that append just won —
// leaving only h1 and r2.
func TestAppendAcceptedThenReorgDiscardsJustAppendedHeight(t *testing.T) {
	index := buildInterleavingChain(t)
	h := installAppendReorgHooks(t, true, false)

	appendErrCh := make(chan error, 1)
	go func() {
		appendErrCh <- index.Append(cloneCandidate(interleavingAppendBlock()))
	}()
	<-h.appendParked // h4 committed at tip 4; Append still holds index.mu

	reorgStarted := make(chan struct{})
	droppedCh := make(chan []int64, 1)
	reorgErrCh := make(chan error, 1)
	go func() {
		close(reorgStarted) // the Reorg call is in flight and contends for the lock
		dropped, err := index.Reorg(interleavingReorgBranch())
		droppedCh <- dropped
		reorgErrCh <- err
	}()
	<-reorgStarted

	// Release the parked append first: its critical section provably completed
	// before Reorg's can begin (Reorg could not acquire index.mu until now).
	close(h.releaseAppend)
	if err := <-appendErrCh; err != nil {
		t.Fatalf("append extending the old tip must succeed before the reorg: %v", err)
	}
	<-h.reorgParked // branch applied; h2, h3 and the just-appended h4 removed
	close(h.releaseReorg)

	if err := <-reorgErrCh; err != nil {
		t.Fatalf("reorg after the accepted append failed: %v", err)
	}
	dropped := <-droppedCh
	if want := []int64{2, 3, 4}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped=%v, want ascending %v including the just-appended height 4", dropped, want)
	}
	assertRetainedChainState(t, index)
	assertRetainedSnapshot(t, index)
}

// TestReorgCommittedThenAppendRejectsStaleParent: the Reorg has fully
// installed r2 at tip 2 (proved by the post-apply rendezvous while it still
// holds the lock) when the Append call starts and blocks waiting for
// index.mu. Reorg must return [2,3]; the append, once it runs against the new
// chain, must be refused with a non-nil ordinary ingestion error — never
// ErrInvalidArgument — and leave no height-4 block, h4 hash, or appended tx.
func TestReorgCommittedThenAppendRejectsStaleParent(t *testing.T) {
	index := buildInterleavingChain(t)
	h := installAppendReorgHooks(t, false, true)

	reorgStarted := make(chan struct{})
	droppedCh := make(chan []int64, 1)
	reorgErrCh := make(chan error, 1)
	go func() {
		close(reorgStarted)
		dropped, err := index.Reorg(interleavingReorgBranch())
		droppedCh <- dropped
		reorgErrCh <- err
	}()
	<-reorgStarted
	<-h.reorgParked // new chain committed at tip 2; Reorg still holds index.mu

	appendStarted := make(chan struct{})
	appendErrCh := make(chan error, 1)
	go func() {
		close(appendStarted) // the Append call is in flight while Reorg has not returned
		appendErrCh <- index.Append(cloneCandidate(interleavingAppendBlock()))
	}()
	<-appendStarted

	// Reorg returns first; the append can only then take the lock, at which
	// point its parent h3 is gone and the candidate no longer extends the tip.
	close(h.releaseReorg)
	if err := <-reorgErrCh; err != nil {
		t.Fatalf("reorg must succeed: %v", err)
	}
	if got, want := <-droppedCh, []int64{2, 3}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dropped=%v, want %v", got, want)
	}
	<-h.rejectParked // rejection decided against the new main chain, lock held
	close(h.releaseReject)

	appendErr := <-appendErrCh
	if appendErr == nil {
		t.Fatal("append parented at discarded h3 must fail after the reorg committed")
	}
	if errors.Is(appendErr, ErrInvalidArgument) {
		t.Fatalf("ingestion rejection must not be the query error ErrInvalidArgument: %v", appendErr)
	}
	if appendErr.Error() == "" {
		t.Fatal("ingestion rejection must carry a non-empty reason")
	}
	assertRetainedChainState(t, index)
	assertRetainedSnapshot(t, index)
}

// TestConcurrentAppendAndReorgBothOrderingsOccur: without any rendezvous, the
// two calls are released together over many fresh indexes. Every round must
// classify as exactly one of the two required serializations with its exact
// dropped list and append result, and the post-state must be the retained
// chain; the run additionally requires that BOTH serializations actually
// occur, so neither ordering is only covered by construction. (The hook tests
// above are the deterministic guarantee; this loop shows natural contention
// reaches both outcomes instead of always serializing one way.)
//
// Goroutine creation order alternates per round: the scheduler otherwise
// hands one fixed call the lock almost every time, which would make observing
// both orders a matter of luck. Both goroutines are created and blocked on
// the gate before either call starts, so whichever creation slot it occupies,
// the two calls genuinely contend for index.mu rather than running serially.
func TestConcurrentAppendAndReorgBothOrderingsOccur(t *testing.T) {
	const rounds = 400
	var seenAppendFirst, seenReorgFirst int
	for round := 0; round < rounds; round++ {
		index := buildInterleavingChain(t)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var appendErr error
		var dropped []int64
		var reorgErr error
		runAppend := func() {
			defer wg.Done()
			<-start
			appendErr = index.Append(cloneCandidate(interleavingAppendBlock()))
		}
		runReorg := func() {
			defer wg.Done()
			<-start
			dropped, reorgErr = index.Reorg(interleavingReorgBranch())
		}
		wg.Add(2)
		if round%2 == 0 {
			go runAppend()
			go runReorg()
		} else {
			go runReorg()
			go runAppend()
		}
		close(start)
		wg.Wait()

		if reorgErr != nil {
			t.Fatalf("round %d: reorg failed: %v", round, reorgErr)
		}
		switch {
		case appendErr == nil && reflect.DeepEqual(dropped, []int64{2, 3, 4}):
			seenAppendFirst++
		case appendErr != nil &&
			!errors.Is(appendErr, ErrInvalidArgument) &&
			appendErr.Error() != "" &&
			reflect.DeepEqual(dropped, []int64{2, 3}):
			seenReorgFirst++
		default:
			t.Fatalf("round %d: impossible interleaving outcome: appendErr=%v dropped=%v reorgErr=%v",
				round, appendErr, dropped, reorgErr)
		}
		assertRetainedChainState(t, index)
		// Cross-check the export against the retained-chain oracle every round.
		oracle := New()
		if err := oracle.Append(cloneCandidate(interleavingBaseChain()[0])); err != nil {
			t.Fatalf("round %d: oracle setup: %v", round, err)
		}
		if _, err := oracle.Reorg(interleavingReorgBranch()); err != nil {
			t.Fatalf("round %d: oracle reorg: %v", round, err)
		}
		if got, want := exportBytes(t, index), exportBytes(t, oracle); !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d: raced export:\n%s\nwant:\n%s", round, got, want)
		}
	}
	if seenAppendFirst == 0 || seenReorgFirst == 0 {
		t.Fatalf("over %d rounds only one serialization occurred (append-first=%d reorg-first=%d); both must be exercised",
			rounds, seenAppendFirst, seenReorgFirst)
	}
	// A one-sided split means one ordering is only being reached by chance;
	// alternating creation order should exercise both reliably.
	if seenAppendFirst*4 < rounds || seenReorgFirst*4 < rounds {
		t.Fatalf("interleaving split unexpectedly one-sided over %d rounds: append-first=%d reorg-first=%d",
			rounds, seenAppendFirst, seenReorgFirst)
	}
	t.Logf("over %d free-running rounds: append-first=%d reorg-first=%d",
		rounds, seenAppendFirst, seenReorgFirst)
}
