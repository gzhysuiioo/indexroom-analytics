package indexroom

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
)

// This file is the regression net for concurrent ingestion: many Append calls
// racing to submit the SAME not-yet-indexed height above a valid parent.
//
// The rules under test are the existing Append rules, now exercised under real
// concurrency:
//
//   - Re-submitting a block identical to one already on the main chain is a
//     successful no-op, and equality is decided by CONTENT, so independent
//     copies of the same candidate all win: every call returns nil, the tip
//     advances exactly one height, and no occurrence (or in-block duplicate)
//     is gained from the repeated submissions.
//   - Two genuinely different candidates that both extend the same parent are
//     a race that exactly one content may win. Once the winning content is the
//     main-chain block, every call carrying identical content succeeds and
//     every call carrying the other content returns a NON-nil ordinary error
//     (never ErrInvalidArgument). The tip, the stored block, the hash->height
//     table, transaction queries, and the exported snapshot must ALL describe
//     the single winning content: no interleaved transaction list and no
//     overwrite following an error.
//
// Per-call outcomes are recorded and, after every call has finished, tied back
// to the finally stored content — merely checking the resulting block count
// cannot distinguish "identical replays" from "one candidate overwrote the
// other". Exported state (Tip, Blocks, ByHash) is read only after the
// WaitGroup has drained, matching the exported-fields inspection convention;
// queries run through the public QueryTxs/Export APIs. Everything is offline.

// cloneCandidate returns a fully independent copy of a candidate block: a new
// tx backing array and a fresh timestamp pointer, so two copies share nothing
// mutable and "identical" can only be a judgement about content.
func cloneCandidate(b Block) Block {
	cp := Block{Height: b.Height, Hash: b.Hash, Parent: b.Parent}
	if b.Txs != nil {
		cp.Txs = append([]string(nil), b.Txs...)
	}
	if b.Time != nil {
		t := *b.Time
		cp.Time = &t
	}
	return cp
}

// appendResult is one call's outcome, indexed by submission position.
type appendResult struct {
	block Block
	err   error
}

// raceAppendAtHeight submits every candidate concurrently at the same height
// (the index's tip+1, parent already on chain) and returns per-call results in
// submission order. It never inspects exported state; callers do that only
// after this returns and all goroutines have finished.
func raceAppendAtHeight(t *testing.T, index *Index, candidates []Block) []appendResult {
	t.Helper()
	start := make(chan struct{})
	results := make([]appendResult, len(candidates))
	var wg sync.WaitGroup
	for i, candidate := range candidates {
		wg.Add(1)
		go func(i int, candidate Block) {
			defer wg.Done()
			<-start // release every submission together
			results[i] = appendResult{block: candidate, err: index.Append(candidate)}
		}(i, candidate)
	}
	close(start)
	wg.Wait()
	return results
}

// raceRow ties one submitted candidate to its observed outcome.
type raceRow struct {
	candidate Block
	err       error
}

// groupRaceOutcomes splits per-call results by content equality with winner
// and loser: every call carrying the winning content must have a nil error,
// every call carrying the other content a non-nil one.
func groupRaceOutcomes(results []appendResult, winner, loser Block) (winners, losers []raceRow) {
	for _, r := range results {
		switch {
		case sameBlock(r.block, winner):
			winners = append(winners, raceRow{r.block, r.err})
		case sameBlock(r.block, loser):
			losers = append(losers, raceRow{r.block, r.err})
		default:
			return nil, nil // a candidate that matches neither is a test-setup bug
		}
	}
	return winners, losers
}

// assertPerCallOutcomes ties each individual call's success/failure to the
// content that finally won, instead of only looking at aggregate counts. A
// winner-content call must have succeeded; a loser-content call must have
// failed with a plain error that is not ErrInvalidArgument (an ingestion
// rejection stays an ordinary error, not a query-argument error).
func assertPerCallOutcomes(t *testing.T, results []appendResult, winner, loser Block) {
	t.Helper()
	winners, losers := groupRaceOutcomes(results, winner, loser)
	if winners == nil || losers == nil || len(winners)+len(losers) != len(results) {
		t.Fatalf("test setup: a submitted candidate matched neither variant:\n%+v", results)
	}
	if len(winners) == 0 {
		t.Fatal("no call carried the winning content")
	}
	if len(losers) == 0 {
		t.Fatal("no call carried the losing content")
	}
	for i, row := range winners {
		if row.err != nil {
			t.Fatalf("winner-content call #%d (%+v) failed: %v", i, row.candidate, row.err)
		}
	}
	for i, row := range losers {
		if row.err == nil {
			t.Fatalf("loser-content call #%d (%+v) succeeded; different content at one height must be rejected",
				i, row.candidate)
		}
		if errors.Is(row.err, ErrInvalidArgument) {
			t.Fatalf("loser-content call #%d returned ErrInvalidArgument; ingestion rejections stay ordinary errors: %v",
				i, row.err)
		}
	}
}

// assertStoredWinner verifies the complete post-race chain describes exactly
// the winning content, read only after all calls finished: tip advanced once,
// the stored block equals the winner (including nil-vs-empty txs and
// nil-vs-zero time), the hash table agrees, and nothing above the new height
// exists.
func assertStoredWinner(t *testing.T, index *Index, parentTip int64, winner Block) {
	t.Helper()
	height := winner.Height
	if index.Tip != height {
		t.Fatalf("tip=%d, want exactly the one new height %d", index.Tip, height)
	}
	stored, ok := index.Blocks[height]
	if !ok {
		t.Fatalf("height %d missing after the race", height)
	}
	if !sameBlock(stored, winner) {
		t.Fatalf("stored block at %d = %+v, want winning content %+v", height, stored, winner)
	}
	if parentTip > 0 && stored.Parent != index.Blocks[parentTip].Hash {
		t.Fatalf("winner parent %q does not chain onto retained parent block %q",
			stored.Parent, index.Blocks[parentTip].Hash)
	}
	if got := index.ByHash[stored.Hash]; got != height {
		t.Fatalf("by-hash maps winning hash %q to %d, want %d", stored.Hash, got, height)
	}
	if _, present := index.Blocks[height+1]; present {
		t.Fatal("a block exists above the contended height")
	}
	if int64(len(index.Blocks)) != height {
		t.Fatalf("block count=%d, want %d (tip advanced exactly once)", len(index.Blocks), height)
	}
}

// assertWinnerQuery verifies QueryTxs reflects only the winning block's own
// occurrences: each winner tx appears at its original in-block position, the
// TotalMatches/MatchedBlocks counts ignore how many times the block was
// submitted, and no tx carried only by the loser leaks through. Hits are read
// via the public API after ingestion finished.
func assertWinnerQuery(t *testing.T, index *Index, winner, loser Block) {
	t.Helper()
	height := winner.Height
	page, err := index.QueryTxs(TxQuery{From: height, To: height, PageSize: MaxPageSize})
	if err != nil {
		t.Fatalf("query at contended height failed: %v", err)
	}
	var want []TxHit = []TxHit{}
	for position, tx := range winner.Txs {
		want = append(want, TxHit{Height: height, BlockHash: winner.Hash, TxID: tx, Position: position})
	}
	if !reflect.DeepEqual(page.Hits, want) {
		t.Fatalf("hits at %d =\n%+v\nwant winner occurrences (original order/duplicates/positions):\n%+v",
			height, page.Hits, want)
	}
	// A block with no transactions holds no matches and is not a matched block.
	wantMatchedBlocks := int64(0)
	if len(winner.Txs) > 0 {
		wantMatchedBlocks = 1
	}
	if page.TotalMatches != int64(len(winner.Txs)) || page.MatchedBlocks != wantMatchedBlocks || page.ToHeight != height {
		t.Fatalf("page stats=%d/%d/%d, want %d/%d/%d",
			page.TotalMatches, page.MatchedBlocks, page.ToHeight,
			len(winner.Txs), wantMatchedBlocks, height)
	}
	if page.NextCursor != "" {
		t.Fatalf("single-height page must end without a cursor, got %q", page.NextCursor)
	}
	// Nothing the losing block carried may show up at the height.
	loserOnly := map[string]bool{}
	for _, tx := range loser.Txs {
		loserOnly[tx] = true
	}
	for _, tx := range winner.Txs {
		delete(loserOnly, tx)
	}
	for tx := range loserOnly {
		page, err := index.QueryTxs(TxQuery{From: height, To: height, TxIDs: []string{tx}})
		if err != nil {
			t.Fatalf("query for loser-only tx %q failed: %v", tx, err)
		}
		if len(page.Hits) != 0 || page.TotalMatches != 0 {
			t.Fatalf("loser-only tx %q leaked into the winning height: %+v", tx, page)
		}
	}
	// Querying the whole chain still sees exactly one complete chain whose
	// occurrence count is the pre-race chain plus the winner's own txs.
	all, err := index.QueryTxs(TxQuery{PageSize: MaxPageSize})
	if err != nil {
		t.Fatalf("whole-chain query failed: %v", err)
	}
	if all.ToHeight != height {
		t.Fatalf("whole-chain ToHeight=%d, want %d", all.ToHeight, height)
	}
	parentCount := int64(0)
	for h := int64(1); h < height; h++ {
		parentCount += int64(len(index.Blocks[h].Txs))
	}
	if all.TotalMatches != parentCount+int64(len(winner.Txs)) {
		t.Fatalf("whole-chain TotalMatches=%d, want %d (parent chain %d + winner txs %d)",
			all.TotalMatches, parentCount+int64(len(winner.Txs)), parentCount, len(winner.Txs))
	}
}

// exportBytes returns the canonical snapshot for cross-checking that the
// persisted view agrees with the winning chain as built independently.
func exportBytes(t *testing.T, index *Index) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := index.Export(&buf); err != nil {
		t.Fatalf("export failed: %v", err)
	}
	return buf.Bytes()
}

// assertWinnerSnapshot builds an independent oracle index by replaying the
// final chain serially (pre-race parent blocks plus the winning block) and
// requires its canonical export to be byte-identical to the raced index
// export. The oracle never shares an Append call with the index under test, so
// a shared ingestion bug cannot make the expectation match itself.
func assertWinnerSnapshot(t *testing.T, index *Index, parents []Block, winner Block) {
	t.Helper()
	oracle := New()
	for _, b := range parents {
		if err := oracle.Append(b); err != nil {
			t.Fatalf("oracle setup append at %d: %v", b.Height, err)
		}
	}
	if err := oracle.Append(cloneCandidate(winner)); err != nil {
		t.Fatalf("oracle append of winner at %d failed: %v", winner.Height, err)
	}
	if got, want := exportBytes(t, index), exportBytes(t, oracle); !bytes.Equal(got, want) {
		t.Fatalf("raced export:\n%s\noracle export (parents + winner):\n%s", got, want)
	}
	// Round-tripping through a snapshot must preserve the same content as
	// well, covering the snapshot behaviour alongside ingestion.
	restored := New()
	if err := restored.Restore(bytes.NewReader(exportBytes(t, index))); err != nil {
		t.Fatalf("restore of raced export failed: %v", err)
	}
	if got, want := exportBytes(t, restored), exportBytes(t, oracle); !bytes.Equal(got, want) {
		t.Fatalf("restored export diverges from oracle:\n%s\nwant:\n%s", got, want)
	}
}

// raceFixture is one concurrency scenario: the parent chain already ingested,
// the height under contention, and two candidate contents that both extend the
// tip (so either may legitimately win).
type raceFixture struct {
	name    string
	parents []Block
	winner  Block // arbitrary pinning for post-race classification; either may win
	loser   Block
}

// validateRaceFixture is a setup-time oracle check: the two candidates are at
// the same next height over the same parent hash, each individually accepted
// on a fresh copy of the parent chain, each is rejected once the other is
// installed, and the two genuinely differ in content. A fixture that fails
// these would make the race assertions meaningless.
func validateRaceFixture(t *testing.T, f raceFixture) {
	t.Helper()
	height := int64(len(f.parents)) + 1
	if f.winner.Height != height || f.loser.Height != height {
		t.Fatalf("fixture %q: candidates must target next height %d", f.name, height)
	}
	tipHash := f.parents[len(f.parents)-1].Hash
	if f.winner.Parent != tipHash || f.loser.Parent != tipHash {
		t.Fatalf("fixture %q: both candidates must parent the tip hash %q", f.name, tipHash)
	}
	if f.winner.Hash == "" || f.loser.Hash == "" {
		t.Fatalf("fixture %q: candidate hashes must be non-empty", f.name)
	}
	if sameBlock(f.winner, f.loser) {
		t.Fatalf("fixture %q: the two candidates must be different content", f.name)
	}
	build := func() *Index {
		index := New()
		for _, b := range f.parents {
			if err := index.Append(b); err != nil {
				t.Fatalf("fixture %q setup append at %d: %v", f.name, b.Height, err)
			}
		}
		return index
	}
	first, second := build(), build()
	if err := first.Append(cloneCandidate(f.winner)); err != nil {
		t.Fatalf("fixture %q: winner candidate must be accepted on its own: %v", f.name, err)
	}
	if err := second.Append(cloneCandidate(f.loser)); err != nil {
		t.Fatalf("fixture %q: loser candidate must be accepted on its own: %v", f.name, err)
	}
	if err := first.Append(cloneCandidate(f.loser)); err == nil {
		t.Fatalf("fixture %q: loser content must be rejected after winner is installed", f.name)
	}
	if err := second.Append(cloneCandidate(f.winner)); err == nil {
		t.Fatalf("fixture %q: winner content must be rejected after loser is installed", f.name)
	}
}

// appendRaceFixtures covers the content distinctions called out by the
// ingestion rules: different transaction sets, order-only and timestamp-only
// differences under identical hash/parent, nil-vs-zero timestamp, and the
// nil-vs-empty tx list equivalence (which is therefore NOT a conflict and gets
// its own identical-content test).
func appendRaceFixtures() []raceFixture {
	baseParent := []Block{{Height: 1, Hash: "p1", Parent: "genesis", Txs: []string{"g1"}}}
	parentWithTime := []Block{{Height: 1, Hash: "p1", Parent: "genesis", Txs: []string{"g1"}, Time: intptr(10)}}
	zero := int64(0)
	seven := int64(7)
	return []raceFixture{
		{
			name:    "different tx sets",
			parents: baseParent,
			winner:  Block{Height: 2, Hash: "c2", Parent: "p1", Txs: []string{"a", "b", "a"}},
			loser:   Block{Height: 2, Hash: "d2", Parent: "p1", Txs: []string{"a", "c"}},
		},
		{
			name:    "same hash and parent, tx order differs",
			parents: baseParent,
			winner:  Block{Height: 2, Hash: "same", Parent: "p1", Txs: []string{"a", "b", "a"}},
			loser:   Block{Height: 2, Hash: "same", Parent: "p1", Txs: []string{"a", "a", "b"}},
		},
		{
			name:    "same hash parent and txs, timestamp provided vs missing",
			parents: parentWithTime,
			winner:  Block{Height: 2, Hash: "same", Parent: "p1", Txs: []string{"a"}, Time: &seven},
			loser:   Block{Height: 2, Hash: "same", Parent: "p1", Txs: []string{"a"}},
		},
		{
			name:    "missing timestamp vs real zero are two contents",
			parents: baseParent,
			winner:  Block{Height: 2, Hash: "same", Parent: "p1", Txs: []string{"a"}},
			loser:   Block{Height: 2, Hash: "same", Parent: "p1", Txs: []string{"a"}, Time: &zero},
		},
		{
			name:    "different tx sets with same timestamp",
			parents: baseParent,
			winner:  Block{Height: 2, Hash: "c2", Parent: "p1", Txs: []string{"x"}, Time: &seven},
			loser:   Block{Height: 2, Hash: "d2", Parent: "p1", Txs: []string{"y"}, Time: &seven},
		},
	}
}

// TestConcurrentAppendIdenticalContentAllSucceeds: many independent copies of
// one candidate race at the next height. Every call must succeed, the tip
// advances exactly one height, and the winning block's transactions are stored
// once — in original order, keeping in-block duplicates and positions — with
// no occurrence gained from the repeated submissions.
func TestConcurrentAppendIdenticalContentAllSucceeds(t *testing.T) {
	const submissions = 16
	parents := []Block{{Height: 1, Hash: "p1", Parent: "genesis", Txs: []string{"g1", "g2"}}}

	cases := []struct {
		name  string
		block Block
	}{
		{
			name:  "txs with an in-block duplicate",
			block: Block{Height: 2, Hash: "c2", Parent: "p1", Txs: []string{"a", "b", "a", "a", "b"}},
		},
		{
			name:  "nil tx list",
			block: Block{Height: 2, Hash: "c2", Parent: "p1", Txs: nil},
		},
		{
			name:  "with timestamp",
			block: Block{Height: 2, Hash: "c2", Parent: "p1", Txs: []string{"a", "b", "a"}, Time: intptr(42)},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := New()
			for _, b := range parents {
				if err := index.Append(b); err != nil {
					t.Fatalf("setup append at %d: %v", b.Height, err)
				}
			}

			// Every submission gets its OWN independent backing data, so
			// acceptance cannot be relying on slice/pointer identity.
			candidates := make([]Block, submissions)
			for i := range candidates {
				candidates[i] = cloneCandidate(tc.block)
			}
			results := raceAppendAtHeight(t, index, candidates)
			for i, r := range results {
				if r.err != nil {
					t.Fatalf("identical-content call #%d failed: %v", i, r.err)
				}
				if !sameBlock(r.block, tc.block) {
					t.Fatalf("call #%d submitted mutated content %+v", i, r.block)
				}
			}

			assertStoredWinner(t, index, 1, tc.block)
			assertWinnerQuery(t, index, tc.block, Block{})
			assertWinnerSnapshot(t, index, parents, tc.block)

			// Explicit duplicate accounting for the in-block duplicate rule:
			// "a" occurs twice in the block (positions 0 and 2 for the first
			// fixture) regardless of the 16 identical submissions, and its
			// hits keep those exact positions.
			if len(tc.block.Txs) > 0 {
				counts := map[string]int{}
				positions := map[string][]int{}
				for pos, tx := range tc.block.Txs {
					counts[tx]++
					positions[tx] = append(positions[tx], pos)
				}
				for tx, wantCount := range counts {
					page, err := index.QueryTxs(TxQuery{From: 2, To: 2, TxIDs: []string{tx}})
					if err != nil {
						t.Fatalf("query %q failed: %v", tx, err)
					}
					if page.TotalMatches != int64(wantCount) || int64(len(page.Hits)) != int64(wantCount) {
						t.Fatalf("tx %q occurrences=%d (matches=%d), want block-internal count %d after %d submissions",
							tx, len(page.Hits), page.TotalMatches, wantCount, submissions)
					}
					var gotPositions []int
					for _, hit := range page.Hits {
						gotPositions = append(gotPositions, hit.Position)
					}
					if !reflect.DeepEqual(gotPositions, positions[tx]) {
						t.Fatalf("tx %q positions=%v, want original in-block positions %v",
							tx, gotPositions, positions[tx])
					}
				}
			}
		})
	}
}

// TestConcurrentAppendIdenticalNilAndEmptyTxsBothWin: a missing tx list and an
// explicit empty list are the SAME content, so a race mixing both forms must
// accept every call and store exactly one empty block.
func TestConcurrentAppendIdenticalNilAndEmptyTxsBothWin(t *testing.T) {
	const each = 8
	parents := []Block{{Height: 1, Hash: "p1", Parent: "genesis", Txs: []string{"g1"}}}
	index := New()
	if err := index.Append(parents[0]); err != nil {
		t.Fatal(err)
	}
	winner := Block{Height: 2, Hash: "c2", Parent: "p1"} // nil txs, no time
	candidates := make([]Block, 0, 2*each)
	for i := 0; i < each; i++ {
		b := cloneCandidate(winner)
		candidates = append(candidates, b)
		empty := cloneCandidate(winner)
		empty.Txs = []string{}
		candidates = append(candidates, empty)
	}
	results := raceAppendAtHeight(t, index, candidates)
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("equivalent nil/empty-tx call #%d failed: %v", i, r.err)
		}
	}
	assertStoredWinner(t, index, 1, winner)
	page, err := index.QueryTxs(TxQuery{From: 2, To: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 0 || page.TotalMatches != 0 || page.MatchedBlocks != 0 {
		t.Fatalf("empty winning block must yield no occurrences: %+v", page)
	}
	assertWinnerSnapshot(t, index, parents, winner)
}

// TestConcurrentAppendConflictingCandidatesOneContentWins: for every content
// distinction in the fixtures, two candidate contents race with multiple
// independent copies each. Either content may be accepted first — the test
// never designates a winner — but once one is the main-chain block, per-call
// success/failure must line up with the finally stored content, and the whole
// observable state must describe that one content.
func TestConcurrentAppendConflictingCandidatesOneContentWins(t *testing.T) {
	for _, fixture := range appendRaceFixtures() {
		t.Run(fixture.name, func(t *testing.T) {
			validateRaceFixture(t, fixture)

			// Run several rounds; because either content may win and the test
			// derives expectations from the actual winner, every round is a
			// valid outcome and the assertions are independent of scheduling.
			const rounds = 8
			const copiesEach = 8
			for round := 0; round < rounds; round++ {
				t.Run(fmt.Sprintf("round%d", round), func(t *testing.T) {
					index := New()
					for _, b := range fixture.parents {
						if err := index.Append(b); err != nil {
							t.Fatalf("setup append at %d: %v", b.Height, err)
						}
					}
					candidates := make([]Block, 0, 2*copiesEach)
					for i := 0; i < copiesEach; i++ {
						candidates = append(candidates, cloneCandidate(fixture.winner))
						candidates = append(candidates, cloneCandidate(fixture.loser))
					}
					results := raceAppendAtHeight(t, index, candidates)

					// Determine the winner from the finally stored content,
					// not from whichever call happened to run first. State is
					// inspected only after every call has returned.
					height := fixture.winner.Height
					stored := index.Blocks[height]
					var winner, loser Block
					switch {
					case sameBlock(stored, fixture.winner):
						winner, loser = fixture.winner, fixture.loser
					case sameBlock(stored, fixture.loser):
						winner, loser = fixture.loser, fixture.winner
					default:
						t.Fatalf("stored block at %d matches neither candidate: %+v", height, stored)
					}

					assertPerCallOutcomes(t, results, winner, loser)
					assertStoredWinner(t, index, int64(len(fixture.parents)), winner)
					assertWinnerQuery(t, index, winner, loser)
					assertWinnerSnapshot(t, index, fixture.parents, winner)
				})
			}
		})
	}
}

// TestConcurrentAppendSameHeightRaceLeavesNoOrphanHashes: the loser's hash
// must never enter the hash index, and the losing txs must not be queryable —
// an error return can never have partially installed its block.
func TestConcurrentAppendSameHeightRaceLeavesNoOrphanHashes(t *testing.T) {
	fixture := raceFixture{
		name:    "orphan check",
		parents: []Block{{Height: 1, Hash: "p1", Parent: "genesis", Txs: []string{"g1"}}},
		winner:  Block{Height: 2, Hash: "win2", Parent: "p1", Txs: []string{"keep"}},
		loser:   Block{Height: 2, Hash: "lose2", Parent: "p1", Txs: []string{"drop", "drop"}},
	}
	validateRaceFixture(t, fixture)

	for round := 0; round < 6; round++ {
		t.Run(fmt.Sprintf("round%d", round), func(t *testing.T) {
			index := New()
			if err := index.Append(fixture.parents[0]); err != nil {
				t.Fatal(err)
			}
			candidates := []Block{
				cloneCandidate(fixture.winner), cloneCandidate(fixture.winner),
				cloneCandidate(fixture.loser), cloneCandidate(fixture.loser),
			}
			results := raceAppendAtHeight(t, index, candidates)

			var winner, loser Block
			if index.ByHash["win2"] == 2 {
				winner, loser = fixture.winner, fixture.loser
			} else {
				winner, loser = fixture.loser, fixture.winner
			}
			assertPerCallOutcomes(t, results, winner, loser)

			losingHash := loser.Hash
			if _, present := index.ByHash[losingHash]; present {
				t.Fatalf("losing hash %q entered the hash index", losingHash)
			}
			for _, tx := range loser.Txs {
				page, err := index.QueryTxs(TxQuery{From: 2, To: 2, TxIDs: []string{tx}})
				if err != nil {
					t.Fatalf("query losing tx %q failed: %v", tx, err)
				}
				if len(page.Hits) != 0 {
					t.Fatalf("losing tx %q was indexed despite rejection: %+v", tx, page.Hits)
				}
			}
			// Exactly the parent hashes plus the single winning hash exist.
			wantHashes := map[string]int64{"p1": 1, winner.Hash: 2}
			if !reflect.DeepEqual(map[string]int64(index.ByHash), wantHashes) {
				t.Fatalf("ByHash=%v, want %v", index.ByHash, wantHashes)
			}
		})
	}
}

// TestConcurrentAppendAtHeightOneCoversFirstBlockRule: the same identical-vs-
// conflicting guarantee applies when the contended height is 1 on an empty
// index (the parent is just the chain-start identifier and need not exist).
func TestConcurrentAppendAtHeightOneCoversFirstBlockRule(t *testing.T) {
	t.Run("identical first blocks all succeed", func(t *testing.T) {
		block := Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "a"}}
		candidates := make([]Block, 12)
		for i := range candidates {
			candidates[i] = cloneCandidate(block)
		}
		index := New()
		results := raceAppendAtHeight(t, index, candidates)
		for i, r := range results {
			if r.err != nil {
				t.Fatalf("identical height-1 call #%d failed: %v", i, r.err)
			}
		}
		assertStoredWinner(t, index, 0, block)
		// Parent tip 0 has no block; check the snapshot against the winner
		// directly rather than via the parent-chain oracle helper.
		oracle := New()
		if err := oracle.Append(cloneCandidate(block)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(exportBytes(t, index), exportBytes(t, oracle)) {
			t.Fatal("height-1 raced export differs from a serial one")
		}
	})

	t.Run("conflicting first blocks keep one content", func(t *testing.T) {
		a := Block{Height: 1, Hash: "same", Parent: "genesis", Txs: []string{"a"}}
		b := Block{Height: 1, Hash: "same", Parent: "genesis", Txs: []string{"b"}}
		for round := 0; round < 6; round++ {
			t.Run(fmt.Sprintf("round%d", round), func(t *testing.T) {
				index := New()
				candidates := []Block{
					cloneCandidate(a), cloneCandidate(a), cloneCandidate(a),
					cloneCandidate(b), cloneCandidate(b), cloneCandidate(b),
				}
				results := raceAppendAtHeight(t, index, candidates)
				stored := index.Blocks[1]
				winner, loser := a, b
				if sameBlock(stored, b) {
					winner, loser = b, a
				} else if !sameBlock(stored, a) {
					t.Fatalf("stored height-1 block matches neither candidate: %+v", stored)
				}
				assertPerCallOutcomes(t, results, winner, loser)
				if index.Tip != 1 || index.ByHash[winner.Hash] != 1 {
					t.Fatalf("unexpected state: tip=%d byHash=%v", index.Tip, index.ByHash)
				}
				page, err := index.QueryTxs(TxQuery{From: 1, To: 1, TxIDs: []string{loser.Txs[0]}})
				if err != nil {
					t.Fatal(err)
				}
				if len(page.Hits) != 0 {
					t.Fatalf("losing height-1 tx %q was indexed", loser.Txs[0])
				}
			})
		}
	})
}
