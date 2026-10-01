package indexroom

import (
	"fmt"
	"reflect"
	"sync"
	"testing"
)

func chain(t *testing.T, blocks ...Block) *Index {
	t.Helper()
	index := New()
	for _, block := range blocks {
		if err := index.Append(block); err != nil {
			t.Fatalf("setup append at height %d: %v", block.Height, err)
		}
	}
	return index
}

func snapshot(index *Index) (map[int64]Block, map[string]int64, int64) {
	blocks := make(map[int64]Block, len(index.Blocks))
	for height, block := range index.Blocks {
		blocks[height] = block
	}
	byHash := make(map[string]int64, len(index.ByHash))
	for hash, height := range index.ByHash {
		byHash[hash] = height
	}
	return blocks, byHash, index.Tip
}

func requireUnchanged(t *testing.T, index *Index, blocks map[int64]Block, byHash map[string]int64, tip int64) {
	t.Helper()
	if !reflect.DeepEqual(index.Blocks, blocks) {
		t.Fatalf("blocks changed after rejected call: got %v want %v", index.Blocks, blocks)
	}
	if !reflect.DeepEqual(index.ByHash, byHash) {
		t.Fatalf("by-hash changed after rejected call: got %v want %v", index.ByHash, byHash)
	}
	if index.Tip != tip {
		t.Fatalf("tip changed after rejected call: got %d want %d", index.Tip, tip)
	}
}

func TestAppendFirstBlockMustBeHeightOne(t *testing.T) {
	index := New()
	if err := index.Append(Block{Height: 2, Hash: "h2", Parent: "h1"}); err == nil {
		t.Fatal("expected refusal of height jump on empty index")
	}
	if err := index.Append(Block{Height: 1, Hash: "h1", Parent: "genesis"}); err != nil {
		t.Fatalf("first block refused: %v", err)
	}
	if index.Tip != 1 || index.ByHash["h1"] != 1 {
		t.Fatalf("unexpected state: tip=%d byHash=%v", index.Tip, index.ByHash)
	}
}

func TestAppendRequiresContiguousTip(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis"},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
	)
	blocks, byHash, tip := snapshot(index)
	for name, block := range map[string]Block{
		"height jump":      {Height: 4, Hash: "h4", Parent: "h2"},
		"wrong parent":     {Height: 3, Hash: "h3", Parent: "h1"},
		"empty hash":       {Height: 3, Hash: "", Parent: "h2"},
		"duplicate hash":   {Height: 3, Hash: "h1", Parent: "h2"},
		"backwards height": {Height: 1, Hash: "h1b", Parent: "genesis"},
	} {
		if err := index.Append(block); err == nil {
			t.Fatalf("expected refusal of %s", name)
		}
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

func TestAppendIdenticalReplayIsNoOp(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
	)
	// Empty tx list and missing tx list are the same block.
	if err := index.Append(Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{}}); err != nil {
		t.Fatalf("identical replay refused: %v", err)
	}
	if err := index.Append(Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"t1"}}); err != nil {
		t.Fatalf("identical replay of older block refused: %v", err)
	}
	if index.Tip != 2 {
		t.Fatalf("tip moved on replay: %d", index.Tip)
	}
	// Same height or hash but different content must fail without overwrite.
	for _, block := range []Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t9"}},
		{Height: 2, Hash: "h2x", Parent: "h1"},
		{Height: 1, Hash: "h1", Parent: "other"},
	} {
		blocks, byHash, tip := snapshot(index)
		if err := index.Append(block); err == nil {
			t.Fatalf("expected refusal of conflicting block %+v", block)
		}
		requireUnchanged(t, index, blocks, byHash, tip)
	}
	if got := index.Blocks[2]; got.Hash != "h2" || got.Txs != nil {
		t.Fatalf("stored block overwritten: %+v", got)
	}
}

func TestAppendStoresCopyOfTxs(t *testing.T) {
	index := New()
	txs := []string{"t1", "t2"}
	if err := index.Append(Block{Height: 1, Hash: "h1", Parent: "genesis", Txs: txs}); err != nil {
		t.Fatal(err)
	}
	txs[0] = "mutated"
	if got := index.Blocks[1].Txs; !reflect.DeepEqual(got, []string{"t1", "t2"}) {
		t.Fatalf("stored txs changed by caller mutation: %v", got)
	}
}

func TestReorgReplacesSuffixAndReportsDropped(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis"},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
		Block{Height: 3, Hash: "h3", Parent: "h2"},
	)
	dropped, err := index.Reorg([]Block{
		{Height: 2, Hash: "h2b", Parent: "h1"},
		{Height: 3, Hash: "h3b", Parent: "h2b"},
		{Height: 4, Hash: "h4b", Parent: "h3b"},
	})
	if err != nil {
		t.Fatalf("reorg refused: %v", err)
	}
	if !reflect.DeepEqual(dropped, []int64{2, 3}) {
		t.Fatalf("dropped=%v, want [2 3]", dropped)
	}
	if index.Tip != 4 {
		t.Fatalf("tip=%d, want 4", index.Tip)
	}
	for _, hash := range []string{"h2", "h3"} {
		if _, ok := index.ByHash[hash]; ok {
			t.Fatalf("removed hash %s still indexed", hash)
		}
	}
	if block, ok := index.Blocks[2]; !ok || block.Hash != "h2b" {
		t.Fatalf("height 2 not replaced: %+v", index.Blocks[2])
	}
}

func TestReorgShorterBranchTruncates(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis"},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
		Block{Height: 3, Hash: "h3", Parent: "h2"},
	)
	dropped, err := index.Reorg([]Block{{Height: 3, Hash: "h3b", Parent: "h2"}})
	if err != nil {
		t.Fatalf("reorg refused: %v", err)
	}
	if !reflect.DeepEqual(dropped, []int64{3}) {
		t.Fatalf("dropped=%v, want [3]", dropped)
	}
	if index.Tip != 3 || index.Blocks[3].Hash != "h3b" {
		t.Fatalf("unexpected tip state: tip=%d block=%+v", index.Tip, index.Blocks[3])
	}
	if _, ok := index.ByHash["h3"]; ok {
		t.Fatal("old hash h3 still indexed")
	}
}

func TestReorgIdenticalBlocksNotReported(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis"},
		Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t1"}},
		Block{Height: 3, Hash: "h3", Parent: "h2"},
	)
	// Height 2 is identical to the stored block (nil vs empty txs), only 3 differs.
	dropped, err := index.Reorg([]Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t1"}},
		{Height: 3, Hash: "h3b", Parent: "h2"},
	})
	if err != nil {
		t.Fatalf("reorg refused: %v", err)
	}
	if !reflect.DeepEqual(dropped, []int64{3}) {
		t.Fatalf("dropped=%v, want [3]", dropped)
	}
	// Re-submitting the branch now in effect succeeds with an empty list.
	dropped, err = index.Reorg([]Block{
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"t1"}},
		{Height: 3, Hash: "h3b", Parent: "h2"},
	})
	if err != nil {
		t.Fatalf("idempotent reorg refused: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped=%v, want empty", dropped)
	}
	if index.Tip != 3 {
		t.Fatalf("tip=%d, want 3", index.Tip)
	}
}

func TestReorgFailureLeavesStateUntouched(t *testing.T) {
	build := func(t *testing.T) *Index {
		return chain(t,
			Block{Height: 1, Hash: "h1", Parent: "genesis"},
			Block{Height: 2, Hash: "h2", Parent: "h1"},
			Block{Height: 3, Hash: "h3", Parent: "h2"},
		)
	}
	cases := map[string][]Block{
		"empty branch":        {},
		"unknown ancestor":    {{Height: 4, Hash: "h4", Parent: "nope"}},
		"bad first height":    {{Height: 3, Hash: "h3b", Parent: "h1"}},
		"gap inside branch":   {{Height: 2, Hash: "h2b", Parent: "h1"}, {Height: 4, Hash: "h4b", Parent: "h2b"}},
		"bad parent inside":   {{Height: 2, Hash: "h2b", Parent: "h1"}, {Height: 3, Hash: "h3b", Parent: "h1"}},
		"empty hash":          {{Height: 2, Hash: "h2b", Parent: "h1"}, {Height: 3, Hash: "", Parent: "h2b"}},
		"duplicate in branch": {{Height: 2, Hash: "h2b", Parent: "h1"}, {Height: 3, Hash: "h2b", Parent: "h2b"}},
		"conflict retained":   {{Height: 2, Hash: "h1", Parent: "h1"}},
		// Error at the very end of the branch: nothing may be applied early.
		"late error": {{Height: 2, Hash: "h2b", Parent: "h1"}, {Height: 3, Hash: "h3b", Parent: "h2b"}, {Height: 5, Hash: "h5b", Parent: "h3b"}},
	}
	for name, branch := range cases {
		t.Run(name, func(t *testing.T) {
			index := build(t)
			blocks, byHash, tip := snapshot(index)
			dropped, err := index.Reorg(branch)
			if err == nil {
				t.Fatal("expected reorg refusal")
			}
			if len(dropped) != 0 {
				t.Fatalf("dropped=%v, want empty on error", dropped)
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

func TestReorgRemovedSuffixHashesMayReappear(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis"},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
		Block{Height: 3, Hash: "h3", Parent: "h2"},
	)
	// h3 lives in the removed suffix, so it may be reused at a new height.
	dropped, err := index.Reorg([]Block{
		{Height: 2, Hash: "h2b", Parent: "h1"},
		{Height: 3, Hash: "h3", Parent: "h2b"},
	})
	if err != nil {
		t.Fatalf("reorg refused: %v", err)
	}
	if !reflect.DeepEqual(dropped, []int64{2, 3}) {
		t.Fatalf("dropped=%v, want [2 3]", dropped)
	}
	if index.ByHash["h3"] != 3 {
		t.Fatalf("h3 not re-indexed: %v", index.ByHash)
	}
}

func TestReorgFailureThenCorrectedRetry(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis"},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
	)
	if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "unknown"}}); err == nil {
		t.Fatal("expected refusal of unknown ancestor")
	}
	dropped, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1"}})
	if err != nil {
		t.Fatalf("corrected retry refused: %v", err)
	}
	if !reflect.DeepEqual(dropped, []int64{2}) || index.Tip != 2 || index.Blocks[2].Hash != "h2b" {
		t.Fatalf("unexpected state after retry: dropped=%v tip=%d", dropped, index.Tip)
	}
}

func TestReorgStoresCopyOfTxs(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "genesis"},
		Block{Height: 2, Hash: "h2", Parent: "h1"},
	)
	txs := []string{"t1"}
	if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2b", Parent: "h1", Txs: txs}}); err != nil {
		t.Fatal(err)
	}
	txs[0] = "mutated"
	if got := index.Blocks[2].Txs; !reflect.DeepEqual(got, []string{"t1"}) {
		t.Fatalf("stored txs changed by caller mutation: %v", got)
	}
}

func TestConcurrentAppendAndReorg(t *testing.T) {
	index := chain(t, Block{Height: 1, Hash: "h1", Parent: "genesis"})
	var wg sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				hash := fmt.Sprintf("w%d-r%d", worker, i)
				// Every branch is rooted at the known genesis block; most
				// calls race with each other and either outcome is fine.
				if _, err := index.Reorg([]Block{{Height: 2, Hash: hash, Parent: "h1"}}); err == nil {
					// Extend the block just installed; fails harmlessly if
					// another caller already replaced it.
					_ = index.Append(Block{Height: 3, Hash: hash + "-x", Parent: hash})
				}
			}
		}(worker)
	}
	wg.Wait()
	// The chain must be fully contiguous with matching parent links, and the
	// two indexes must agree — the result of some complete serial order.
	if int64(len(index.Blocks)) != index.Tip || len(index.ByHash) != len(index.Blocks) {
		t.Fatalf("inconsistent indexes: blocks=%d byHash=%d tip=%d",
			len(index.Blocks), len(index.ByHash), index.Tip)
	}
	for height := int64(2); height <= index.Tip; height++ {
		block, ok := index.Blocks[height]
		if !ok {
			t.Fatalf("gap at height %d (tip=%d)", height, index.Tip)
		}
		if block.Parent != index.Blocks[height-1].Hash {
			t.Fatalf("broken parent link at height %d", height)
		}
		if index.ByHash[block.Hash] != height {
			t.Fatalf("by-hash mismatch at height %d", height)
		}
	}
}
