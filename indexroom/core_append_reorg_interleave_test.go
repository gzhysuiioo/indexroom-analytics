package indexroom

import (
	"errors"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

// This file is the regression net for Append/Reorg INTERLEAVING: an append
// request pointing at the OLD chain tip races a reorg that abandons that tip.
// The existing mixed-concurrency checks establish that the final chain is
// continuous and that blocks line up with their hashes, but "the append
// succeeded" must not be confused with "the appended block survives":
//
//   - When the append is the first call committed, it must be accepted against
//     the tip that was current when it was validated (h4@4 parent h3 is the
//     legitimate tip+1 of the h1..h3 chain). The concurrent reorg then
//     succeeds and reports the dropped old heights in ascending order, and
//     that report MUST include the just-appended height 4: a successful
//     append earns the block no survival guarantee across a later reorg.
//
//   - When the reorg is the first call committed, r2@2 parent h1 is already
//     the main chain when the append acquires the lock: the old parent hash h3
//     is gone and cannot be used to attach anything to the new main chain.
//     The append must return a NON-NIL ordinary ingestion rejection — never
//     the query-parameter error ErrInvalidArgument — and must leave no height-4
//     block and no h4 hash record behind.
//
// Both orders are forced DETERMINISTICALLY through the package's lock-held
// rendezvous hooks (appendStoredHookLocked fires right after an Append records
// its accepted block, reorgAppliedHookLocked fires once the branch is fully
// applied): the first operation is parked mid-call while holding index.mu;
// the second operation is then started and positively observed BLOCKED at
// Index.mu (its goroutine sits in sync.(*Mutex).Lock) before the first is
// released. The two calls therefore genuinely overlap, and which commit point
// lands first is forced by the test rather than hoped for from the scheduler;
// sequential calls or repeated runs relying on lucky scheduling cannot give
// this coverage. Whichever order wins, the main chain left behind is
// identical: h1 and r2 only, tip 2.
//
// Fixture (the task's literal scenario):
//
//	h1@1 parent g   txs [kept-tx]                    } retained
//	h2@2 parent h1  txs [old-2]                      } old main chain
//	h3@3 parent h2  txs [old-3]                      } tip 3
//	h4@4 parent h3  txs [append-tx]    <- Append     } extending old tip
//	r2@2 parent h1  txs [new-tx, new-tx] <- Reorg    } replaces 2..tip
//
// Post-state either order: h1 + r2, tip 2. QueryTxs over the kept chain sees
// 3 hits over 2 matched blocks with no filter (kept-tx once, new-tx twice);
// old-2/old-3/append-tx all return successful EMPTY pages.

// appendReorgFixture builds the literal scenario: a tip-3 main chain and the
// two contending operations. r2 contains the same new transaction twice so
// QueryTxs multiplicity, positions, and matched-block counting are exercised.
type appendReorgFixture struct {
	oldChain []Block
	appended Block
	branch   []Block
}

func interleavingFixture() appendReorgFixture {
	return appendReorgFixture{
		oldChain: []Block{
			{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"kept-tx"}},
			{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"old-2"}},
			{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"old-3"}},
		},
		appended: Block{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"append-tx"}},
		branch:   []Block{{Height: 2, Hash: "r2", Parent: "h1", Txs: []string{"new-tx", "new-tx"}}},
	}
}

func (f appendReorgFixture) build(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range f.oldChain {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at height %d: %v", b.Height, err)
		}
	}
	return index
}

// assertPostInterleavingState checks the common end state both orders must
// produce: only h1 and r2 remain, tip 2; h2/h3/h4 are absent by height and by
// hash; the remaining hash->height table describes exactly the kept chain;
// and the stored r2 links onto the retained h1. Read after BOTH calls have
// returned, matching the exported-fields inspection convention.
func (f appendReorgFixture) assertPostInterleavingState(t *testing.T, index *Index) {
	t.Helper()
	if index.Tip != 2 {
		t.Fatalf("tip=%d, want 2 (only h1 and r2 survive)", index.Tip)
	}
	if len(index.Blocks) != 2 {
		t.Fatalf("block count=%d, want 2 (h1 and r2)", len(index.Blocks))
	}
	r2 := f.branch[0]
	if stored, ok := index.Blocks[1]; !ok || !equalBlockContent(stored, f.oldChain[0]) {
		t.Fatalf("height 1 = %+v, want retained %+v", stored, f.oldChain[0])
	}
	if stored, ok := index.Blocks[2]; !ok || !equalBlockContent(stored, r2) {
		t.Fatalf("height 2 = %+v, want replacement %+v", stored, r2)
	}
	if stored := index.Blocks[2]; stored.Parent != index.Blocks[1].Hash {
		t.Fatalf("r2 parent %q does not chain onto retained h1 %q", stored.Parent, index.Blocks[1].Hash)
	}
	if _, ok := index.Blocks[3]; ok {
		t.Fatal("height 3 (h3) still readable after the interleaving")
	}
	if _, ok := index.Blocks[4]; ok {
		t.Fatal("height 4 (h4) readable after reorg — a rejected or reorged-away append must not persist")
	}
	for _, hash := range []string{"h2", "h3", "h4"} {
		if _, ok := index.ByHash[hash]; ok {
			t.Fatalf("discarded hash %q still in the hash->height table", hash)
		}
	}
	wantByHash := map[string]int64{"h1": 1, "r2": 2}
	if !reflect.DeepEqual(map[string]int64(index.ByHash), wantByHash) {
		t.Fatalf("ByHash=%v, want exactly the kept chain %v", index.ByHash, wantByHash)
	}
}

// assertPostInterleavingQueries ties the state guarantees to the public
// QueryTxs API: removed transactions return successful empty pages, the
// retained+replacement chain yields the task's literal counts — filtering by
// new-tx: 2 hits at (2,0) and (2,1), TotalMatches 2, MatchedBlocks 1;
// unfiltered: 3 hits over 2 matched blocks.
func (f appendReorgFixture) assertPostInterleavingQueries(t *testing.T, index *Index) {
	t.Helper()
	// Old-chain and just-appended transactions: successful EMPTY results.
	for _, tx := range []string{"old-2", "old-3", "append-tx"} {
		page, err := index.QueryTxs(TxQuery{TxIDs: []string{tx}})
		if err != nil {
			t.Fatalf("query for removed tx %q failed: %v", tx, err)
		}
		if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 {
			t.Fatalf("removed tx %q still queryable: %+v", tx, page)
		}
		if page.NextCursor != "" {
			t.Fatalf("empty result for removed tx %q minted a cursor: %q", tx, page.NextCursor)
		}
	}
	// The retained transaction survives on h1.
	kept, err := index.QueryTxs(TxQuery{TxIDs: []string{"kept-tx"}})
	if err != nil {
		t.Fatal(err)
	}
	wantKept := []TxHit{{Height: 1, BlockHash: "h1", TxID: "kept-tx", Position: 0}}
	if !reflect.DeepEqual(kept.Hits, wantKept) {
		t.Fatalf("kept-tx hits=%v, want %v", kept.Hits, wantKept)
	}
	if kept.TotalMatches != 1 || kept.MatchedBlocks != 1 || kept.ToHeight != 2 {
		t.Fatalf("kept-tx stats=%d/%d/to%d, want 1/1/to2", kept.TotalMatches, kept.MatchedBlocks, kept.ToHeight)
	}
	// The new transaction appears exactly twice, both on r2 at positions
	// 0 and 1: TotalMatches 2, MatchedBlocks 1.
	newPage, err := index.QueryTxs(TxQuery{TxIDs: []string{"new-tx"}})
	if err != nil {
		t.Fatal(err)
	}
	wantNew := []TxHit{
		{Height: 2, BlockHash: "r2", TxID: "new-tx", Position: 0},
		{Height: 2, BlockHash: "r2", TxID: "new-tx", Position: 1},
	}
	if !reflect.DeepEqual(newPage.Hits, wantNew) {
		t.Fatalf("new-tx hits=%v, want two occurrences at (2,0) and (2,1): %v", newPage.Hits, wantNew)
	}
	if newPage.TotalMatches != 2 || newPage.MatchedBlocks != 1 || newPage.ToHeight != 2 {
		t.Fatalf("new-tx stats=%d/%d/to%d, want 2/1/to2", newPage.TotalMatches, newPage.MatchedBlocks, newPage.ToHeight)
	}
	if newPage.NextCursor != "" {
		t.Fatalf("two-hit result over a page-sized range must end without a cursor, got %q", newPage.NextCursor)
	}
	// Unfiltered: kept-tx once plus new-tx twice = 3 hits over 2 blocks.
	all, err := index.QueryTxs(TxQuery{PageSize: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	wantAll := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "kept-tx", Position: 0},
		{Height: 2, BlockHash: "r2", TxID: "new-tx", Position: 0},
		{Height: 2, BlockHash: "r2", TxID: "new-tx", Position: 1},
	}
	if !reflect.DeepEqual(all.Hits, wantAll) {
		t.Fatalf("unfiltered hits=%v, want %v", all.Hits, wantAll)
	}
	if all.TotalMatches != 3 || all.MatchedBlocks != 2 || all.ToHeight != 2 || all.NextCursor != "" {
		t.Fatalf("unfiltered stats=%d/%d/to%d/cursor=%q, want 3/2/to2/no-cursor",
			all.TotalMatches, all.MatchedBlocks, all.ToHeight, all.NextCursor)
	}
}

// appendReorgHooks deterministically orders the two contending calls. Both
// rendezvous fire while index.mu is held:
//
//   - appendStoredHookLocked: right after an Append records its accepted block
//     (tip advanced, block committed but the call has not returned);
//   - reorgAppliedHookLocked: right after the branch is fully applied.
//
// The heights observed at the fire points are recorded so the test can assert
// what was already committed at the parked instant.
type appendReorgHooks struct {
	appendParked  chan struct{}
	releaseAppend chan struct{}
	reorgParked   chan struct{}
	releaseReorg  chan struct{}

	appendStoredHeight int64
	reorgNewTip        int64
	appendOnce         sync.Once
	reorgOnce          sync.Once
}

func newAppendReorgHooks() *appendReorgHooks {
	return &appendReorgHooks{
		appendParked:  make(chan struct{}),
		releaseAppend: make(chan struct{}),
		reorgParked:   make(chan struct{}),
		releaseReorg:  make(chan struct{}),
	}
}

func (h *appendReorgHooks) install(t *testing.T) {
	t.Helper()
	appendStoredHookLocked = func(height int64) {
		h.appendStoredHeight = height
		h.appendOnce.Do(func() { close(h.appendParked) })
		<-h.releaseAppend
	}
	reorgAppliedHookLocked = func(newTip int64) {
		h.reorgNewTip = newTip
		h.reorgOnce.Do(func() { close(h.reorgParked) })
		<-h.releaseReorg
	}
	t.Cleanup(func() {
		appendStoredHookLocked = nil
		reorgAppliedHookLocked = nil
	})
}

// requireBlockedAtMu positively proves that a goroutine carrying frame
// entryFrame is currently parked in sync.(*Mutex).Lock waiting for index.mu:
// its full stack dump is in state "semacquire" and runs through the named
// Index entrypoint. The first operation holds that mutex at a hook, so the
// blocked state is the ONLY state the second call can reach — runtime.Gosched
// merely lets its goroutine materialize there; it cannot change which commit
// point lands first. This is what makes the overlap deterministic rather than
// a matter of scheduling luck.
func requireBlockedAtMu(t *testing.T, entryFrame string) {
	t.Helper()
	for i := 0; i < 10000; i++ {
		buf := make([]byte, 1<<18)
		n := runtime.Stack(buf, true)
		for _, stack := range strings.Split(string(buf[:n]), "\n\n") {
			firstLF := strings.IndexByte(stack, '\n')
			if firstLF < 0 {
				continue
			}
			header, body := stack[:firstLF], stack[firstLF:]
			// Go reports a parked mutex waiter as "[sync.Mutex.Lock]" (older
			// runtimes: "[semacquire]"); require the entrypoint frame and the
			// mutex lock itself anywhere beneath it.
			if !strings.Contains(header, "Mutex") && !strings.Contains(header, "semacquire") {
				continue
			}
			if strings.Contains(body, entryFrame) && strings.Contains(body, "sync.(*Mutex).Lock") {
				return
			}
		}
		runtime.Gosched()
	}
	t.Fatalf("no goroutine observed blocked in sync.(*Mutex).Lock at %s while the first call holds index.mu", entryFrame)
}

// assertCallNotDone is the non-blocking companion used across this package:
// the second operation must not have returned while the first is parked
// mid-call holding the lock.
func assertCallNotDone[R any](t *testing.T, ch chan R, what string) {
	t.Helper()
	select {
	case r := <-ch:
		t.Fatalf("%s already finished while the first call holds index.mu: %+v", what, r)
	default:
	}
}

type reorgOutcome struct {
	dropped []int64
	err     error
}

// Ordering 1: Append commits first. It is parked immediately after h4 was
// accepted against the old tip h3 (so its eventual success is already
// decided); Reorg is started and positively observed blocked on index.mu;
// Append is then allowed to return; Reorg commits and must drop [2,3,4] in
// ascending order — including the just-appended height 4 — and leave h1 + r2.
func TestInterleavedAppendCommittedBeforeReorgDropsAppendedHeight(t *testing.T) {
	f := interleavingFixture()
	index := f.build(t)
	h := newAppendReorgHooks()
	h.install(t)

	appendErr := make(chan error, 1)
	go func() {
		appendErr <- index.Append(f.appended)
	}()
	<-h.appendParked // h4 fully stored (tip advanced to 4), Append holds index.mu
	if h.appendStoredHeight != 4 {
		t.Fatalf("append hook fired at height %d, want 4", h.appendStoredHeight)
	}
	// The parked append still holds index.mu: a TryLock must fail without
	// blocking or handing the mutex to the reorg goroutine.
	if index.mu.TryLock() {
		index.mu.Unlock()
		t.Fatal("append must still hold index.mu while parked")
	}

	reorgDone := make(chan reorgOutcome, 1)
	go func() {
		dropped, err := index.Reorg(f.branch)
		reorgDone <- reorgOutcome{dropped, err}
	}()
	requireBlockedAtMu(t, "(*Index).Reorg")
	assertCallNotDone(t, reorgDone, "reorg")

	// Release the parked append: it must return success for the request that
	// legitimately extended the tip it was validated against.
	close(h.releaseAppend)
	if err := <-appendErr; err != nil {
		t.Fatalf("append validated against the old tip must succeed: %v", err)
	}

	// The blocked reorg now acquires the lock, applies r2, and parks after
	// applying (still inside the call). It must report the appended height 4
	// among the dropped old heights.
	<-h.reorgParked
	if h.reorgNewTip != 2 {
		t.Fatalf("reorg hook saw new tip %d, want 2", h.reorgNewTip)
	}
	close(h.releaseReorg)
	outcome := <-reorgDone
	if outcome.err != nil {
		t.Fatalf("reorg after the committed append refused: %v", outcome.err)
	}
	if want := []int64{2, 3, 4}; !reflect.DeepEqual(outcome.dropped, want) {
		t.Fatalf("dropped=%v, want %v (the just-appended height 4 must be reported)",
			outcome.dropped, want)
	}

	f.assertPostInterleavingState(t, index)
	f.assertPostInterleavingQueries(t, index)
}

// Ordering 2: Reorg commits first. The reorg is parked right after r2 is
// applied (tip 2, old suffix gone) but before it returns; the stale append
// (h4@4 parent h3 — a hash that no longer exists) is started and positively
// observed blocked at index.mu; the reorg is released; the append then
// acquires the lock against the NEW main chain and must fail with a non-nil
// ingestion error that is not ErrInvalidArgument, storing nothing.
func TestInterleavedReorgCommittedBeforeStaleAppendRejects(t *testing.T) {
	f := interleavingFixture()
	index := f.build(t)
	h := newAppendReorgHooks()
	h.install(t)

	reorgDone := make(chan reorgOutcome, 1)
	go func() {
		dropped, err := index.Reorg(f.branch)
		reorgDone <- reorgOutcome{dropped, err}
	}()
	<-h.reorgParked // r2 fully applied, tip 2; Reorg still holds index.mu
	if h.reorgNewTip != 2 {
		t.Fatalf("reorg hook saw new tip %d, want 2", h.reorgNewTip)
	}
	if index.mu.TryLock() {
		index.mu.Unlock()
		t.Fatal("reorg must still hold index.mu while parked")
	}

	appendErr := make(chan error, 1)
	go func() {
		appendErr <- index.Append(f.appended)
	}()
	requireBlockedAtMu(t, "(*Index).Append")
	assertCallNotDone(t, appendErr, "stale append")

	// Let the parked reorg return: it reports only the old-chain heights that
	// existed when it applied; the append has not committed anything.
	close(h.releaseReorg)
	outcome := <-reorgDone
	if outcome.err != nil {
		t.Fatalf("reorg validated against the old chain must succeed: %v", outcome.err)
	}
	if want := []int64{2, 3}; !reflect.DeepEqual(outcome.dropped, want) {
		t.Fatalf("dropped=%v, want %v", outcome.dropped, want)
	}

	// The blocked append now runs against the new main chain: the old parent
	// h3 is gone, so it is rejected — a non-nil INGESTION error, never the
	// query-parameter error ErrInvalidArgument — and stores nothing.
	err := <-appendErr
	if err == nil {
		t.Fatal("stale append parented on the reorged-away h3 must be rejected")
	}
	if errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("ingestion rejection reported as query argument error ErrInvalidArgument: %v", err)
	}
	// A rejected append must never have reached the stored hook, so no height-4
	// record exists even transiently from its perspective.
	select {
	case <-h.appendParked:
		t.Fatal("the rejected stale append reached the stored hook; it must not store h4")
	default:
	}

	f.assertPostInterleavingState(t, index)
	f.assertPostInterleavingQueries(t, index)
}

// TestInterleavingFixtureSerialSemantics is the non-interleaved oracle: both
// operations, each on its own against the chain state in effect, behave as the
// interleaving tests assume, fixing the literal dropped lists and the stale
// append's rejection for the exact fixture before concurrency is introduced.
func TestInterleavingFixtureSerialSemantics(t *testing.T) {
	f := interleavingFixture()

	t.Run("append then reorg drops 2,3,4", func(t *testing.T) {
		index := f.build(t)
		if err := index.Append(f.appended); err != nil {
			t.Fatalf("append extending the old tip must succeed: %v", err)
		}
		dropped, err := index.Reorg(f.branch)
		if err != nil {
			t.Fatalf("reorg refused: %v", err)
		}
		if want := []int64{2, 3, 4}; !reflect.DeepEqual(dropped, want) {
			t.Fatalf("dropped=%v, want %v", dropped, want)
		}
		f.assertPostInterleavingState(t, index)
		f.assertPostInterleavingQueries(t, index)
	})

	t.Run("reorg then append rejected with no residue", func(t *testing.T) {
		index := f.build(t)
		dropped, err := index.Reorg(f.branch)
		if err != nil {
			t.Fatalf("reorg refused: %v", err)
		}
		if want := []int64{2, 3}; !reflect.DeepEqual(dropped, want) {
			t.Fatalf("dropped=%v, want %v", dropped, want)
		}
		blocks, byHash, tip := snapshot(index)
		err = index.Append(f.appended)
		if err == nil {
			t.Fatal("stale append parented on removed h3 must be rejected")
		}
		if errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("ingestion rejection must not be ErrInvalidArgument: %v", err)
		}
		requireUnchanged(t, index, blocks, byHash, tip)
		f.assertPostInterleavingState(t, index)
		f.assertPostInterleavingQueries(t, index)
	})
}
