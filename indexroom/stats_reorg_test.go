package indexroom

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// These tests pin down the QueryTimeStats contract across a legal reorg that
// starts from a known ancestor and shortens the main chain: a query running
// while the branch is replaced may observe the old or the new chain, but its
// buckets, totals, resolved heights, and missing-time count must all come
// from one complete main-chain state. Checking only that per-bucket counts
// sum to the window total would not catch a mix, so every result is compared
// in full against the two complete states.

// shorteningReorgFixture builds the shared scenario:
//
//	height 1 (retained): txs [keep keep], time 0 — a real zero timestamp
//	height 2 (retained): txs [keep-missing], no timestamp
//	old branch, tip 6:   heights 3..6 with old-* txs, times 5/12/18/missing
//	new branch, tip 4:   heights 3..4 with new-* txs, times 7/25
//
// The replacement branch carries different identifiers, duplicate counts, and
// a different time distribution, so the old and new chains each produce a
// full statistics result that differs in every field: bucket contents,
// totals, resolved ToHeight, and MissingTimeBlocks.
func shorteningReorgFixture(t *testing.T) (index *Index, oldBranch, newBranch []Block) {
	t.Helper()
	index = chain(t,
		timeBlock(1, "keep-1", "genesis", []string{"keep", "keep"}, intptr(0)),
		timeBlock(2, "keep-2", "keep-1", []string{"keep-missing"}, nil),
	)
	oldBranch = []Block{
		timeBlock(3, "old-3", "keep-2", []string{"old-a", "old-b"}, intptr(5)),
		timeBlock(4, "old-4", "old-3", []string{"old-a", "old-a"}, intptr(12)),
		timeBlock(5, "old-5", "old-4", []string{"old-c"}, intptr(18)),
		timeBlock(6, "old-6", "old-5", []string{"old-miss"}, nil),
	}
	newBranch = []Block{
		timeBlock(3, "new-3", "keep-2", []string{"new-a", "new-a", "new-b"}, intptr(7)),
		timeBlock(4, "new-4", "new-3", []string{"new-a"}, intptr(25)),
	}
	if _, err := index.Reorg(oldBranch); err != nil {
		t.Fatalf("building the old branch failed: %v", err)
	}
	return index, oldBranch, newBranch
}

// reorgStatsQuery is the fixed query for the fixture: the window, step, and
// height lower bound stay constant across the reorg and the height upper
// bound is left unspecified, pinning the chain tip observed by each call.
var reorgStatsQuery = TimeStatsQuery{From: 1, To: 0, Start: 0, End: 30, StepSeconds: 10}

// wantOldChainStats is the complete expected result for the old chain
// (tip 6). Height 1's zero timestamp lands in bucket [0,10); heights 2 and 6
// carry no timestamp and only count as missing; "old-a" spans two buckets
// but is one distinct identifier over the window; height 4's duplicate
// "old-a" counts two occurrences in one block; bucket [20,30) has no match
// and is returned zeroed.
var wantOldChainStats = TimeStats{
	Buckets: []TimeBucket{
		{Start: 0, End: 10, TxCount: 4, DistinctTxIDs: 3, Blocks: 2},
		{Start: 10, End: 20, TxCount: 3, DistinctTxIDs: 2, Blocks: 2},
		{Start: 20, End: 30},
	},
	Totals:            TimeTotals{TxCount: 7, DistinctTxIDs: 4, Blocks: 4},
	FromHeight:        1,
	ToHeight:          6,
	MissingTimeBlocks: 2,
}

// wantNewChainStats is the complete expected result for the shortened new
// chain (tip 4). The retained heights contribute exactly as before; the
// replacement branch moves its matches to buckets [0,10) and [20,30), leaves
// [10,20) empty, and "new-a" spans two buckets but is one distinct identifier
// over the window. ToHeight follows the shortened tip and only the retained
// missing-time block remains in range.
var wantNewChainStats = TimeStats{
	Buckets: []TimeBucket{
		{Start: 0, End: 10, TxCount: 5, DistinctTxIDs: 3, Blocks: 2},
		{Start: 10, End: 20},
		{Start: 20, End: 30, TxCount: 1, DistinctTxIDs: 1, Blocks: 1},
	},
	Totals:            TimeTotals{TxCount: 6, DistinctTxIDs: 3, Blocks: 3},
	FromHeight:        1,
	ToHeight:          4,
	MissingTimeBlocks: 1,
}

// diffTimeStats names every field in which got diverges from want, so a
// failure points at the exact part of the response that violates the
// single-chain requirement instead of dumping two structs.
func diffTimeStats(got, want TimeStats) string {
	var parts []string
	if !reflect.DeepEqual(got.Buckets, want.Buckets) {
		parts = append(parts, fmt.Sprintf("buckets got %+v want %+v", got.Buckets, want.Buckets))
	}
	if got.Totals != want.Totals {
		parts = append(parts, fmt.Sprintf("totals got %+v want %+v", got.Totals, want.Totals))
	}
	if got.FromHeight != want.FromHeight {
		parts = append(parts, fmt.Sprintf("from-height got %d want %d", got.FromHeight, want.FromHeight))
	}
	if got.ToHeight != want.ToHeight {
		parts = append(parts, fmt.Sprintf("to-height got %d want %d", got.ToHeight, want.ToHeight))
	}
	if got.MissingTimeBlocks != want.MissingTimeBlocks {
		parts = append(parts, fmt.Sprintf("missing-time-blocks got %d want %d", got.MissingTimeBlocks, want.MissingTimeBlocks))
	}
	if len(parts) == 0 {
		return "no differences"
	}
	return strings.Join(parts, "; ")
}

func TestQueryTimeStatsShorteningReorgBeforeAndAfter(t *testing.T) {
	index, _, newBranch := shorteningReorgFixture(t)

	// Before the reorg the query resolves to the old chain in full.
	before, err := index.QueryTimeStats(reorgStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, wantOldChainStats) {
		t.Fatalf("pre-reorg stats violate the old-chain state: %s", diffTimeStats(before, wantOldChainStats))
	}

	// The reorg starts at the known ancestor (height 2) and shortens the
	// chain from tip 6 to tip 4; every old-branch height is dropped.
	dropped, err := index.Reorg(newBranch)
	if err != nil {
		t.Fatalf("shortening reorg refused: %v", err)
	}
	if !reflect.DeepEqual(dropped, []int64{3, 4, 5, 6}) {
		t.Fatalf("dropped=%v, want [3 4 5 6]", dropped)
	}

	// A query issued after the reorg returned reflects only the new chain:
	// transactions at removed heights no longer count and the resolved
	// height cap follows the shortened tip.
	after, err := index.QueryTimeStats(reorgStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after, wantNewChainStats) {
		t.Fatalf("post-reorg stats violate the new-chain state: %s", diffTimeStats(after, wantNewChainStats))
	}
}

func TestQueryTimeStatsDuringShorteningReorgNeverMixesChainStates(t *testing.T) {
	index, oldBranch, newBranch := shorteningReorgFixture(t)

	// Readers query continuously while the main goroutine alternates the
	// chain between the old (tip 6) and new (tip 4) branches. Every single
	// result must equal one of the two complete states; which of the two a
	// given query happens to observe is irrelevant to the outcome.
	stop := make(chan struct{})
	var readers sync.WaitGroup
	// observed counts accepted query results; the reorg loop below runs
	// until readers have genuinely queried alongside the reorgs, so the
	// interleaving is exercised on every run rather than by scheduling
	// luck. It only paces the loop — pass/fail never depends on it.
	var observed atomic.Int64
	// Per-reader observation tallies, written only by that reader and read
	// after readers.Wait(); they are logged, never asserted.
	tallies := make([][2]int, 4)
	for r := 0; r < 4; r++ {
		readers.Add(1)
		go func(reader int) {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				stats, err := index.QueryTimeStats(reorgStatsQuery)
				if err != nil {
					t.Errorf("reader %d: query failed: %v", reader, err)
					return
				}
				switch {
				case reflect.DeepEqual(stats, wantOldChainStats):
					tallies[reader][0]++
				case reflect.DeepEqual(stats, wantNewChainStats):
					tallies[reader][1]++
				default:
					t.Errorf("reader %d: query mixed chain states:\nvs old chain: %s\nvs new chain: %s",
						reader, diffTimeStats(stats, wantOldChainStats), diffTimeStats(stats, wantNewChainStats))
					return
				}
				observed.Add(1)
			}
		}(r)
	}

	// Reorgs in both directions; each is legal, starts at the same
	// ancestor, and is validated before anything is applied. The loop keeps
	// alternating until the readers have collected enough results to prove
	// queries ran while the chain was being replaced; the cycle cap
	// guarantees termination no matter how goroutines are scheduled.
	cycles := 0
	for ; observed.Load() < 200 && cycles < 10000; cycles++ {
		if _, err := index.Reorg(newBranch); err != nil {
			t.Fatalf("reorg to new branch (cycle %d) refused: %v", cycles, err)
		}
		if _, err := index.Reorg(oldBranch); err != nil {
			t.Fatalf("reorg back to old branch (cycle %d) refused: %v", cycles, err)
		}
	}
	// End on the shortened chain so the final state is deterministic.
	if _, err := index.Reorg(newBranch); err != nil {
		t.Fatalf("final reorg to new branch refused: %v", err)
	}
	close(stop)
	readers.Wait()

	t.Logf("readers collected %d results over %d reorg cycles", observed.Load(), cycles)
	for reader, tally := range tallies {
		t.Logf("reader %d observed %d old-chain and %d new-chain results", reader, tally[0], tally[1])
	}

	// Once the last reorg has returned, a query can only see the new chain.
	final, err := index.QueryTimeStats(reorgStatsQuery)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(final, wantNewChainStats) {
		t.Fatalf("stats after the final reorg violate the new-chain state: %s", diffTimeStats(final, wantNewChainStats))
	}
}
