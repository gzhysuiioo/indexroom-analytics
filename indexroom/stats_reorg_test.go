package indexroom

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// This file guards QueryTimeStats against a main-chain reorg that makes the
// chain shorter. A query that is in flight when the reorg commits may answer
// from either the old or the new chain, but every part of ONE returned
// TimeStats — per-bucket counts, whole-window totals, the resolved height
// range, and the missing-time-block count — must come from that same complete
// chain. After the reorg has returned, every query must reflect the new chain
// only: transactions at removed heights disappear and the upper height moves
// with the shortened tip.
//
// The scenario (window [0,30), step 10 -> [0,10) [10,20) [20,30); height
// lower bound fixed at 1, upper bound left at zero so it pins the observed
// chain tip):
//
//	Retained ancestor (both chains):
//	  h1 t=0  txs [a, a]    // same id twice in one block/segment
//	  h2 t=5  txs [b]
//	Old branch, tip 5:
//	  h3 t=15 txs [a, c]    // a crosses segments (also in h1)
//	  h4 t=-  txs [d, d, e] // missing timestamp: enters no segment, and even
//	                        // a block with several matching txs is never counted
//	  h5 t=25 txs [f]       // only filler of [20,30) on the old chain
//	New shorter branch, tip 4 (replaces h3..h5, rooted at the retained h2):
//	  j3 t=10 txs [p, p, q] // different ids and multiplicities from h3
//	  j4 t=0  txs [r]       // zero timestamp: a real time, lands in [0,10);
//	                        // different time distribution, h5 simply vanishes
//
// Old-chain result (range 1..5, one missing-time block):
//
//	[0,10):  occ 3 (a,a,b), distinct {a,b}=2, blocks 2
//	[10,20): occ 3? no — [a,c] -> occ 2, distinct {a,c}=2, blocks 1
//	[20,30): occ 1 (f),      distinct {f}=1,  blocks 1
//	totals: occ 6, window-distinct {a,b,c,f}=4, blocks 4, missing 1
//
// New-chain result (range 1..4, no missing-time block, [20,30) returned zero):
//
//	[0,10):  occ 4 (a,a,b,r), distinct {a,b,r}=3, blocks 3
//	[10,20): occ 3 (p,p,q),     distinct {p,q}=2,  blocks 1
//	[20,30): occ 0, distinct 0, blocks 0
//	totals: occ 7, window-distinct {a,b,p,q,r}=5, blocks 4, missing 0
//
// The two complete answers differ in every discriminating field, so any
// stitched-together answer stands out.

var reorgStatsQuery = TimeStatsQuery{From: 1, Start: 0, End: 30, StepSeconds: 10}

func reorgOldChainBlocks() []Block {
	return []Block{
		timeBlock(1, "h1", "g", []string{"a", "a"}, intptr(0)),
		timeBlock(2, "h2", "h1", []string{"b"}, intptr(5)),
		timeBlock(3, "h3", "h2", []string{"a", "c"}, intptr(15)),
		timeBlock(4, "h4", "h3", []string{"d", "d", "e"}, nil),
		timeBlock(5, "h5", "h4", []string{"f"}, intptr(25)),
	}
}

func reorgNewBranch() []Block {
	return []Block{
		timeBlock(3, "j3", "h2", []string{"p", "p", "q"}, intptr(10)),
		timeBlock(4, "j4", "j3", []string{"r"}, intptr(0)),
	}
}

func buildOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, b := range reorgOldChainBlocks() {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at %d: %v", b.Height, err)
		}
	}
	return index
}

// literalOldStats is the hand-derived complete old-chain answer.
func literalOldStats() TimeStats {
	return TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 3, DistinctTxIDs: 2, Blocks: 2},
			{Start: 10, End: 20, TxCount: 2, DistinctTxIDs: 2, Blocks: 1},
			{Start: 20, End: 30, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
		},
		Totals:            TimeTotals{TxCount: 6, DistinctTxIDs: 4, Blocks: 4},
		FromHeight:        1,
		ToHeight:          5,
		MissingTimeBlocks: 1,
	}
}

// literalNewStats is the hand-derived complete new-chain answer.
func literalNewStats() TimeStats {
	return TimeStats{
		Buckets: []TimeBucket{
			{Start: 0, End: 10, TxCount: 4, DistinctTxIDs: 3, Blocks: 3},
			{Start: 10, End: 20, TxCount: 3, DistinctTxIDs: 2, Blocks: 1},
			{Start: 20, End: 30, TxCount: 0, DistinctTxIDs: 0, Blocks: 0},
		},
		Totals:            TimeTotals{TxCount: 7, DistinctTxIDs: 5, Blocks: 4},
		FromHeight:        1,
		ToHeight:          4,
		MissingTimeBlocks: 0,
	}
}

// referenceTimeStats is an independent oracle: it derives the full TimeStats
// from an explicitly supplied, complete ordered chain rather than from the
// index under test, so a shared bug in QueryTimeStats cannot make an
// expectation match itself.
func referenceTimeStats(chain []Block, q TimeStatsQuery) TimeStats {
	tip := int64(len(chain))
	from := q.From
	if from == 0 {
		from = 1
	}
	to := q.To
	if to == 0 || to > tip {
		to = tip
	}
	width := q.End - q.Start
	count := width / q.StepSeconds
	if width%q.StepSeconds != 0 {
		count++
	}
	stats := TimeStats{
		Buckets:    make([]TimeBucket, count),
		FromHeight: from,
		ToHeight:   to,
	}
	for i := range stats.Buckets {
		stats.Buckets[i].Start = q.Start + q.StepSeconds*int64(i)
		end := q.Start + q.StepSeconds*(int64(i)+1)
		if end > q.End {
			end = q.End
		}
		stats.Buckets[i].End = end
	}
	if from > to {
		return stats
	}
	var filter map[string]struct{}
	if len(q.TxIDs) > 0 {
		filter = map[string]struct{}{}
		for _, id := range q.TxIDs {
			filter[id] = struct{}{}
		}
	}
	type acc struct {
		occ    int64
		ids    map[string]struct{}
		blocks int64
	}
	per := make([]acc, count)
	windowIDs := map[string]struct{}{}
	for height := from; height <= to; height++ {
		block := chain[height-1]
		if block.Time == nil {
			stats.MissingTimeBlocks++
			continue
		}
		t := *block.Time
		if t < q.Start || t >= q.End {
			continue
		}
		bucket := (t - q.Start) / q.StepSeconds
		matched := false
		for _, tx := range block.Txs {
			if filter != nil {
				if _, ok := filter[tx]; !ok {
					continue
				}
			}
			stats.Totals.TxCount++
			per[bucket].occ++
			if per[bucket].ids == nil {
				per[bucket].ids = map[string]struct{}{}
			}
			per[bucket].ids[tx] = struct{}{}
			windowIDs[tx] = struct{}{}
			matched = true
		}
		if matched {
			per[bucket].blocks++
			stats.Totals.Blocks++
		}
	}
	for i := range per {
		stats.Buckets[i].TxCount = per[i].occ
		stats.Buckets[i].DistinctTxIDs = int64(len(per[i].ids))
		stats.Buckets[i].Blocks = per[i].blocks
	}
	stats.Totals.DistinctTxIDs = int64(len(windowIDs))
	return stats
}

// classifyTimeStats accepts got only when it is byte-for-byte the complete
// answer of exactly one chain. Anything else — fields stitched from old and
// new, or plain corruption — is rejected with a report naming the offending
// piece. It deliberately does NOT treat "per-bucket occurrence counts sum to
// the total" as sufficient: that equality holds for several mixed answers.
func classifyTimeStats(got, oldRef, newRef TimeStats) error {
	if reflect.DeepEqual(got, oldRef) {
		return nil
	}
	if reflect.DeepEqual(got, newRef) {
		return nil
	}
	var report strings.Builder
	fmt.Fprintln(&report, "QueryTimeStats return does not match one complete main chain:")
	if len(got.Buckets) != len(oldRef.Buckets) || len(got.Buckets) != len(newRef.Buckets) {
		fmt.Fprintf(&report, "  Buckets length        got=%d old=%d new=%d -> neither\n",
			len(got.Buckets), len(oldRef.Buckets), len(newRef.Buckets))
		return fmt.Errorf("%s  => CORRUPT: segment count matches neither chain", report.String())
	}
	tags := map[string]bool{}
	tag := func(name string, gotV, oldV, newV any) string {
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
		fmt.Fprintf(&report, "  %-22s got=%v old=%v new=%v -> %s\n", name, gotV, oldV, newV, label)
		return label
	}
	tag("FromHeight", got.FromHeight, oldRef.FromHeight, newRef.FromHeight)
	tag("ToHeight", got.ToHeight, oldRef.ToHeight, newRef.ToHeight)
	tag("MissingTimeBlocks", got.MissingTimeBlocks, oldRef.MissingTimeBlocks, newRef.MissingTimeBlocks)
	tag("Totals", got.Totals, oldRef.Totals, newRef.Totals)
	for i := range got.Buckets {
		tag(fmt.Sprintf("Buckets[%d]", i), got.Buckets[i], oldRef.Buckets[i], newRef.Buckets[i])
	}
	switch {
	case tags["old"] && tags["new"]:
		fmt.Fprintf(&report, "  => MIXED: pieces come from both the old and the new chain; one return must reflect a single main chain")
	default:
		fmt.Fprintf(&report, "  => CORRUPT: matches neither the old nor the new chain")
	}
	return fmt.Errorf("%s", report.String())
}

// bucketOccurrenceSumEqualsTotal is the deliberately weak check that this
// regression must be stronger than: it holds for several old/new mixtures.
func bucketOccurrenceSumEqualsTotal(s TimeStats) bool {
	var sum int64
	for _, b := range s.Buckets {
		sum += b.TxCount
	}
	return sum == s.Totals.TxCount
}

func TestReorgTimeStatsReferenceMatchesHandDerived(t *testing.T) {
	oldChain := reorgOldChainBlocks()
	if got, want := referenceTimeStats(oldChain, reorgStatsQuery), literalOldStats(); !reflect.DeepEqual(got, want) {
		t.Fatalf("old-chain oracle=%+v\nwant=%+v", got, want)
	}
	// The oracle derives the new chain from the retained ancestor plus the
	// replacement branch, exactly what the index must hold after the reorg.
	newChain := append(append([]Block{}, oldChain[:2]...), reorgNewBranch()...)
	if got, want := referenceTimeStats(newChain, reorgStatsQuery), literalNewStats(); !reflect.DeepEqual(got, want) {
		t.Fatalf("new-chain oracle=%+v\nwant=%+v", got, want)
	}
}

// The counting semantics the regression rests on, asserted directly for each
// chain so the guarantees do not hide inside a DeepEqual.
func TestReorgTimeStatsCountingSemanticsOnBothChains(t *testing.T) {
	old := literalOldStats()
	if old.Buckets[0].TxCount != 3 || old.Buckets[0].DistinctTxIDs != 2 {
		t.Fatalf("old [0,10): duplicate a must count twice but as one id: %+v", old.Buckets[0])
	}
	if old.Buckets[1].DistinctTxIDs != 2 {
		t.Fatalf("old [10,20) distinct: %+v", old.Buckets[1])
	}
	if old.Totals.DistinctTxIDs != 5-1 { // a in two segments must count once over the window
		t.Fatalf("old window distinct must dedup a across segments: %+v", old.Totals)
	}
	if old.MissingTimeBlocks != 1 {
		t.Fatalf("missing-time h4 must count once toward missing: %d", old.MissingTimeBlocks)
	}
	if old.Totals.Blocks != 4 {
		t.Fatalf("h4 has three matching txs but no time; it must add no block, want 4: %d", old.Totals.Blocks)
	}

	new := literalNewStats()
	if new.MissingTimeBlocks != 0 {
		t.Fatalf("zero-time j4 is a real time, not missing: %d", new.MissingTimeBlocks)
	}
	if new.Buckets[0].TxCount != 4 || new.Buckets[0].Blocks != 3 {
		t.Fatalf("zero-time j4 must enter [0,10): %+v", new.Buckets[0])
	}
	if len(new.Buckets) != 3 || new.Buckets[2] != (TimeBucket{Start: 20, End: 30}) {
		t.Fatalf("empty [20,30) segment must still be returned zeroed: %+v", new.Buckets)
	}
	if new.Buckets[1].TxCount != 3 || new.Buckets[1].DistinctTxIDs != 2 {
		t.Fatalf("new [10,20): p twice counts twice but as one id: %+v", new.Buckets[1])
	}
	if new.ToHeight != 4 || new.Totals.TxCount != 7 {
		t.Fatalf("shortened chain must pin tip 4 and drop f: %+v", new)
	}
}

func TestQueryTimeStatsBeforeAndAfterShorteningReorg(t *testing.T) {
	index := buildOldChain(t)
	oldRef := literalOldStats()

	before, err := index.QueryTimeStats(reorgStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if err := classifyTimeStats(before, oldRef, literalNewStats()); err != nil {
		t.Fatal(err)
	}

	dropped, err := index.Reorg(reorgNewBranch())
	if err != nil {
		t.Fatalf("shortening reorg refused: %v", err)
	}
	if want := []int64{3, 4, 5}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped=%v, want %v", dropped, want)
	}
	if index.Tip != 4 {
		t.Fatalf("tip=%d, want shortened tip 4", index.Tip)
	}

	// After the reorg returns, only the complete new-chain answer is allowed.
	after, err := index.QueryTimeStats(reorgStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, literalNewStats()) {
		t.Fatalf("post-reorg query not wholly on the new chain:\n%+v", after)
	}
	if after.ToHeight != 4 {
		t.Fatalf("upper height must follow the shortened tip, got %d", after.ToHeight)
	}
	if after.Buckets[2].TxCount != 0 || after.MissingTimeBlocks != 0 {
		t.Fatalf("removed height 5 tx f or old missing count leaked: %+v", after)
	}
	// The removed height really is gone from the chain.
	if _, ok := index.Blocks[5]; ok {
		t.Fatal("height 5 still present after shortening reorg")
	}
	if _, ok := index.ByHash["h5"]; ok {
		t.Fatal("removed hash h5 still indexed")
	}
}

// Installs one-shot rendezvous hooks and guarantees they are removed.
type reorgStatsHooks struct {
	queryParked   chan struct{}
	releaseQuery  chan struct{}
	reorgParked   chan struct{}
	releaseReorg  chan struct{}
	queryHookOnce sync.Once
	reorgHookOnce sync.Once
}

func installReorgStatsHooks(t *testing.T) *reorgStatsHooks {
	t.Helper()
	h := &reorgStatsHooks{
		queryParked:  make(chan struct{}),
		releaseQuery: make(chan struct{}),
		reorgParked:  make(chan struct{}),
		releaseReorg: make(chan struct{}),
	}
	statsQueryHookLocked = func(to int64) {
		h.queryHookOnce.Do(func() { close(h.queryParked) })
		<-h.releaseQuery
	}
	reorgAppliedHookLocked = func(newTip int64) {
		if newTip != 4 {
			t.Errorf("reorg hook saw newTip=%d, want 4", newTip)
		}
		h.reorgHookOnce.Do(func() { close(h.reorgParked) })
		<-h.releaseReorg
	}
	t.Cleanup(func() {
		statsQueryHookLocked = nil
		reorgAppliedHookLocked = nil
	})
	return h
}

// A query that has pinned the OLD tip (5) and is still holding the lock when
// the reorg starts must finish on the complete old chain.
func TestQueryTimeStatsInFlightObservesCompleteOldChain(t *testing.T) {
	index := buildOldChain(t)
	h := installReorgStatsHooks(t)
	oldRef, newRef := literalOldStats(), literalNewStats()

	result := make(chan TimeStats, 1)
	errCh := make(chan error, 1)
	go func() {
		stats, err := index.QueryTimeStats(reorgStatsQuery)
		if err != nil {
			errCh <- err
			return
		}
		result <- stats
	}()

	<-h.queryParked // query holds index.mu and has pinned tip 5
	reorgDone := make(chan struct{})
	go func() {
		if _, err := index.Reorg(reorgNewBranch()); err != nil {
			errCh <- err
		}
		close(reorgDone)
	}()

	// Let the parked query scan and finish first; the reorg cannot touch the
	// chain until the query drops the lock.
	close(h.releaseQuery)
	var got TimeStats
	select {
	case got = <-result:
	case err := <-errCh:
		t.Fatalf("concurrent call failed: %v", err)
	}
	if err := classifyTimeStats(got, oldRef, newRef); err != nil {
		t.Fatalf("in-flight query did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(got, oldRef) {
		t.Fatalf("query holding the lock across reorg start must see old chain:\n%+v", got)
	}
	close(h.releaseReorg) // unblock the parked reorg
	<-reorgDone
}

// A query issued while a successful reorg is committed but has not yet
// returned (new chain installed under the lock) must wait and then observe
// the complete NEW chain, including the shortened upper height.
func TestQueryTimeStatsInFlightObservesCompleteNewChain(t *testing.T) {
	index := buildOldChain(t)
	h := installReorgStatsHooks(t)
	oldRef, newRef := literalOldStats(), literalNewStats()

	reorgErr := make(chan error, 1)
	go func() {
		_, err := index.Reorg(reorgNewBranch())
		reorgErr <- err
	}()
	<-h.reorgParked // new branch applied (tip 4), Reorg still holds the lock

	result := make(chan TimeStats, 1)
	go func() {
		stats, err := index.QueryTimeStats(reorgStatsQuery)
		if err != nil {
			t.Errorf("query during reorg failed: %v", err)
			return
		}
		result <- stats
	}()

	// Releasing the reorg lets it return and hands the lock to the waiting
	// query; the chain the query can first see is already entirely the new one.
	close(h.releaseReorg)
	if err := <-reorgErr; err != nil {
		t.Fatalf("reorg failed: %v", err)
	}
	close(h.releaseQuery) // no query parks in this timing, but keep the seam drained

	got := <-result
	if err := classifyTimeStats(got, oldRef, newRef); err != nil {
		t.Fatalf("query across reorg commit did not return one complete chain:\n%v", err)
	}
	if !reflect.DeepEqual(got, newRef) {
		t.Fatalf("query after reorg commit must see the complete new chain:\n%+v", got)
	}
}

// The validator must actually catch old/new mixtures — including ones where
// the per-bucket occurrence counts still sum to the window total.
func TestClassifyTimeStatsRejectsMixedAndCorruptReturns(t *testing.T) {
	oldRef, newRef := literalOldStats(), literalNewStats()
	if err := classifyTimeStats(oldRef, oldRef, newRef); err != nil {
		t.Fatalf("pure old answer rejected: %v", err)
	}
	if err := classifyTimeStats(newRef, oldRef, newRef); err != nil {
		t.Fatalf("pure new answer rejected: %v", err)
	}

	// Mixture 1: new-chain buckets and totals but the stale OLD upper height
	// and missing-time count. Bucket occurrences sum to the total, so the
	// naive sum check accepts it; the complete-chain check must not.
	mixed1 := newRef
	mixed1.ToHeight = oldRef.ToHeight
	mixed1.MissingTimeBlocks = oldRef.MissingTimeBlocks
	if !bucketOccurrenceSumEqualsTotal(mixed1) {
		t.Fatal("test setup: mixed1 is meant to pass the weak sum check")
	}
	if err := classifyTimeStats(mixed1, oldRef, newRef); err == nil {
		t.Fatal("new-chain counts under the old tip height were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("mixed1 not reported as MIXED:\n%v", err)
	}

	// Mixture 2: old-chain buckets and totals but the NEW shortened upper
	// height — claims tip 4 while still counting removed height 5's f.
	mixed2 := oldRef
	mixed2.ToHeight = newRef.ToHeight
	if !bucketOccurrenceSumEqualsTotal(mixed2) {
		t.Fatal("test setup: mixed2 is meant to pass the weak sum check")
	}
	if err := classifyTimeStats(mixed2, oldRef, newRef); err == nil {
		t.Fatal("old-chain counts under the new tip height were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("mixed2 not reported as MIXED:\n%v", err)
	}

	// Mixture 3: new range and missing count but old per-bucket shape and a
	// stitched total — cross-component mix with no consistent sum.
	mixed3 := newRef
	mixed3.Buckets = oldRef.Buckets
	mixed3.Totals.TxCount = oldRef.Totals.TxCount
	if err := classifyTimeStats(mixed3, oldRef, newRef); err == nil {
		t.Fatal("old buckets against the new range were accepted")
	} else if !strings.Contains(err.Error(), "MIXED") {
		t.Fatalf("mixed3 not reported as MIXED:\n%v", err)
	}

	// Corruption that belongs to neither chain must also be rejected.
	corrupt := newRef
	corrupt.Buckets[0].TxCount++
	corrupt.Totals.TxCount++
	if err := classifyTimeStats(corrupt, oldRef, newRef); err == nil {
		t.Fatal("corrupt answer matching neither chain was accepted")
	}
}

// Sustained overlap: reorgs alternate between the old and the shortened new
// branch while readers query continuously. Every answer must classify as one
// complete chain; success never depends on which side of the reorg a query
// happened to land. Deterministic, offline, and timing-independent — a torn
// answer fails regardless of scheduling.
func TestQueryTimeStatsRepeatedShorteningReorgsStayConsistent(t *testing.T) {
	index := buildOldChain(t)
	oldRef, newRef := literalOldStats(), literalNewStats()
	oldBranch := reorgOldChainBlocks()[2:] // h3,h4,h5 rooted at h2, restores tip 5

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
				branch := reorgNewBranch()
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
		go func() {
			defer wg.Done()
			for i := 0; i < reads; i++ {
				select {
				case <-stop:
					return
				default:
				}
				stats, err := index.QueryTimeStats(reorgStatsQuery)
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
	close(stop)

	// Leave the index deterministically on the new chain and re-verify.
	if _, err := index.Reorg(reorgNewBranch()); err != nil {
		t.Fatalf("final reorg failed: %v", err)
	}
	final, err := index.QueryTimeStats(reorgStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(final, newRef) {
		t.Fatalf("final state is not the complete new chain:\n%+v", final)
	}
}
