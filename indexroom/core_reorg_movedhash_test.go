package indexroom

import (
	"reflect"
	"strings"
	"testing"
)

// This file is the regression guard for reusing OLD BRANCH hashes in the
// replacing branch when a reused hash actually moves to a DIFFERENT height.
// The pre-existing case (TestReorgRemovedSuffixHashesMayReappear) only reuses
// a removed hash at its old height; here two removed hashes swap heights, the
// reorg must relocate them, and a later branch must resolve a reused parent at
// its new height. Reuse stays bounded by the replaced suffix: taking a hash of
// the ancestor or below, or repeating a hash inside one branch, rejects the
// whole branch.
//
// Initial main chain (ancestor h2 is retained, tip 5):
//
//	h1 @1 parent genesis txs [gen-1]
//	h2 @2 parent h1      txs [gen-2]
//	h3 @3 parent h2      txs [old-a old-b]
//	h4 @4 parent h3      txs [old-c]
//	h5 @5 parent h4      txs [old-d]
//
// First alternate branch replaces everything above h2 (heights 3..5); the old
// hashes h3 and h4 swap heights, blocks stay consecutive, parent links follow
// the new order, and every transaction is distinguishable from the old block:
//
//	h4 @3 parent h2 txs [new3-a]        // old h4 moves down from height 4
//	h3 @4 parent h4 txs [new4-a new4-b] // old h3 moves up from height 3
//	n5 @5 parent h3 txs [new5-a]
//
// Second reorg extends from the relocated h4 — it now sits at height 3, so the
// branch starts at the height after its NEW position:
//
//	m4 @4 parent h4 txs [m4-a]
//	m5 @5 parent m4 txs [m5-a]

func reorgMovedOldChain() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"gen-1"}},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"gen-2"}},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"old-a", "old-b"}},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"old-c"}},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"old-d"}},
	}
}

// reorgMovedSwapBranch is the first alternate branch: the removed hashes h4
// and h3 reappear at each other's old height, chained in the new order.
func reorgMovedSwapBranch() []Block {
	return []Block{
		{Height: 3, Hash: "h4", Parent: "h2", Txs: []string{"new3-a"}},
		{Height: 4, Hash: "h3", Parent: "h4", Txs: []string{"new4-a", "new4-b"}},
		{Height: 5, Hash: "n5", Parent: "h3", Txs: []string{"new5-a"}},
	}
}

// reorgMovedExtendBranch roots at h4's NEW height (3), so it starts at 4.
func reorgMovedExtendBranch() []Block {
	return []Block{
		{Height: 4, Hash: "m4", Parent: "h4", Txs: []string{"m4-a"}},
		{Height: 5, Hash: "m5", Parent: "m4", Txs: []string{"m5-a"}},
	}
}

func buildReorgMovedChain(t *testing.T) *Index {
	t.Helper()
	return chain(t, reorgMovedOldChain()...)
}

func joinBlocks(prefix, suffix []Block) []Block {
	out := append([]Block{}, prefix...)
	return append(out, suffix...)
}

// Two removed-suffix hashes swap heights in the replacing branch. The reorg
// must succeed, report every old height above the ancestor that actually
// changed (a reappearing hash hides nothing), relocate the hashes, keep the
// ancestor and below untouched, and answer transaction queries from the new
// main chain only.
func TestReorgReusedHashesSwapHeightsAndRelocate(t *testing.T) {
	index := buildReorgMovedChain(t)

	dropped, err := index.Reorg(reorgMovedSwapBranch())
	if err != nil {
		t.Fatalf("swap-heights reorg refused: %v", err)
	}
	// h3 and h4 both reappear as hashes, but at swapped heights with swapped
	// parents and new transactions: every old height 3..5 genuinely changed,
	// so all three must be reported in ascending order.
	if !reflect.DeepEqual(dropped, []int64{3, 4, 5}) {
		t.Fatalf("dropped=%v, want [3 4 5]", dropped)
	}
	if index.Tip != 5 {
		t.Fatalf("tip=%d, want 5", index.Tip)
	}

	// The ancestor h2 and the block below it are retained byte for byte.
	for height, want := range map[int64]Block{
		1: {Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"gen-1"}},
		2: {Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"gen-2"}},
	} {
		if !sameBlock(index.Blocks[height], want) {
			t.Fatalf("retained height %d changed: got %+v want %+v", height, index.Blocks[height], want)
		}
	}

	// The relocated blocks sit at consecutive heights, carrying replacement
	// content, with parent links connected in the new order.
	wantAtHeight := map[int64]Block{
		3: {Height: 3, Hash: "h4", Parent: "h2", Txs: []string{"new3-a"}},
		4: {Height: 4, Hash: "h3", Parent: "h4", Txs: []string{"new4-a", "new4-b"}},
		5: {Height: 5, Hash: "n5", Parent: "h3", Txs: []string{"new5-a"}},
	}
	for height, want := range wantAtHeight {
		if !sameBlock(index.Blocks[height], want) {
			t.Fatalf("height %d: got %+v, want %+v", height, index.Blocks[height], want)
		}
	}
	for height := int64(2); height <= 5; height++ {
		if index.Blocks[height].Parent != index.Blocks[height-1].Hash {
			t.Fatalf("parent link at height %d does not follow the new block order: %q -> want parent %q",
				height, index.Blocks[height].Parent, index.Blocks[height-1].Hash)
		}
	}

	// ByHash relocates both moved hashes; the fully removed h5 is gone.
	wantByHash := map[string]int64{"h1": 1, "h2": 2, "h4": 3, "h3": 4, "n5": 5}
	if !reflect.DeepEqual(index.ByHash, wantByHash) {
		t.Fatalf("ByHash=%v, want %v", index.ByHash, wantByHash)
	}

	// Transaction query answers from the current main chain only, ordered by
	// height then in-block position.
	wantChain := joinBlocks(reorgMovedOldChain()[:2], reorgMovedSwapBranch())
	page, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	wantHits := referenceTxHits(wantChain)
	if !reflect.DeepEqual(page.Hits, wantHits) {
		t.Fatalf("hits=\n %v\nwant\n %v", page.Hits, wantHits)
	}
	requireOrdered(t, page.Hits)
	if page.TotalMatches != 6 || page.MatchedBlocks != 5 || page.ToHeight != 5 {
		t.Fatalf("page stats total=%d blocks=%d to=%d, want 6/5/5",
			page.TotalMatches, page.MatchedBlocks, page.ToHeight)
	}
	for _, hit := range page.Hits {
		switch hit.TxID {
		case "old-a", "old-b", "old-c", "old-d":
			t.Fatalf("removed transaction %q still returned under a reused hash: %+v", hit.TxID, hit)
		}
	}
	// The moved hashes expose their replacement content at the new heights and
	// in-block positions.
	for _, want := range []TxHit{
		{Height: 3, BlockHash: "h4", TxID: "new3-a", Position: 0},
		{Height: 4, BlockHash: "h3", TxID: "new4-a", Position: 0},
		{Height: 4, BlockHash: "h3", TxID: "new4-b", Position: 1},
		{Height: 5, BlockHash: "n5", TxID: "new5-a", Position: 0},
	} {
		if !containsHit(page.Hits, want) {
			t.Fatalf("replacement hit %+v missing from %v", want, page.Hits)
		}
	}
}

// After the swap reorg, a further branch uses a moved hash (h4, now at height
// 3) as the first block's parent, starting at the next height. The service must
// resolve the parent at its new position: accept the branch, keep h4 and below,
// leave h4 at the post-swap height with its post-swap content, and place the
// extension's transactions after it in height/position order.
func TestReorgBranchExtendsFromHashMovedToNewHeight(t *testing.T) {
	index := buildReorgMovedChain(t)
	if _, err := index.Reorg(reorgMovedSwapBranch()); err != nil {
		t.Fatalf("setup swap reorg refused: %v", err)
	}
	if got := index.ByHash["h4"]; got != 3 {
		t.Fatalf("setup: h4 resolves at height %d, want its new height 3", got)
	}

	dropped, err := index.Reorg(reorgMovedExtendBranch())
	if err != nil {
		// A stale-height lookup would resolve h4 at old height 4 and reject
		// the branch head for not starting at 5.
		t.Fatalf("extension rooted at relocated h4 refused (parent resolved at its stale height?): %v", err)
	}
	// Only the replaced heights 4..5 changed; the retained range through h4 at
	// height 3 must never be reported.
	if !reflect.DeepEqual(dropped, []int64{4, 5}) {
		t.Fatalf("dropped=%v, want [4 5]", dropped)
	}
	if index.Tip != 5 {
		t.Fatalf("tip=%d, want 5", index.Tip)
	}

	wantByHash := map[string]int64{"h1": 1, "h2": 2, "h4": 3, "m4": 4, "m5": 5}
	if !reflect.DeepEqual(index.ByHash, wantByHash) {
		t.Fatalf("ByHash=%v, want %v", index.ByHash, wantByHash)
	}
	// The moved parent keeps the height and content it got from the first
	// replacement; the swapped h3 (height 4) and n5 (height 5) are gone.
	if got := index.Blocks[3]; !sameBlock(got, Block{Height: 3, Hash: "h4", Parent: "h2", Txs: []string{"new3-a"}}) {
		t.Fatalf("relocated parent block altered by the extension: got %+v", got)
	}
	old := reorgMovedOldChain()
	if !sameBlock(index.Blocks[1], old[0]) || !sameBlock(index.Blocks[2], old[1]) {
		t.Fatal("ancestor h2 and below must be retained across the extension")
	}
	for _, gone := range []string{"h3", "n5"} {
		if _, ok := index.ByHash[gone]; ok {
			t.Fatalf("replaced hash %q still indexed after extension", gone)
		}
	}

	wantChain := []Block{
		old[0], old[1],
		{Height: 3, Hash: "h4", Parent: "h2", Txs: []string{"new3-a"}},
		{Height: 4, Hash: "m4", Parent: "h4", Txs: []string{"m4-a"}},
		{Height: 5, Hash: "m5", Parent: "m4", Txs: []string{"m5-a"}},
	}
	page, err := index.QueryTxs(TxQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(page.Hits, referenceTxHits(wantChain)) {
		t.Fatalf("hits=\n %v\nwant\n %v", page.Hits, referenceTxHits(wantChain))
	}
	requireOrdered(t, page.Hits)
	if page.TotalMatches != 5 || page.MatchedBlocks != 5 || page.ToHeight != 5 {
		t.Fatalf("page stats total=%d blocks=%d to=%d, want 5/5/5",
			page.TotalMatches, page.MatchedBlocks, page.ToHeight)
	}
	for _, hit := range page.Hits {
		switch hit.TxID {
		case "old-a", "old-b", "old-c", "old-d", "new4-a", "new4-b", "new5-a":
			// Old-suffix txs must not survive on reused hashes, and txs of the
			// blocks the extension replaced must vanish with them.
			t.Fatalf("removed transaction %q leaked into the current chain: %+v", hit.TxID, hit)
		}
	}
	// The moved parent's transaction still answers at height 3 with the value
	// set by the first replacement; the extension follows it.
	for _, want := range []TxHit{
		{Height: 3, BlockHash: "h4", TxID: "new3-a", Position: 0},
		{Height: 4, BlockHash: "m4", TxID: "m4-a", Position: 0},
		{Height: 5, BlockHash: "m5", TxID: "m5-a", Position: 0},
	} {
		if !containsHit(page.Hits, want) {
			t.Fatalf("expected hit %+v missing from %v", want, page.Hits)
		}
	}
}

// Hash reuse is bounded by the replaced suffix. A branch that takes a hash of
// the ancestor or of a block below it, or that repeats one of its own hashes,
// must be rejected in full — no dropped-height report and no observable change,
// even when the conflict is in the last block.
func TestReorgHashReuseBoundaryRejectsWholeBranch(t *testing.T) {
	cases := map[string]struct {
		branch    []Block
		wantError string
	}{
		// Every branch below is otherwise legal (consecutive heights, parent
		// links matching the previous block); the single defect sits in the
		// last block to prove full-branch validation applies end to end.
		"branch reuses the ancestor hash in its last block": {
			branch: []Block{
				{Height: 3, Hash: "a3", Parent: "h2", Txs: []string{"x"}},
				{Height: 4, Hash: "a4", Parent: "a3", Txs: []string{"x"}},
				{Height: 5, Hash: "h2", Parent: "a4", Txs: []string{"x"}},
			},
			wantError: "hash conflicts with a retained block",
		},
		"branch reuses a below-ancestor hash in its last block": {
			branch: []Block{
				{Height: 3, Hash: "b3", Parent: "h2", Txs: []string{"x"}},
				{Height: 4, Hash: "b4", Parent: "b3", Txs: []string{"x"}},
				{Height: 5, Hash: "h1", Parent: "b4", Txs: []string{"x"}},
			},
			wantError: "hash conflicts with a retained block",
		},
		"duplicate hash inside the branch, repeated in its last block": {
			branch: []Block{
				{Height: 3, Hash: "r3", Parent: "h2", Txs: []string{"x"}},
				{Height: 4, Hash: "r4", Parent: "r3", Txs: []string{"x"}},
				{Height: 5, Hash: "r3", Parent: "r4", Txs: []string{"x"}},
			},
			wantError: "duplicate hash inside branch",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			index := buildReorgMovedChain(t)
			blocks, byHash, tip := snapshot(index)
			before, err := index.QueryTxs(TxQuery{})
			if err != nil {
				t.Fatal(err)
			}

			dropped, err := index.Reorg(tc.branch)
			if err == nil {
				t.Fatal("reorg crossing the hash-reuse boundary must be rejected")
			}
			if !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantError)
			}
			if len(dropped) != 0 {
				t.Fatalf("dropped=%v, a rejected reorg reports no heights", dropped)
			}
			// Full validation precedes any mutation: a conflict in the final
			// block leaves tip, blocks, hash index, and transaction answers
			// exactly as they were.
			requireUnchanged(t, index, blocks, byHash, tip)
			after, err := index.QueryTxs(TxQuery{})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("transaction answer changed after a rejected reorg:\n before=%+v\n after =%+v", before, after)
			}
		})
	}
}
