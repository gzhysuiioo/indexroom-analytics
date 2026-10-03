package indexroom

import (
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This file guards QueryTimeStats against a Restore that replaces the whole
// main chain with a snapshot. The promise under test: the index stays
// queryable while the snapshot is still being read, and every QueryTimeStats
// return — per-bucket counts, per-bucket distinct identifiers and block
// counts, whole-window totals, the resolved height range, and the
// missing-time-block count — comes from ONE complete main chain, never a
// stitch of the old chain's transaction counts with the new chain's height,
// missing-time count, or deduplication. After a successful restore, no count
// from the dropped taller chain may linger; after a rejected restore, the
// old chain must be exactly as it was, with no partially read blocks left
// behind.
//
// The scenario (window [0,30), step 10 -> [0,10) [10,20) [20,30); height
// bounds left at their defaults: From zero starts at height 1, To zero pins
// the observed chain tip; no transaction filter):
//
//	Old main chain, tip 4:
//	  h1 t=0  txs [a, a]  // same id twice in one block/segment
//	  h2 t=10 txs [a, b]  // a crosses segments (also in h1)
//	  h3 t=-  txs [a]     // missing timestamp: enters no segment and no
//	                      // block count, but counts toward MissingTimeBlocks
//	  h4 t=20 txs [c]
//	Restored version-2 snapshot, tip 2:
//	  n1 t=0  txs [b]
//	  n2 t=20 txs [b, d, d] // d twice: two occurrences, one distinct id
//
// Old-chain result (range 1..4, one missing-time block):
//
//	[0,10):  occ 2 (a,a), distinct {a}=1,   blocks 1
//	[10,20): occ 2 (a,b), distinct {a,b}=2, blocks 1
//	[20,30): occ 1 (c),   distinct {c}=1,   blocks 1
//	totals: occ 5, window-distinct {a,b,c}=3, blocks 3, missing 1, to 4
//
// New-chain result (range 1..2, no missing-time block, [10,20) stays as an
// empty segment):
//
//	[0,10):  occ 1 (b),     distinct {b}=1,   blocks 1
//	[10,20): occ 0,         distinct 0,       blocks 0
//	[20,30): occ 3 (b,d,d), distinct {b,d}=2, blocks 1
//	totals: occ 4, window-distinct {b,d}=2, blocks 2, missing 0, to 2
//
// The two complete answers differ in every discriminating field, so any
// stitched-together answer stands out.

var restoreStatsQuery = TimeStatsQuery{Start: 0, End: 30, StepSeconds: 10}

func restoreOldChainBlocks() []Block {
	return []Block{
		timeBlock(1, "h1", "g", []string{"a", "a"}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"a", "b"}, intptr(10)),
		timeBlock(3, "h3", "h2", []string{"a"}, nil),
		timeBlock(4, "h4", "h3", []string{"c"}, intptr(20)),
	}
}

func buildRestoreOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range restoreOldChainBlocks() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

// restoreNewSnapshot is the version-2 snapshot that replaces the old chain.
const restoreNewSnapshot = `{"version":2,"tip":2,"blocks":[` +
	`{"height":1,"hash":"n1","parent":"g","txs":["b"],"timestamp":0},` +
	`{"height":2,"hash":"n2","parent":"n1","txs":["b","d","d"],"timestamp":20}` +
	`]}`

// literalRestoreOldStats is the hand-derived complete old-chain answer.
func literalRestoreOldStats() TimeStats {
	return TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 2, DistinctTxIDs: 1, Blocks: 1},
			{Start: 10, End: 20, TxCount: 2, DistinctTxIDs: 2, Blocks: 1},
			{Start: 20, End: 30, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
		},
		Totals:            TimeTotals{TxCount: 5, DistinctTxIDs: 3, Blocks: 3},
		FromHeight:        1,
		ToHeight:          4,
		MissingTimeBlocks: 1,
	}
}

// literalRestoreNewStats is the hand-derived complete restored-chain answer.
func literalRestoreNewStats() TimeStats {
	return TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
			{Start: 10, End: 20, TxCount: 0, DistinctTxIDs: 0, Blocks: 0},
			{Start: 20, End: 30, TxCount: 3, DistinctTxIDs: 2, Blocks: 1},
		},
		Totals:            TimeTotals{TxCount: 4, DistinctTxIDs: 2, Blocks: 2},
		FromHeight:        1,
		ToHeight:          2,
		MissingTimeBlocks: 0,
	}
}

// restoreNewChainBlocks is the restored snapshot as an ordered chain, for
// the independent oracle.
func restoreNewChainBlocks() []Block {
	return []Block{
		timeBlock(1, "n1", "g", []string{"b"}, intptr(0)),
		timeBlock(2, "n2", "n1", []string{"b", "d", "d"}, intptr(20)),
	}
}

func TestRestoreTimeStatsReferenceMatchesHandDerived(t *testing.T) {
	if got, want := referenceTimeStats(restoreOldChainBlocks(), restoreStatsQuery), literalRestoreOldStats(); !reflect.DeepEqual(got, want) {
		t.Fatalf("old-chain oracle=%+v\nwant=%+v", got, want)
	}
	if got, want := referenceTimeStats(restoreNewChainBlocks(), restoreStatsQuery), literalRestoreNewStats(); !reflect.DeepEqual(got, want) {
		t.Fatalf("new-chain oracle=%+v\nwant=%+v", got, want)
	}
}

// The counting semantics the regression rests on, asserted directly for each
// chain so the guarantees do not hide inside a DeepEqual.
func TestRestoreTimeStatsCountingSemanticsOnBothChains(t *testing.T) {
	old := literalRestoreOldStats()
	if old.Buckets[0].TxCount != 2 || old.Buckets[0].DistinctTxIDs != 1 {
		t.Fatalf("old [0,10): duplicate a must count twice but as one id: %+v", old.Buckets[0])
	}
	if old.Totals.DistinctTxIDs != 3 { // a in two segments counts once over the window
		t.Fatalf("old window distinct must dedup a across segments: %+v", old.Totals)
	}
	if old.MissingTimeBlocks != 1 || old.Totals.Blocks != 3 {
		t.Fatalf("untimed h3 must count toward missing only, never toward blocks: %+v", old)
	}
	if old.ToHeight != 4 {
		t.Fatalf("old chain pins tip 4: %+v", old)
	}

	new := literalRestoreNewStats()
	if new.Buckets[2].TxCount != 3 || new.Buckets[2].DistinctTxIDs != 2 {
		t.Fatalf("new [20,30): d twice counts twice but as one id: %+v", new.Buckets[2])
	}
	if len(new.Buckets) != 3 || new.Buckets[1] != (TimeBucket{Start: 10, End: 20}) {
		t.Fatalf("empty [10,20) segment must still be returned zeroed: %+v", new.Buckets)
	}
	if new.MissingTimeBlocks != 0 || new.ToHeight != 2 || new.Totals.TxCount != 4 {
		t.Fatalf("restored chain must pin tip 2 with no missing-time block: %+v", new)
	}
}

func TestQueryTimeStatsBeforeAndAfterRestore(t *testing.T) {
	index := buildRestoreOldChain(t)

	before, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, literalRestoreOldStats()) {
		t.Fatalf("pre-restore query not wholly on the old chain:\n%+v", before)
	}

	if err := index.Restore(strings.NewReader(restoreNewSnapshot)); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	if index.Tip != 2 {
		t.Fatalf("tip=%d, want restored tip 2", index.Tip)
	}

	// After the restore returns, only the complete new-chain answer is
	// allowed: nothing from the dropped heights 3 and 4 may linger in any
	// count, and the upper height must follow the shortened tip.
	after, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, literalRestoreNewStats()) {
		t.Fatalf("post-restore query not wholly on the new chain:\n%+v", after)
	}
	if after.ToHeight != 2 {
		t.Fatalf("upper height must follow the restored tip, got %d", after.ToHeight)
	}
	if after.Totals.TxCount != 4 || after.MissingTimeBlocks != 0 {
		t.Fatalf("old-chain transactions or missing-time count leaked: %+v", after)
	}
	for height := int64(3); height <= 4; height++ {
		if _, ok := index.Blocks[height]; ok {
			t.Fatalf("old height %d still present after restore", height)
		}
	}
	for _, gone := range []string{"h1", "h2", "h3", "h4"} {
		if _, ok := index.ByHash[gone]; ok {
			t.Fatalf("replaced hash %s still indexed", gone)
		}
	}
}

// While the snapshot stream is still being read, the restore holds no lock
// and a statistics query must complete with the complete old-chain answer.
func TestQueryTimeStatsWhileRestoreStillReading(t *testing.T) {
	index := buildRestoreOldChain(t)
	oldRef, newRef := literalRestoreOldStats(), literalRestoreNewStats()

	// Split the snapshot between the two blocks: the restore consumes the
	// first block and then blocks waiting for the rest of the stream.
	cut := strings.Index(restoreNewSnapshot, `{"height":2`)
	if cut <= 0 {
		t.Fatalf("test setup: snapshot split point not found in %s", restoreNewSnapshot)
	}
	pr, pw := io.Pipe()
	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(pr)
	}()

	// A pipe write returns only once the reader has consumed the bytes, so
	// after this write the restore is parked inside the read, before it
	// could possibly have taken the index lock.
	writeDone := make(chan error, 1)
	go func() {
		_, err := pw.Write([]byte(restoreNewSnapshot[:cut]))
		writeDone <- err
	}()
	if err := <-writeDone; err != nil {
		t.Fatalf("first snapshot chunk write: %v", err)
	}
	select {
	case err := <-restoreDone:
		t.Fatalf("restore finished before the snapshot was fully written: %v", err)
	default:
	}

	// The query must not wait for the restore and must answer from the
	// complete old chain.
	during, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(during, oldRef) {
		t.Fatalf("query during snapshot read must see the complete old chain:\n%+v", during)
	}

	// Finish the stream: the restore applies, and the next query sees only
	// the complete new chain.
	go func() {
		_, err := pw.Write([]byte(restoreNewSnapshot[cut:]))
		writeDone <- err
	}()
	if err := <-writeDone; err != nil {
		t.Fatalf("second snapshot chunk write: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("closing snapshot stream: %v", err)
	}
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	after, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, newRef) {
		t.Fatalf("query after restore must see the complete new chain:\n%+v", after)
	}
}

// A query that has pinned the OLD tip (4) and is still holding the lock when
// the restore starts must finish on the complete old chain; the restore
// commits only afterwards, and the next query sees the complete new chain.
func TestQueryTimeStatsInFlightAcrossRestoreCommit(t *testing.T) {
	index := buildRestoreOldChain(t)
	oldRef, newRef := literalRestoreOldStats(), literalRestoreNewStats()

	queryParked := make(chan struct{})
	releaseQuery := make(chan struct{})
	var queryHookOnce sync.Once
	statsQueryHookLocked = func(to int64) {
		queryHookOnce.Do(func() {
			if to != 4 {
				t.Errorf("parked query resolved to=%d, want old tip 4", to)
			}
			close(queryParked)
		})
		<-releaseQuery
	}
	t.Cleanup(func() { statsQueryHookLocked = nil })

	result := make(chan TimeStats, 1)
	errCh := make(chan error, 1)
	go func() {
		stats, err := index.QueryTimeStats(restoreStatsQuery)
		if err != nil {
			errCh <- err
			return
		}
		result <- stats
	}()

	<-queryParked // query holds index.mu and has pinned tip 4
	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(strings.NewReader(restoreNewSnapshot))
	}()

	// Let the parked query scan and finish first; the restore cannot swap
	// the chain until the query drops the lock.
	close(releaseQuery)
	var got TimeStats
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("concurrent query failed: %v", err)
	}
	if err := classifyTimeStats(got, oldRef, newRef); err != nil {
		t.Fatalf("in-flight query did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(got, oldRef) {
		t.Fatalf("query holding the lock across restore start must see old chain:\n%+v", got)
	}
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore failed: %v", err)
	}

	after, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, newRef) {
		t.Fatalf("query after restore commit must see the complete new chain:\n%+v", after)
	}
}

// A query issued while a successful restore is committed but has not yet
// returned (new chain installed under the lock) must wait and then observe
// the complete NEW chain, including the shortened upper height.
func TestQueryTimeStatsIssuedDuringRestoreCommitSeesNewChain(t *testing.T) {
	index := buildRestoreOldChain(t)
	oldRef, newRef := literalRestoreOldStats(), literalRestoreNewStats()

	restoreParked := make(chan struct{})
	releaseRestore := make(chan struct{})
	var restoreHookOnce sync.Once
	restoreAppliedHookLocked = func(newTip int64) {
		if newTip != 2 {
			t.Errorf("restore hook saw newTip=%d, want 2", newTip)
		}
		restoreHookOnce.Do(func() { close(restoreParked) })
		<-releaseRestore
	}
	t.Cleanup(func() { restoreAppliedHookLocked = nil })

	restoreDone := make(chan error, 1)
	go func() {
		restoreDone <- index.Restore(strings.NewReader(restoreNewSnapshot))
	}()
	<-restoreParked // snapshot chain installed (tip 2), Restore still holds the lock

	result := make(chan TimeStats, 1)
	errCh := make(chan error, 1)
	go func() {
		stats, err := index.QueryTimeStats(restoreStatsQuery)
		if err != nil {
			errCh <- err
			return
		}
		result <- stats
	}()

	// Releasing the restore lets it return and hands the lock to the waiting
	// query; the chain the query can first see is already entirely the new one.
	close(releaseRestore)
	if err := <-restoreDone; err != nil {
		t.Fatalf("restore failed: %v", err)
	}
	var got TimeStats
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("query during restore commit failed: %v", err)
	}
	if err := classifyTimeStats(got, oldRef, newRef); err != nil {
		t.Fatalf("query across restore commit did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(got, newRef) {
		t.Fatalf("query after restore commit must see the complete new chain:\n%+v", got)
	}
}

// A version-2 snapshot whose LAST block carries an illegal negative
// timestamp is rejected only when that final block is parsed. The restore
// must report ErrInvalidSnapshot and leave the old chain — and every
// statistic derived from it — exactly as it was, with no partially read
// blocks left behind.
func TestRestoreNegativeTimeInLastBlockKeepsOldChainStats(t *testing.T) {
	index := buildRestoreOldChain(t)
	oldRef := literalRestoreOldStats()

	badSnapshot := `{"version":2,"tip":2,"blocks":[` +
		`{"height":1,"hash":"n1","parent":"g","txs":["b"],"timestamp":0},` +
		`{"height":2,"hash":"n2","parent":"n1","txs":["b","d","d"],"timestamp":-5}` +
		`]}`
	err := index.Restore(strings.NewReader(badSnapshot))
	if !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("restore err=%v, want ErrInvalidSnapshot", err)
	}

	// The chain is untouched: same tip, same blocks, no snapshot hash
	// leaked into the index.
	if index.Tip != 4 {
		t.Fatalf("tip=%d after rejected restore, want 4", index.Tip)
	}
	if len(index.Blocks) != 4 || len(index.ByHash) != 4 {
		t.Fatalf("rejected restore left partial blocks: blocks=%d hashes=%d", len(index.Blocks), len(index.ByHash))
	}
	for _, leaked := range []string{"n1", "n2"} {
		if _, ok := index.ByHash[leaked]; ok {
			t.Fatalf("rejected snapshot hash %s entered the index", leaked)
		}
	}

	after, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, oldRef) {
		t.Fatalf("stats after rejected restore differ from the old chain:\n%+v", after)
	}
}

// The validator must actually catch old/new mixtures in this scenario —
// including old-chain transaction counts reported under the new chain's
// height, missing-time count, or deduplication.
func TestClassifyTimeStatsRejectsRestoreMixtures(t *testing.T) {
	oldRef, newRef := literalRestoreOldStats(), literalRestoreNewStats()
	if err := classifyTimeStats(oldRef, oldRef, newRef); err != nil {
		t.Fatalf("pure old answer rejected: %v", err)
	}
	if err := classifyTimeStats(newRef, oldRef, newRef); err != nil {
		t.Fatalf("pure new answer rejected: %v", err)
	}

	// Old-chain buckets and totals under the NEW upper height: claims tip 2
	// while still counting the dropped heights 3 and 4.
	mixed := oldRef
	mixed.ToHeight = newRef.ToHeight
	if err := classifyTimeStats(mixed, oldRef, newRef); err == nil {
		t.Fatal("old-chain counts under the new tip height were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("old counts under new height not reported as MIXED:\n%v", err)
	}

	// New-chain counts under the OLD upper height and missing-time count.
	mixed = newRef
	mixed.ToHeight = oldRef.ToHeight
	mixed.MissingTimeBlocks = oldRef.MissingTimeBlocks
	if err := classifyTimeStats(mixed, oldRef, newRef); err == nil {
		t.Fatal("new-chain counts under the old tip height were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("new counts under old height not reported as MIXED:\n%v", err)
	}

	// New range and missing count but the old chain's deduplication result.
	mixed = newRef
	mixed.Totals.DistinctTxIDs = oldRef.Totals.DistinctTxIDs
	if err := classifyTimeStats(mixed, oldRef, newRef); err == nil {
		t.Fatal("old-chain distinct count under the new chain was accepted")
	}
}

// Sustained overlap: restores alternate between the old-chain snapshot and
// the replacement snapshot while readers query continuously. Every answer
// must classify as one complete chain; success never depends on which side
// of a restore a query happened to land. Deterministic, offline, and
// timing-independent — a torn answer fails regardless of scheduling.
func TestQueryTimeStatsRepeatedRestoresStayConsistent(t *testing.T) {
	index := buildRestoreOldChain(t)
	oldRef, newRef := literalRestoreOldStats(), literalRestoreNewStats()
	oldSnapshot := exportString(t, index)
	if !strings.Contains(oldSnapshot, `"version":2`) {
		t.Fatalf("test setup: old chain must export as version 2: %s", oldSnapshot)
	}

	const restorers = 2
	const rounds = 40
	const readers = 4
	const reads = 80

	var wg sync.WaitGroup
	for r := 0; r < restorers; r++ {
		wg.Add(1)
		go func(seed int) {
			defer wg.Done()
			for i := 0; i < rounds; i++ {
				snap := restoreNewSnapshot
				if (i+seed)%2 == 0 {
					snap = oldSnapshot
				}
				if err := index.Restore(strings.NewReader(snap)); err != nil {
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
				stats, err := index.QueryTimeStats(restoreStatsQuery)
				if err != nil {
					t.Errorf("query failed: %v", err)
					return
				}
				if err := classifyTimeStats(stats, oldRef, newRef); err != nil {
					t.Errorf("%v", err)
					return
				}
			}
		}()
	}
	wg.Wait()

	// Leave the index deterministically on the new chain and re-verify.
	if err := index.Restore(strings.NewReader(restoreNewSnapshot)); err != nil {
		t.Fatalf("final restore failed: %v", err)
	}
	final, err := index.QueryTimeStats(restoreStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(final, newRef) {
		t.Fatalf("final state is not the complete new chain:\n%+v", final)
	}
}
