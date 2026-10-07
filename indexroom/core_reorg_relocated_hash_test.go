package indexroom

import (
	"reflect"
	"testing"
)

// This file guards the reorg rule that a hash living inside the replaced
// suffix may be reused by the alternate branch — including at a DIFFERENT
// height than it used to occupy. TestReorgRemovedSuffixHashesMayReappear
// already covers reuse at the original height; here the reused hashes move,
// so every lookup that resolves a hash must follow its new position:
//
//   - the dropped-height report must come from comparing the block AT each
//     replaced height, not from checking whether a hash still exists, so a
//     height whose old hash reappears elsewhere is still reported;
//   - a later branch rooted at a moved hash must start at the height the
//     hash occupies NOW, not the one it occupied before the first reorg;
//   - the reuse budget is exactly the replaced suffix: a hash at or below
//     the retained ancestor, or the same hash twice inside one branch,
//     rejects the whole reorg with no dropped heights and no state change.
//
// Shared fixture — old main chain, tip 5:
//
//	h1@1 parent g   txs [r1]          } retained range
//	h2@2 parent h1  txs [r2]          } (ancestor for every branch below)
//	h3@3 parent h2  txs [oldA]        } old suffix, replaced by the
//	h4@4 parent h3  txs [oldB1,oldB2] } relocated-hash branch: h4 and h5
//	h5@5 parent h4  txs [oldC]        } swap heights, h3 disappears
//
// Relocated-hash branch rooted at the retained h2:
//
//	n3@3 parent h2  txs [newA]        // fresh hash
//	h5@4 parent n3  txs [newB]        // h5 moved 5 -> 4
//	h4@5 parent h5  txs [newC1,newC2] // h4 moved 4 -> 5
//
// Every height above the ancestor changes content, so the dropped report is
// [3 4 5] even though h4 and h5 remain indexed afterwards.

func relocatedHashOldChain() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"r1"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"r2"}},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"oldA"}},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"oldB1", "oldB2"}},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"oldC"}},
	}
}

func relocatedHashBranch() []Block {
	return []Block{
		{Height: 3, Hash: "n3", Parent: "h2", Txs: []string{"newA"}},
		{Height: 4, Hash: "h5", Parent: "n3", Txs: []string{"newB"}},
		{Height: 5, Hash: "h4", Parent: "h5", Txs: []string{"newC1", "newC2"}},
	}
}

func buildRelocatedHashOldChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	for _, block := range relocatedHashOldChain() {
		if err := index.Append(block); err != nil {
			t.Fatalf("setup append at height %d: %v", block.Height, err)
		}
	}
	return index
}

// relocatedHashHits derives the expected occurrence list from an explicit
// chain, independent of the Index under test.
func relocatedHashHits(chain []Block) []TxHit {
	hits := []TxHit{}
	for _, block := range chain {
		for position, tx := range block.Txs {
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

func relocatedHashNewChain() []Block {
	chain := append([]Block{}, relocatedHashOldChain()[:2]...)
	return append(chain, relocatedHashBranch()...)
}

// The swap itself: both reused hashes land at heights different from their
// old ones, the branch replaces everything above the retained ancestor, and
// the dropped report lists every changed old height exactly once.
func TestReorgRelocatedHashesSwapHeights(t *testing.T) {
	index := buildRelocatedHashOldChain(t)

	dropped, err := index.Reorg(relocatedHashBranch())
	if err != nil {
		t.Fatalf("relocated-hash reorg refused: %v", err)
	}
	// h4 and h5 are still indexed after the reorg, but the blocks AT heights
	// 4 and 5 changed, so both heights must be reported; the retained range
	// (1..2) must not appear.
	if want := []int64{3, 4, 5}; !reflect.DeepEqual(dropped, want) {
		t.Fatalf("dropped=%v, want %v (a hash reappearing must not hide a changed height)", dropped, want)
	}
	if index.Tip != 5 {
		t.Fatalf("tip=%d, want 5", index.Tip)
	}

	// The retained range is untouched, block for block.
	for _, retained := range relocatedHashOldChain()[:2] {
		stored, ok := index.Blocks[retained.Height]
		if !ok || !equalBlockContent(stored, retained) {
			t.Fatalf("retained height %d changed: %+v", retained.Height, stored)
		}
		if index.ByHash[retained.Hash] != retained.Height {
			t.Fatalf("retained hash %q resolved to %d, want %d",
				retained.Hash, index.ByHash[retained.Hash], retained.Height)
		}
	}

	// The moved hashes resolve to their NEW heights; the dropped hash is gone.
	if got := index.ByHash["h5"]; got != 4 {
		t.Fatalf("h5 resolved to height %d, want its new height 4", got)
	}
	if got := index.ByHash["h4"]; got != 5 {
		t.Fatalf("h4 resolved to height %d, want its new height 5", got)
	}
	if _, ok := index.ByHash["h3"]; ok {
		t.Fatal("dropped hash h3 still indexed")
	}
	if block := index.Blocks[4]; block.Hash != "h5" || block.Parent != "n3" {
		t.Fatalf("height 4 holds %+v, want the relocated h5 block", block)
	}
	if block := index.Blocks[5]; block.Hash != "h4" || block.Parent != "h5" {
		t.Fatalf("height 5 holds %+v, want the relocated h4 block", block)
	}

	// Queries see only the new chain: the replacement transactions at their
	// new heights and positions, nothing from the old blocks — oldB1/oldB2
	// (old h4) and oldC (old h5) must not ride along on the reused hashes.
	page, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	wantHits := relocatedHashHits(relocatedHashNewChain())
	if !reflect.DeepEqual(page.Hits, wantHits) {
		t.Fatalf("hits after relocated-hash reorg=%v, want %v", page.Hits, wantHits)
	}
	if page.TotalMatches != int64(len(wantHits)) || page.MatchedBlocks != 5 || page.ToHeight != 5 {
		t.Fatalf("stats=%d/%d/to%d, want %d/5/to5",
			page.TotalMatches, page.MatchedBlocks, page.ToHeight, len(wantHits))
	}
	for _, stale := range []TxHit{
		{Height: 4, BlockHash: "h4", TxID: "oldB1", Position: 0},
		{Height: 4, BlockHash: "h4", TxID: "oldB2", Position: 1},
		{Height: 5, BlockHash: "h5", TxID: "oldC", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "oldA", Position: 0},
	} {
		for _, hit := range page.Hits {
			if hit == stale {
				t.Fatalf("stale occurrence %+v survived the reorg via a reused hash", stale)
			}
		}
		if page, err := index.QueryTxs(TxQuery{TxIDs: []string{stale.TxID}}); err != nil {
			t.Fatal(err)
		} else if len(page.Hits) != 0 || page.TotalMatches != 0 {
			t.Fatalf("removed transaction %q still queryable: %+v", stale.TxID, page)
		}
	}
}

// After the swap, a follow-up branch rooted at a moved hash must be anchored
// at the hash's CURRENT height. h4 now sits at height 5, so a branch whose
// first block has parent h4 must start at height 6; the service must not
// demand height 5 (the old position), must not attach the branch anywhere
// else, and must keep everything at and below the parent.
func TestReorgBranchRootedAtRelocatedHash(t *testing.T) {
	index := buildRelocatedHashOldChain(t)
	if _, err := index.Reorg(relocatedHashBranch()); err != nil {
		t.Fatalf("setup reorg refused: %v", err)
	}

	continuation := []Block{
		{Height: 6, Hash: "m6", Parent: "h4", Txs: []string{"post1"}},
		{Height: 7, Hash: "m7", Parent: "m6", Txs: []string{"post2", "post3"}},
	}
	dropped, err := index.Reorg(continuation)
	if err != nil {
		t.Fatalf("branch rooted at the relocated h4 refused (parent must resolve at its new height 5): %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped=%v, want empty: the branch only extends beyond the tip", dropped)
	}
	if index.Tip != 7 {
		t.Fatalf("tip=%d, want 7", index.Tip)
	}

	// The parent block is still exactly where the first reorg put it, with
	// the first replacement's content, and every block at or below it — the
	// retained range and the whole relocated branch — survived.
	if block := index.Blocks[5]; block.Hash != "h4" ||
		!reflect.DeepEqual(block.Txs, []string{"newC1", "newC2"}) {
		t.Fatalf("relocated parent at height 5 changed: %+v", block)
	}
	if index.ByHash["h4"] != 5 || index.ByHash["h5"] != 4 || index.ByHash["n3"] != 3 {
		t.Fatalf("relocated hashes moved again: %v", index.ByHash)
	}
	for _, kept := range relocatedHashNewChain() {
		stored, ok := index.Blocks[kept.Height]
		if !ok || !equalBlockContent(stored, kept) {
			t.Fatalf("kept height %d lost or altered: %+v", kept.Height, stored)
		}
	}
	if block := index.Blocks[6]; block.Hash != "m6" || block.Parent != "h4" {
		t.Fatalf("height 6 holds %+v, want m6 parented on the relocated h4", block)
	}

	// The follow-up branch's transactions appear after the relocated parent's
	// own, ordered by height and in-block position.
	page, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	fullChain := append(relocatedHashNewChain(), continuation...)
	wantHits := relocatedHashHits(fullChain)
	if !reflect.DeepEqual(page.Hits, wantHits) {
		t.Fatalf("hits after follow-up branch=%v, want %v", page.Hits, wantHits)
	}
	if page.TotalMatches != int64(len(wantHits)) || page.MatchedBlocks != 7 || page.ToHeight != 7 {
		t.Fatalf("stats=%d/%d/to%d, want %d/7/to7",
			page.TotalMatches, page.MatchedBlocks, page.ToHeight, len(wantHits))
	}
}

// The reuse budget is exactly the replaced suffix. A branch that takes a
// hash from the retained range (the ancestor or below), or that uses one
// hash twice, is rejected in full — no dropped heights, and the chain, the
// old blocks, and the query results stay exactly as they were, even when
// the conflict sits in the branch's last block.
func TestReorgRelocatedHashReuseBoundaries(t *testing.T) {
	cases := map[string][]Block{
		// h2 IS the branch ancestor: retained, so off-limits for reuse.
		"ancestor hash reused": {
			{Height: 3, Hash: "n3", Parent: "h2", Txs: []string{"newA"}},
			{Height: 4, Hash: "h2", Parent: "n3", Txs: []string{"newB"}},
		},
		// h1 sits below the ancestor: equally retained.
		"below-ancestor hash reused": {
			{Height: 3, Hash: "n3", Parent: "h2", Txs: []string{"newA"}},
			{Height: 4, Hash: "h1", Parent: "n3", Txs: []string{"newB"}},
		},
		// A suffix hash may move, but only once per branch.
		"same relocated hash twice in branch": {
			{Height: 3, Hash: "h4", Parent: "h2", Txs: []string{"newA"}},
			{Height: 4, Hash: "n4", Parent: "h4", Txs: []string{"newB"}},
			{Height: 5, Hash: "h4", Parent: "n4", Txs: []string{"newC"}},
		},
		// Valid blocks first, retained-range hash in the LAST block: the
		// whole branch must still be rejected and nothing may be applied.
		"retained hash conflict at branch end": {
			{Height: 3, Hash: "n3", Parent: "h2", Txs: []string{"newA"}},
			{Height: 4, Hash: "n4", Parent: "n3", Txs: []string{"newB"}},
			{Height: 5, Hash: "h1", Parent: "n4", Txs: []string{"newC"}},
		},
		// Same, but the late conflict is a duplicate inside the branch.
		"duplicate hash at branch end": {
			{Height: 3, Hash: "n3", Parent: "h2", Txs: []string{"newA"}},
			{Height: 4, Hash: "n4", Parent: "n3", Txs: []string{"newB"}},
			{Height: 5, Hash: "n3", Parent: "n4", Txs: []string{"newC"}},
		},
	}
	oldChain := relocatedHashOldChain()
	wantHits := relocatedHashHits(oldChain)
	for name, branch := range cases {
		t.Run(name, func(t *testing.T) {
			index := buildRelocatedHashOldChain(t)
			blocks, byHash, tip := snapshot(index)

			dropped, err := index.Reorg(branch)
			if err == nil {
				t.Fatal("expected reorg refusal")
			}
			if len(dropped) != 0 {
				t.Fatalf("dropped=%v, want no reported heights on a rejected reorg", dropped)
			}
			requireUnchanged(t, index, blocks, byHash, tip)

			page, err := index.QueryTxs(TxQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(page.Hits, wantHits) ||
				page.TotalMatches != int64(len(wantHits)) || page.ToHeight != 5 {
				t.Fatalf("query results changed after rejected reorg: %+v", page)
			}
		})
	}
}
