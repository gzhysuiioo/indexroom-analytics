package indexroom

import (
	"reflect"
	"testing"
)

func intptr(v int64) *int64 { return &v }

func TestAppendTimestampMissingIsDistinctFromZero(t *testing.T) {
	index := New()
	if err := index.Append(Block{Height: 1, Hash: "h1", Parent: "g"}); err != nil {
		t.Fatal(err)
	}
	// Replaying with a real zero timestamp is different block content.
	if err := index.Append(Block{Height: 1, Hash: "h1", Parent: "g", Time: intptr(0)}); err == nil {
		t.Fatal("missing timestamp replayed as zero was accepted")
	}
	if got := index.Blocks[1].Time; got != nil {
		t.Fatalf("stored time changed: %v", got)
	}

	other := New()
	if err := other.Append(Block{Height: 1, Hash: "h1", Parent: "g", Time: intptr(0)}); err != nil {
		t.Fatal(err)
	}
	if err := other.Append(Block{Height: 1, Hash: "h1", Parent: "g"}); err == nil {
		t.Fatal("zero timestamp replayed as missing was accepted")
	}
	if got := *other.Blocks[1].Time; got != 0 {
		t.Fatalf("stored zero changed: %v", got)
	}
}

func TestAppendAcceptsEqualAndDecreasingTimes(t *testing.T) {
	index := New()
	blocks := []Block{
		{Height: 1, Hash: "h1", Parent: "g", Time: intptr(100)},
		{Height: 2, Hash: "h2", Parent: "h1", Time: intptr(100)},
		{Height: 3, Hash: "h3", Parent: "h2", Time: intptr(50)},
		{Height: 4, Hash: "h4", Parent: "h3"}, // missing time among known times
		{Height: 5, Hash: "h5", Parent: "h4", Time: intptr(0)},
	}
	for _, block := range blocks {
		if err := index.Append(block); err != nil {
			t.Fatalf("append at %d refused: %v", block.Height, err)
		}
	}
	if index.Blocks[1].Time == nil || *index.Blocks[1].Time != 100 {
		t.Fatalf("time not stored: %v", index.Blocks[1].Time)
	}
	if index.Blocks[4].Time != nil {
		t.Fatalf("height 4 should have missing time: %v", index.Blocks[4].Time)
	}
}

func TestAppendNegativeTimeRejectedAtomically(t *testing.T) {
	index := chain(t, Block{Height: 1, Hash: "h1", Parent: "g", Time: intptr(10)})
	blocks, byHash, tip := snapshot(index)
	if err := index.Append(Block{Height: 2, Hash: "h2", Parent: "h1", Time: intptr(-1)}); err == nil {
		t.Fatal("negative timestamp accepted")
	}
	requireUnchanged(t, index, blocks, byHash, tip)
}

func TestAppendStoresCopyOfTime(t *testing.T) {
	index := New()
	when := int64(42)
	if err := index.Append(Block{Height: 1, Hash: "h1", Parent: "g", Time: &when}); err != nil {
		t.Fatal(err)
	}
	when = 99
	if got := index.Blocks[1].Time; got == nil || *got != 42 {
		t.Fatalf("stored time aliases caller memory: %v", got)
	}
}

func TestAppendTimeOnlyDifferenceIsConflict(t *testing.T) {
	index := chain(t, Block{Height: 1, Hash: "h1", Parent: "g", Time: intptr(7)})
	blocks, byHash, tip := snapshot(index)
	for _, block := range []Block{
		{Height: 1, Hash: "h1", Parent: "g", Time: intptr(8)},
		{Height: 1, Hash: "h1", Parent: "g"}, // known -> missing
	} {
		if err := index.Append(block); err == nil {
			t.Fatalf("time-only difference accepted: %+v", block)
		}
		requireUnchanged(t, index, blocks, byHash, tip)
	}
	// Identical replay including a zero timestamp stays a no-op.
	if err := index.Append(Block{Height: 1, Hash: "h1", Parent: "g", Time: intptr(7)}); err != nil {
		t.Fatalf("identical replay refused: %v", err)
	}
}

func TestReorgTimeOnlyChangeReplacesBlock(t *testing.T) {
	index := chain(t,
		Block{Height: 1, Hash: "h1", Parent: "g", Time: intptr(1)},
		Block{Height: 2, Hash: "h2", Parent: "h1", Time: intptr(2)},
		Block{Height: 3, Hash: "h3", Parent: "h2", Time: intptr(3)},
	)
	// Same hashes and txs, only height 2's timestamp changes: it must drop.
	dropped, err := index.Reorg([]Block{
		{Height: 2, Hash: "h2", Parent: "h1", Time: intptr(20)},
		{Height: 3, Hash: "h3", Parent: "h2", Time: intptr(3)},
	})
	if err != nil {
		t.Fatalf("reorg refused: %v", err)
	}
	if !reflect.DeepEqual(dropped, []int64{2}) {
		t.Fatalf("dropped=%v, want [2]", dropped)
	}
	if got := *index.Blocks[2].Time; got != 20 {
		t.Fatalf("time not replaced: %d", got)
	}
	// Replaying the identical branch (same times) reports nothing.
	dropped, err = index.Reorg([]Block{
		{Height: 2, Hash: "h2", Parent: "h1", Time: intptr(20)},
		{Height: 3, Hash: "h3", Parent: "h2", Time: intptr(3)},
	})
	if err != nil || len(dropped) != 0 {
		t.Fatalf("identical replay: dropped=%v err=%v", dropped, err)
	}
}

func TestReorgNegativeTimeFailsWholeOperation(t *testing.T) {
	build := func(t *testing.T) *Index {
		return chain(t,
			Block{Height: 1, Hash: "h1", Parent: "g", Time: intptr(1)},
			Block{Height: 2, Hash: "h2", Parent: "h1", Time: intptr(2)},
		)
	}
	for name, branch := range map[string][]Block{
		"negative at start": {
			{Height: 2, Hash: "h2b", Parent: "h1", Time: intptr(-1)},
		},
		"negative at end": {
			{Height: 2, Hash: "h2b", Parent: "h1", Time: intptr(20)},
			{Height: 3, Hash: "h3b", Parent: "h2b", Time: intptr(-9)},
		},
		"missing replay swapped to negative": {
			{Height: 2, Hash: "h2b2", Parent: "h1", Time: intptr(-1)},
			{Height: 3, Hash: "h3b2", Parent: "h2b2", Time: intptr(4)},
		},
	} {
		t.Run(name, func(t *testing.T) {
			index := build(t)
			blocks, byHash, tip := snapshot(index)
			dropped, err := index.Reorg(branch)
			if err == nil {
				t.Fatal("negative-time branch accepted")
			}
			if len(dropped) != 0 {
				t.Fatalf("dropped=%v, want empty", dropped)
			}
			requireUnchanged(t, index, blocks, byHash, tip)
		})
	}
}

func TestReorgStoresCopyOfTime(t *testing.T) {
	index := chain(t, Block{Height: 1, Hash: "h1", Parent: "g"})
	when := int64(5)
	if _, err := index.Reorg([]Block{{Height: 2, Hash: "h2", Parent: "h1", Time: &when}}); err != nil {
		t.Fatal(err)
	}
	when = 500
	if got := index.Blocks[2].Time; got == nil || *got != 5 {
		t.Fatalf("stored time aliases caller memory: %v", got)
	}
}
