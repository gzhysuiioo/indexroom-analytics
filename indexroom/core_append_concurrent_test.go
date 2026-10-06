package indexroom

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

// This file is the regression guard for submitting the SAME pending height
// concurrently. The main chain already holds a valid parent and the next
// height does not exist yet when the submissions start; every contender is
// already in flight inside Append before any of them commits. The rules under
// test:
//
//   - Several copies of one identical block submitted at once ALL succeed
//     (content equality, never pointer identity); the tip advances exactly
//     one height and the stored block carries the original transactions with
//     duplicates and in-block positions intact — submission count must not
//     add occurrences, and in-block duplicates must never be merged.
//   - Two DIFFERENT candidate blocks, each a valid child of the same parent,
//     race for one height. Either content may win; once one is the main-chain
//     block, every call carrying that exact content succeeds and every call
//     carrying the other content returns a non-empty ordinary error. The tip,
//     the stored block, the hash-to-height map, and every query answer must
//     describe the accepted content alone: the two transaction lists are
//     never stitched together, and an errored call never overwrites the
//     stored block.
//   - Content distinctions include the tight cases: same hash and parent with
//     only the transaction order changed, and same everything with only the
//     timestamp presence changed (a missing timestamp is different content
//     from a real zero). A missing transaction list and an empty one stay the
//     SAME content.
//
// Per-call success/failure is correlated with the final stored content — not
// merely with the resulting block count. Public state is read only after all
// goroutines have returned; Append's ordinary rejection errors are asserted
// to stay plain errors, never ErrInvalidArgument.
//
// Determinism: two test-only rendezvous (nil in production) let each round
// park every contender before the mutex, then choose — independently of the
// Go scheduler — which content acquires the lock first; the chosen leader
// holds the lock while the other contenders are released and block on it, so
// the forced serialization is the tightest possible overlap. Both winner
// directions are forced for every conflict shape, and hook-free free-for-all
// rounds are run on top so the guarantee does not depend on the winner.

// concurrentAppendFixture is the shared chain state every race starts from:
// the parent at height 2 exists and is valid, height 3 is missing. The
// parent transactions use their own identifier namespace ("p*"), so any
// candidate occurrence in a query is unambiguously attributable.
func concurrentAppendParentChain(t *testing.T) *Index {
	t.Helper()
	return chain(t,
		Block{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"p1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"p2", "p2"}},
	)
}

// copyBlock returns an independent allocation of every slice and timestamp
// pointer. Equality between submissions must rest on content, so contenders
// never share backing arrays.
func copyBlock(b Block) Block {
	c := b
	if b.Txs != nil {
		// append to nil returns nil for a zero-length source, which would
		// erase the non-nil empty slice distinction; size the copy
		// explicitly so an empty list stays empty-but-present.
		c.Txs = make([]string, len(b.Txs))
		copy(c.Txs, b.Txs)
	}
	if b.Time != nil {
		when := *b.Time
		c.Time = &when
	}
	return c
}

// wireKey distinguishes submissions for gate control, including a nil tx
// list from an allocated empty one. Content-level equality (sameBlock) is
// what the assertions use; the finer key only lets a test name the precise
// submission that must lead.
func wireKey(b Block) string {
	txs := "nil"
	if b.Txs != nil {
		raw, _ := json.Marshal(b.Txs)
		txs = string(raw)
	}
	when := "nil"
	if b.Time != nil {
		when = fmt.Sprintf("t%d", *b.Time)
	}
	return fmt.Sprintf("h=%d|hash=%q|parent=%q|txs=%s|time=%s", b.Height, b.Hash, b.Parent, txs, when)
}

// appendResult pairs one call's input content with its returned error.
type appendResult struct {
	key   string
	block Block
	err   error
}

// appendGate is the deterministic rendezvous for one racing round.
type appendGate struct {
	mu       sync.Mutex
	arrivals map[string]int
	release  map[string]chan struct{}

	entered chan Block
	proceed chan struct{}
	leader  sync.Once
}

func installAppendGate(t *testing.T, wireKeys []string) *appendGate {
	t.Helper()
	g := &appendGate{
		arrivals: map[string]int{},
		release:  map[string]chan struct{}{},
		entered:  make(chan Block, 1),
		proceed:  make(chan struct{}),
	}
	for _, key := range wireKeys {
		g.release[key] = make(chan struct{})
	}
	appendArrivalHook = func(block Block) {
		key := wireKey(block)
		g.mu.Lock()
		g.arrivals[key]++
		g.mu.Unlock()
		<-g.release[key]
	}
	appendEnteredHookLocked = func(block Block) {
		g.leader.Do(func() {
			g.entered <- block
			<-g.proceed
		})
	}
	t.Cleanup(func() {
		appendArrivalHook = nil
		appendEnteredHookLocked = nil
	})
	return g
}

func (g *appendGate) waitArrivals(t *testing.T, want map[string]int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		g.mu.Lock()
		got := g.arrivals
		ok := len(got) == len(want)
		if ok {
			for key, n := range want {
				if got[key] != n {
					ok = false
					break
				}
			}
		}
		g.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("contenders did not all reach the gate: got %v want %v", g.arrivals, want)
}

// forceWinner releases the chosen content group, waits until one of its
// calls actually holds the lock, releases every loser to block on the mutex,
// and only then lets the leader validate and commit.
func (g *appendGate) forceWinner(t *testing.T, winnerKey string, loserKeys []string) {
	t.Helper()
	close(g.release[winnerKey])
	select {
	case leader := <-g.entered:
		if got := wireKey(leader); got != winnerKey {
			t.Fatalf("gate leader=%s, want %s", got, winnerKey)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("leader never reached the locked gate")
	}
	for _, key := range loserKeys {
		close(g.release[key])
	}
	close(g.proceed)
}

// launchAppends runs one independent copy per supplied block concurrently
// and returns after every call has finished.
func launchAppends(index *Index, blocks []Block) []appendResult {
	results := make([]appendResult, len(blocks))
	var wg sync.WaitGroup
	for i, block := range blocks {
		wg.Add(1)
		go func(i int, block Block) {
			defer wg.Done()
			results[i] = appendResult{key: wireKey(block), block: block, err: index.Append(block)}
		}(i, copyBlock(block))
	}
	wg.Wait()
	return results
}

func blockHits(height int64, b Block) []TxHit {
	hits := []TxHit{}
	for position, tx := range b.Txs {
		hits = append(hits, TxHit{Height: height, BlockHash: b.Hash, TxID: tx, Position: position})
	}
	return hits
}

func chainHits(chain []Block) []TxHit {
	var hits []TxHit
	for _, b := range chain {
		hits = append(hits, blockHits(b.Height, b)...)
	}
	return hits
}

// requireOrdinaryRejectError ties a failed call to the contract: a non-empty
// ordinary error, never the query-argument sentinel.
func requireOrdinaryRejectError(t *testing.T, err error, context string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected a non-nil error", context)
	}
	if err.Error() == "" {
		t.Fatalf("%s: rejection error must be non-empty", context)
	}
	if errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("%s: ingest rejection must stay an ordinary error, got ErrInvalidArgument: %v", context, err)
	}
}

// requireWinnerState correlates every visible surface with the accepted
// content after the join: tip, maps, exact stored block, and the complete
// ordered query answer over the whole chain.
func requireWinnerState(t *testing.T, index *Index, winner Block, loserHashes ...string) {
	t.Helper()
	const height = int64(3)
	if index.Tip != height {
		t.Fatalf("tip=%d, want 3", index.Tip)
	}
	if len(index.Blocks) != 3 {
		t.Fatalf("stored %d blocks, want 3 — the height must be filled exactly once", len(index.Blocks))
	}
	stored, ok := index.Blocks[height]
	if !ok {
		t.Fatal("height 3 missing after the race")
	}
	if !sameBlock(stored, winner) {
		t.Fatalf("stored block=%+v, want accepted content %+v", stored, winner)
	}
	// Exact, unmerged transaction list — the loser's transactions must never
	// be spliced in and duplicates must survive in order and multiplicity.
	if !reflect.DeepEqual(stored.Txs, winner.Txs) {
		t.Fatalf("stored txs=%q, want %q", stored.Txs, winner.Txs)
	}
	if stored.Parent != "h2" {
		t.Fatalf("stored parent=%q, want h2", stored.Parent)
	}
	if got := index.ByHash[winner.Hash]; got != height {
		t.Fatalf("ByHash[%q]=%d, want 3", winner.Hash, got)
	}
	for _, hash := range loserHashes {
		if hash == winner.Hash {
			continue
		}
		if _, present := index.ByHash[hash]; present {
			t.Fatalf("rejected hash %q must not be indexed", hash)
		}
	}

	page, err := index.QueryTxs(TxQuery{From: height, To: height, PageSize: MaxPageSize})
	if err != nil {
		t.Fatalf("height-3 query failed: %v", err)
	}
	if want := blockHits(height, winner); !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("height-3 hits=%v, want %v", page.Hits, want)
	}
	if page.TotalMatches != int64(len(winner.Txs)) || page.MatchedBlocks != 1 || page.ToHeight != height {
		t.Fatalf("height-3 statistics=%d/%d/%d, want %d/1/3",
			page.TotalMatches, page.MatchedBlocks, page.ToHeight, len(winner.Txs))
	}

	// The full-chain answer must be the parent occurrences plus exactly the
	// winner's occurrences — no stitched mixture.
	parent := []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"p1"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"p2", "p2"}},
	}
	full, err := index.QueryTxs(TxQuery{From: 1, To: height, PageSize: MaxPageSize})
	if err != nil {
		t.Fatalf("full-chain query failed: %v", err)
	}
	want := chainHits(append(append([]Block{}, parent...), winner))
	if !reflect.DeepEqual(full.Hits, want) {
		t.Fatalf("full-chain hits=%v, want %v", full.Hits, want)
	}
	if full.TotalMatches != int64(len(want)) || full.ToHeight != height {
		t.Fatalf("full-chain statistics=%d/%d, want %d/3", full.TotalMatches, full.ToHeight, len(want))
	}
}

// requireResultsCorrelate checks every individual call against the content
// that actually won: same-content calls succeed, other-content calls return
// an ordinary rejection.
func requireResultsCorrelate(t *testing.T, results []appendResult, winner Block) {
	t.Helper()
	for _, result := range results {
		if sameBlock(result.block, winner) {
			if result.err != nil {
				t.Fatalf("call carrying accepted content %+v failed: %v", result.block, result.err)
			}
			continue
		}
		requireOrdinaryRejectError(t, result.err, fmt.Sprintf("losing call %+v", result.block))
	}
}

// TestConcurrentAppendIdenticalCopiesAllSucceed submits independent copies
// of one block for a missing height at once. Every call succeeds; the tip
// advances exactly one height; the original in-block duplicates and
// positions are all the query ever sees.
func TestConcurrentAppendIdenticalCopiesAllSucceed(t *testing.T) {
	canonical := Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a", "b", "a"}, Time: intptr(50)}
	copies := make([]Block, 6)
	for i := range copies {
		copies[i] = copyBlock(canonical) // fresh slice and fresh *int64 each time
	}
	keys := []string{wireKey(canonical)}
	index := concurrentAppendParentChain(t)
	g := installAppendGate(t, keys)
	done := make(chan []appendResult, 1)
	go func() { done <- launchAppends(index, copies) }()
	g.waitArrivals(t, map[string]int{wireKey(canonical): len(copies)})
	g.forceWinner(t, wireKey(canonical), nil)
	results := <-done

	for _, result := range results {
		if result.err != nil {
			t.Fatalf("identical-content submission failed: %v", result.err)
		}
	}
	requireWinnerState(t, index, canonical)

	// The duplicate identifier "a" appears at its original positions 0 and 2
	// exactly twice — six submissions must not add occurrences or merge the
	// pair into one.
	filtered, err := index.QueryTxs(TxQuery{From: 3, To: 3, TxIDs: []string{"a"}, PageSize: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	wantFiltered := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 2},
	}
	if !reflect.DeepEqual(filtered.Hits, wantFiltered) || filtered.TotalMatches != 2 || filtered.MatchedBlocks != 1 {
		t.Fatalf("duplicate occurrences=%v stats=%d/%d, want %v 2/1",
			filtered.Hits, filtered.TotalMatches, filtered.MatchedBlocks, wantFiltered)
	}
}

// conflictVariant is one shape of "two different valid children": both
// extend h2 at height 3 and would be accepted on an empty slot.
type conflictVariant struct {
	name string
	a    Block
	b    Block
	// bHashDiffers says B's hash is not A's, so its absence from ByHash is an
	// extra observable.
	bHashDiffers bool
}

func conflictVariants() []conflictVariant {
	base := func() Block {
		return Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a", "b", "a"}}
	}
	aTime := base()
	aTime.Time = nil
	bTime := base()
	bTime.Time = intptr(0) // missing timestamp vs real zero: two contents
	return []conflictVariant{
		{
			name:         "different hash and transactions",
			a:            base(),
			b:            Block{Height: 3, Hash: "h3b", Parent: "h2", Txs: []string{"x"}},
			bHashDiffers: true,
		},
		{
			name: "transaction order only",
			a:    base(),
			b:    Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a", "a", "b"}},
		},
		{
			name: "timestamp presence only",
			a:    aTime,
			b:    bTime,
		},
		{
			name: "transaction content only",
			a:    base(),
			b:    Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a", "b", "c"}},
		},
	}
}

// contendersFor builds the simultaneous submissions for a variant: three
// independent copies of each candidate.
func contendersFor(v conflictVariant) ([]Block, string, string) {
	blocks := make([]Block, 0, 6)
	for i := 0; i < 3; i++ {
		blocks = append(blocks, copyBlock(v.a))
	}
	for i := 0; i < 3; i++ {
		blocks = append(blocks, copyBlock(v.b))
	}
	return blocks, wireKey(v.a), wireKey(v.b)
}

// For every conflict shape, force each direction: the post-join state and
// every per-call result must follow whichever content was accepted.
func TestConcurrentAppendConflictForcedWinner(t *testing.T) {
	for _, v := range conflictVariants() {
		t.Run(v.name, func(t *testing.T) {
			t.Run("A accepted first", func(t *testing.T) {
				runForcedRound(t, v, true)
			})
			t.Run("B accepted first", func(t *testing.T) {
				runForcedRound(t, v, false)
			})
		})
	}
}

func runForcedRound(t *testing.T, v conflictVariant, aWins bool) {
	t.Helper()
	blocks, keyA, keyB := contendersFor(v)
	index := concurrentAppendParentChain(t)
	g := installAppendGate(t, []string{keyA, keyB})

	done := make(chan []appendResult, 1)
	go func() { done <- launchAppends(index, blocks) }()
	g.waitArrivals(t, map[string]int{keyA: 3, keyB: 3})
	winner, loser := v.a, v.b
	if aWins {
		g.forceWinner(t, keyA, []string{keyB})
	} else {
		g.forceWinner(t, keyB, []string{keyA})
		winner, loser = v.b, v.a
	}
	results := <-done

	requireResultsCorrelate(t, results, winner)
	var loserHashes []string
	if v.bHashDiffers {
		loserHashes = []string{loser.Hash}
	}
	requireWinnerState(t, index, winner, loserHashes...)

	// An errored call changes nothing: a plain serial replay of the winner
	// afterwards is still a successful no-op, and the loser is still refused.
	blocksBefore, byHashBefore, tipBefore := snapshot(index)
	if err := index.Append(copyBlock(winner)); err != nil {
		t.Fatalf("winner replay after the race failed: %v", err)
	}
	if err := index.Append(copyBlock(loser)); err == nil {
		t.Fatal("losing content overwrote the height on retry")
	}
	requireUnchanged(t, index, blocksBefore, byHashBefore, tipBefore)
}

// TestConcurrentAppendConflictWinnerOwnsTimeAndSnapshot tightens the
// timestamp-presence variant: the accepted content decides the time stats
// answer and the snapshot version and timestamp literals, including after a
// restore roundtrip.
func TestConcurrentAppendConflictWinnerOwnsTimeAndSnapshot(t *testing.T) {
	var v conflictVariant
	for _, candidate := range conflictVariants() {
		if candidate.name == "timestamp presence only" {
			v = candidate
		}
	}
	for _, tc := range []struct {
		name        string
		missingWins bool
	}{
		{name: "missing time wins", missingWins: true},
		{name: "real zero wins", missingWins: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks, keyA, keyB := contendersFor(v)
			index := concurrentAppendParentChain(t)
			g := installAppendGate(t, []string{keyA, keyB})

			done := make(chan []appendResult, 1)
			go func() { done <- launchAppends(index, blocks) }()
			g.waitArrivals(t, map[string]int{keyA: 3, keyB: 3})
			winner := v.a
			if tc.missingWins {
				g.forceWinner(t, keyA, []string{keyB})
			} else {
				g.forceWinner(t, keyB, []string{keyA})
				winner = v.b
			}
			results := <-done
			requireResultsCorrelate(t, results, winner)
			requireWinnerState(t, index, winner)

			stats, err := index.QueryTimeStats(TimeStatsQuery{
				From: 3, To: 3, Start: 0, End: 1000, StepSeconds: 1000,
			})
			if err != nil {
				t.Fatal(err)
			}
			if tc.missingWins {
				if stats.MissingTimeBlocks != 1 || stats.Totals.TxCount != 0 {
					t.Fatalf("missing-time winner stats=%+v, want one missing block and zero totals", stats)
				}
			} else {
				if stats.MissingTimeBlocks != 0 || stats.Totals.TxCount != 3 || stats.Buckets[0].TxCount != 3 {
					t.Fatalf("zero-time winner stats=%+v, want 3 occurrences in bucket 0, none missing", stats)
				}
			}

			var raw bytes.Buffer
			if err := index.Export(&raw); err != nil {
				t.Fatalf("export failed: %v", err)
			}
			var doc map[string]any
			if err := json.Unmarshal(raw.Bytes(), &doc); err != nil {
				t.Fatal(err)
			}
			wantVersion := float64(2)
			if tc.missingWins {
				wantVersion = 1
			}
			if doc["version"] != wantVersion {
				t.Fatalf("snapshot version=%v, want %v", doc["version"], wantVersion)
			}
			exported := doc["blocks"].([]any)
			third := exported[2].(map[string]any)
			if !tc.missingWins {
				if ts, ok := third["timestamp"]; !ok || ts != float64(0) {
					t.Fatalf("winner timestamp literal=%v, want 0", third["timestamp"])
				}
				first := exported[0].(map[string]any)
				if ts, ok := first["timestamp"]; !ok || ts != nil {
					t.Fatalf("parent block timestamp literal=%v, want null", first["timestamp"])
				}
			} else if _, present := third["timestamp"]; present {
				t.Fatal("all-missing chain must keep the version-1 layout without timestamp fields")
			}

			fresh := New()
			if err := fresh.Restore(bytes.NewReader(raw.Bytes())); err != nil {
				t.Fatalf("restore failed: %v", err)
			}
			gotBlocks, gotByHash, gotTip := snapshot(fresh)
			wantBlocks, wantByHash, wantTip := snapshot(index)
			if gotTip != wantTip || !reflect.DeepEqual(gotBlocks, wantBlocks) || !reflect.DeepEqual(gotByHash, wantByHash) {
				t.Fatalf("restored chain differs from export: tip %d vs %d", gotTip, wantTip)
			}
		})
	}
}

// TestConcurrentAppendNilAndEmptyTxsAreSameContent: a missing transaction
// list and an empty one are equal content, so a head-to-head race between
// the two shapes must end with both calls succeeding regardless of which
// commits first.
func TestConcurrentAppendNilAndEmptyTxsAreSameContent(t *testing.T) {
	nilTxs := Block{Height: 3, Hash: "h3", Parent: "h2", Txs: nil, Time: intptr(0)}
	emptyTxs := Block{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{}, Time: intptr(0)}
	cases := []struct {
		name     string
		firstKey string
		loserKey string
	}{
		{name: "missing tx list commits first", firstKey: wireKey(nilTxs), loserKey: wireKey(emptyTxs)},
		{name: "empty tx list commits first", firstKey: wireKey(emptyTxs), loserKey: wireKey(nilTxs)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := concurrentAppendParentChain(t)
			g := installAppendGate(t, []string{wireKey(nilTxs), wireKey(emptyTxs)})
			done := make(chan []appendResult, 1)
			go func() { done <- launchAppends(index, []Block{copyBlock(nilTxs), copyBlock(emptyTxs)}) }()
			g.waitArrivals(t, map[string]int{wireKey(nilTxs): 1, wireKey(emptyTxs): 1})
			g.forceWinner(t, tc.firstKey, []string{tc.loserKey})
			results := <-done

			for _, result := range results {
				if result.err != nil {
					t.Fatalf("nil vs empty tx list must be equal content: %+v err=%v", result.block, result.err)
				}
			}
			if index.Tip != 3 || len(index.Blocks) != 3 {
				t.Fatalf("tip=%d blocks=%d, want 3/3", index.Tip, len(index.Blocks))
			}
			stored := index.Blocks[3]
			if !sameBlock(stored, nilTxs) || !sameBlock(stored, emptyTxs) {
				t.Fatalf("stored block %+v must be equal to both nil and empty tx shapes", stored)
			}
			// storeLocked preserves the exact committed shape.
			if tc.firstKey == wireKey(nilTxs) && stored.Txs != nil {
				t.Fatalf("nil-txs winner should store nil, got %#v", stored.Txs)
			}
			if tc.firstKey == wireKey(emptyTxs) && (stored.Txs == nil || len(stored.Txs) != 0) {
				t.Fatalf("empty-txs winner should store an empty non-nil slice, got %#v", stored.Txs)
			}
			page, err := index.QueryTxs(TxQuery{From: 3, To: 3, PageSize: MaxPageSize})
			if err != nil {
				t.Fatal(err)
			}
			if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 {
				t.Fatalf("empty block query=%+v, want no occurrences", page)
			}
		})
	}
}

// TestConcurrentAppendConflictFreeForAll runs hook-free barrier rounds in
// which no winner is designated: whatever the scheduler does, per-call
// outcomes must correlate exactly with the content that ended up stored.
func TestConcurrentAppendConflictFreeForAll(t *testing.T) {
	for _, v := range conflictVariants() {
		t.Run(v.name, func(t *testing.T) {
			const rounds = 25
			for round := 0; round < rounds; round++ {
				index := concurrentAppendParentChain(t)
				blocks := make([]Block, 0, 10)
				for i := 0; i < 5; i++ {
					blocks = append(blocks, copyBlock(v.a))
				}
				for i := 0; i < 5; i++ {
					blocks = append(blocks, copyBlock(v.b))
				}

				// Plain barrier rendezvous, no leader parking: once released
				// the lock acquisition order is genuinely up to the runtime.
				barrier := make(chan struct{})
				var arrived sync.WaitGroup
				arrived.Add(len(blocks))
				appendArrivalHook = func(Block) {
					arrived.Done()
					<-barrier
				}
				appendEnteredHookLocked = nil
				t.Cleanup(func() { appendArrivalHook = nil })
				results := make([]appendResult, len(blocks))
				var wg sync.WaitGroup
				for i, block := range blocks {
					wg.Add(1)
					go func(i int, block Block) {
						defer wg.Done()
						results[i] = appendResult{key: wireKey(block), block: block, err: index.Append(block)}
					}(i, copyBlock(block))
				}
				arrived.Wait()
				close(barrier)
				wg.Wait()
				appendArrivalHook = nil

				stored, ok := index.Blocks[3]
				if !ok {
					t.Fatalf("round %d: height 3 was never filled", round)
				}
				matchesA := sameBlock(stored, v.a)
				matchesB := sameBlock(stored, v.b)
				if matchesA == matchesB {
					t.Fatalf("round %d: stored block matches neither or both candidates: %+v", round, stored)
				}
				winner := v.a
				if matchesB {
					winner = v.b
				}
				var successes, failures int
				for _, result := range results {
					if sameBlock(result.block, winner) {
						if result.err != nil {
							t.Fatalf("round %d: accepted-content call failed: %v", round, result.err)
						}
						successes++
					} else {
						requireOrdinaryRejectError(t, result.err, fmt.Sprintf("round %d losing call", round))
						failures++
					}
				}
				if successes != 5 || failures != 5 {
					t.Fatalf("round %d: %d successes and %d failures, want 5/5", round, successes, failures)
				}
				var loserHashes []string
				if v.bHashDiffers {
					loserHash := v.b.Hash
					if matchesB {
						loserHash = v.a.Hash
					}
					loserHashes = []string{loserHash}
				}
				requireWinnerState(t, index, winner, loserHashes...)
			}
		})
	}
}
