package indexroom

import (
	"errors"
	"fmt"
	"sync"
	"testing"
)

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// snapshot captures the observable chain state for before/after comparisons.
type snapshot struct {
	tip    int64
	blocks map[int64]Block
	byHash map[string]int64
}

func (index *Index) snapshot() snapshot {
	s := snapshot{tip: index.Tip, blocks: map[int64]Block{}, byHash: map[string]int64{}}
	for h, b := range index.Blocks {
		txs := append([]string(nil), b.Txs...)
		s.blocks[h] = Block{Height: b.Height, Hash: b.Hash, Parent: b.Parent, Txs: txs}
	}
	for hash, h := range index.ByHash {
		s.byHash[hash] = h
	}
	return s
}

func (s snapshot) equal(other snapshot) bool {
	if s.tip != other.tip || len(s.blocks) != len(other.blocks) || len(s.byHash) != len(other.byHash) {
		return false
	}
	for h, b := range s.blocks {
		o, ok := other.blocks[h]
		if !ok || !blocksEqual(b, o) {
			return false
		}
	}
	for hash, h := range s.byHash {
		if other.byHash[hash] != h {
			return false
		}
	}
	return true
}

func chain(n int) []Block {
	blocks := make([]Block, n)
	parent := "genesis"
	for i := 0; i < n; i++ {
		h := int64(i + 1)
		blocks[i] = Block{Height: h, Hash: fmt.Sprintf("h%d", h), Parent: parent, Txs: []string{fmt.Sprintf("t%d", h)}}
		parent = blocks[i].Hash
	}
	return blocks
}

func seed(t *testing.T, n int) *Index {
	t.Helper()
	index := New()
	for _, b := range chain(n) {
		if err := index.Append(b); err != nil {
			t.Fatalf("seed append height %d: %v", b.Height, err)
		}
	}
	return index
}

func TestAppendFirstBlock(t *testing.T) {
	cases := []struct {
		name   string
		block  Block
		accept bool
	}{
		{"height 1 with arbitrary parent", Block{Height: 1, Hash: "h1", Parent: "anything"}, true},
		{"height 0", Block{Height: 0, Hash: "h1"}, false},
		{"negative height", Block{Height: -1, Hash: "h1"}, false},
		{"height 2", Block{Height: 2, Hash: "h2"}, false},
		{"empty hash", Block{Height: 1, Hash: ""}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			index := New()
			before := index.snapshot()
			err := index.Append(tc.block)
			if tc.accept {
				if err != nil {
					t.Fatalf("expected accept, got %v", err)
				}
				if index.Tip != 1 || index.Blocks[1].Hash != tc.block.Hash {
					t.Fatalf("state not applied: tip=%d", index.Tip)
				}
			} else {
				if err == nil {
					t.Fatalf("expected rejection, got nil")
				}
				if !before.equal(index.snapshot()) {
					t.Fatalf("state changed after rejected append: %v", errString(err))
				}
			}
		})
	}
}

func TestAppendLinear(t *testing.T) {
	index := seed(t, 2)

	cases := []struct {
		name   string
		block  Block
		accept bool
	}{
		{"correct next block", Block{Height: 3, Hash: "h3", Parent: "h2"}, true},
		{"height jump", Block{Height: 5, Hash: "h5", Parent: "h3"}, false},
		{"wrong parent", Block{Height: 4, Hash: "h4", Parent: "h1"}, false},
		{"empty hash", Block{Height: 4, Hash: "", Parent: "h3"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := index.snapshot()
			err := index.Append(tc.block)
			if tc.accept {
				if err != nil {
					t.Fatalf("expected accept, got %v", err)
				}
				if index.Tip != tc.block.Height {
					t.Fatalf("tip=%d, want %d", index.Tip, tc.block.Height)
				}
			} else {
				if err == nil {
					t.Fatalf("expected rejection, got nil")
				}
				if !before.equal(index.snapshot()) {
					t.Fatalf("state changed after rejected append: %v", errString(err))
				}
			}
		})
	}
}

func TestAppendDuplicateIsIdempotent(t *testing.T) {
	index := seed(t, 2)
	before := index.snapshot()

	// Exact same block: same height, hash, parent, txs in order.
	if err := index.Append(chain(2)[1]); err != nil {
		t.Fatalf("duplicate append: %v", err)
	}
	if index.Tip != 2 {
		t.Fatalf("tip changed after duplicate: %d", index.Tip)
	}
	if !before.equal(index.snapshot()) {
		t.Fatalf("state changed after duplicate append")
	}

	// Empty tx list and no txs are the same block.
	index2 := New()
	if err := index2.Append(Block{Height: 1, Hash: "a"}); err != nil {
		t.Fatal(err)
	}
	if err := index2.Append(Block{Height: 1, Hash: "a", Txs: []string{}}); err != nil {
		t.Fatalf("empty tx list should be identical: %v", err)
	}
	if index2.Tip != 1 {
		t.Fatalf("tip changed: %d", index2.Tip)
	}
}

func TestAppendConflictsRefuseAndDoNotOverwrite(t *testing.T) {
	index := seed(t, 2)
	before := index.snapshot()

	conflicts := []Block{
		{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"different"}}, // occupied height, different txs
		{Height: 2, Hash: "h9", Parent: "h1"},                                  // occupied height, different hash
		{Height: 3, Hash: "h1", Parent: "h2"},                                  // hash already indexed at another height
		{Height: 3, Hash: "h2", Parent: "h2"},                                  // hash already indexed at another height
	}
	for i, block := range conflicts {
		if err := index.Append(block); err == nil {
			t.Fatalf("case %d: expected rejection", i)
		}
	}
	if !before.equal(index.snapshot()) {
		t.Fatalf("state changed after conflicting appends")
	}
	// Original records untouched.
	if index.Blocks[1].Hash != "h1" || index.Blocks[2].Hash != "h2" {
		t.Fatalf("existing records were overwritten")
	}
}

func TestAppendDeepCopiesTxs(t *testing.T) {
	index := New()
	block := Block{Height: 1, Hash: "h1", Txs: []string{"t1", "t2"}}
	if err := index.Append(block); err != nil {
		t.Fatal(err)
	}
	block.Txs[0] = "mutated"
	block.Txs = append(block.Txs, "t3")
	stored := index.Blocks[1]
	if len(stored.Txs) != 2 || stored.Txs[0] != "t1" {
		t.Fatalf("stored txs changed after caller mutation: %v", stored.Txs)
	}
}

func TestReorgBasic(t *testing.T) {
	index := seed(t, 3)
	branch := []Block{
		{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"t4"}},
	}
	dropped, err := index.Reorg(branch)
	if err != nil {
		t.Fatalf("reorg: %v", err)
	}
	wantDropped := []int64{2, 3}
	if len(dropped) != len(wantDropped) {
		t.Fatalf("dropped=%v, want %v", dropped, wantDropped)
	}
	for i := range wantDropped {
		if dropped[i] != wantDropped[i] {
			t.Fatalf("dropped=%v, want %v", dropped, wantDropped)
		}
	}
	if index.Tip != 2 {
		t.Fatalf("tip=%d, want 2", index.Tip)
	}
	if _, ok := index.Blocks[3]; ok {
		t.Fatalf("truncated height 3 still present")
	}
	if _, ok := index.ByHash["h3"]; ok {
		t.Fatalf("truncated hash h3 still present")
	}
	if index.Blocks[2].Hash != "h2b" {
		t.Fatalf("height 2 not replaced")
	}
	if index.ByHash["h2b"] != 2 {
		t.Fatalf("h2b not indexed")
	}
	if index.ByHash["h2"] != 0 {
		t.Fatalf("old hash h2 still indexed")
	}
}

func TestReorgLongerBranch(t *testing.T) {
	index := seed(t, 2)
	branch := []Block{
		{Height: 2, Hash: "h2b", Parent: "h1"},
		{Height: 3, Hash: "h3b", Parent: "h2b"},
		{Height: 4, Hash: "h4b", Parent: "h3b"},
	}
	dropped, err := index.Reorg(branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0] != 2 {
		t.Fatalf("dropped=%v, want [2]", dropped)
	}
	if index.Tip != 4 {
		t.Fatalf("tip=%d, want 4", index.Tip)
	}
	for h, hash := range map[int64]string{2: "h2b", 3: "h3b", 4: "h4b"} {
		if index.Blocks[h].Hash != hash {
			t.Fatalf("height %d hash=%s, want %s", h, index.Blocks[h].Hash, hash)
		}
	}
}

func TestReorgShorterBranch(t *testing.T) {
	index := seed(t, 4)
	branch := []Block{{Height: 2, Hash: "h2b", Parent: "h1"}}
	dropped, err := index.Reorg(branch)
	if err != nil {
		t.Fatal(err)
	}
	want := []int64{2, 3, 4}
	if len(dropped) != len(want) {
		t.Fatalf("dropped=%v, want %v", dropped, want)
	}
	for i := range want {
		if dropped[i] != want[i] {
			t.Fatalf("dropped=%v, want %v", dropped, want)
		}
	}
	if index.Tip != 2 {
		t.Fatalf("tip=%d", index.Tip)
	}
	for _, h := range []int64{3, 4} {
		if _, ok := index.Blocks[h]; ok {
			t.Fatalf("height %d should be gone", h)
		}
	}
	for _, hash := range []string{"h3", "h4"} {
		if _, ok := index.ByHash[hash]; ok {
			t.Fatalf("hash %s should be gone", hash)
		}
	}
}

func TestReorgBranchExtendingTip(t *testing.T) {
	index := seed(t, 2)
	branch := []Block{{Height: 3, Hash: "h3", Parent: "h2"}}
	dropped, err := index.Reorg(branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped=%v, want empty", dropped)
	}
	if index.Tip != 3 {
		t.Fatalf("tip=%d", index.Tip)
	}
}

func TestReorgIdenticalBlocksNotCounted(t *testing.T) {
	index := seed(t, 3)
	// Branch reuses h2 verbatim and adds a new h3b.
	branch := []Block{
		chain(3)[1], // h2 identical
		{Height: 3, Hash: "h3b", Parent: "h2"},
	}
	dropped, err := index.Reorg(branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 1 || dropped[0] != 3 {
		t.Fatalf("dropped=%v, want [3]", dropped)
	}
	if index.Blocks[2].Hash != "h2" {
		t.Fatalf("identical h2 should remain")
	}
}

func TestReorgSameBranchIsNoop(t *testing.T) {
	index := seed(t, 3)
	branch := []Block{
		{Height: 2, Hash: "h2b", Parent: "h1"},
		{Height: 3, Hash: "h3b", Parent: "h2b"},
	}
	if _, err := index.Reorg(branch); err != nil {
		t.Fatal(err)
	}
	before := index.snapshot()
	dropped, err := index.Reorg(branch)
	if err != nil {
		t.Fatalf("re-submitting same branch: %v", err)
	}
	if len(dropped) != 0 {
		t.Fatalf("dropped=%v, want empty", dropped)
	}
	if !before.equal(index.snapshot()) {
		t.Fatalf("state changed after no-op reorg")
	}
}

func TestReorgRejectedLeavesStateUntouched(t *testing.T) {
	index := seed(t, 3)
	before := index.snapshot()

	cases := []struct {
		name   string
		branch []Block
	}{
		{"empty branch", nil},
		{"unknown ancestor", []Block{{Height: 2, Hash: "x", Parent: "nope"}}},
		{"first height wrong", []Block{{Height: 3, Hash: "x", Parent: "h1"}}},
		{"gap in heights", []Block{
			{Height: 2, Hash: "h2b", Parent: "h1"},
			{Height: 4, Hash: "h4b", Parent: "h2b"},
		}},
		{"broken parent link", []Block{
			{Height: 2, Hash: "h2b", Parent: "h1"},
			{Height: 3, Hash: "h3b", Parent: "wrong"},
		}},
		{"empty hash in branch", []Block{
			{Height: 2, Hash: "h2b", Parent: "h1"},
			{Height: 3, Hash: "", Parent: "h2b"},
		}},
		{"duplicate hash in branch", []Block{
			{Height: 2, Hash: "dup", Parent: "h1"},
			{Height: 3, Hash: "dup", Parent: "dup"},
		}},
		{"hash conflicts with retained chain", []Block{
			{Height: 2, Hash: "h1", Parent: "h1"},
		}},
		{"error at end of branch", []Block{
			{Height: 2, Hash: "h2b", Parent: "h1"},
			{Height: 3, Hash: "h3b", Parent: "h2b"},
			{Height: 4, Hash: "", Parent: "h3b"},
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dropped, err := index.Reorg(tc.branch)
			if err == nil {
				t.Fatalf("expected rejection")
			}
			if len(dropped) != 0 {
				t.Fatalf("dropped=%v, want empty on rejection", dropped)
			}
			if !before.equal(index.snapshot()) {
				t.Fatalf("state changed after rejected reorg: %v", errString(err))
			}
		})
	}
}

func TestReorgSuffixHashAllowedInBranch(t *testing.T) {
	index := seed(t, 3)
	// Branch from h1 reuses hash h3 (from the removed suffix) at height 2.
	branch := []Block{
		{Height: 2, Hash: "h3", Parent: "h1"},
		{Height: 3, Hash: "h3b", Parent: "h3"},
	}
	dropped, err := index.Reorg(branch)
	if err != nil {
		t.Fatal(err)
	}
	if len(dropped) != 2 {
		t.Fatalf("dropped=%v, want [2 3]", dropped)
	}
	if index.Blocks[2].Hash != "h3" {
		t.Fatalf("suffix hash should be allowed in new branch")
	}
}

func TestReorgDeepCopiesTxs(t *testing.T) {
	index := seed(t, 2)
	branch := []Block{{Height: 2, Hash: "h2b", Parent: "h1", Txs: []string{"t4", "t5"}}}
	if _, err := index.Reorg(branch); err != nil {
		t.Fatal(err)
	}
	branch[0].Txs[0] = "mutated"
	branch[0].Txs = append(branch[0].Txs, "t6")
	stored := index.Blocks[2]
	if len(stored.Txs) != 2 || stored.Txs[0] != "t4" {
		t.Fatalf("stored txs changed after caller mutation: %v", stored.Txs)
	}
}

func TestReorgThenCorrectAndRetry(t *testing.T) {
	index := seed(t, 3)
	bad := []Block{
		{Height: 2, Hash: "h2b", Parent: "h1"},
		{Height: 3, Hash: "", Parent: "h2b"}, // invalid
	}
	if _, err := index.Reorg(bad); err == nil {
		t.Fatal("expected rejection")
	}
	good := []Block{
		{Height: 2, Hash: "h2b", Parent: "h1"},
		{Height: 3, Hash: "h3b", Parent: "h2b"},
	}
	dropped, err := index.Reorg(good)
	if err != nil {
		t.Fatalf("retry after fix: %v", err)
	}
	if len(dropped) != 2 || index.Tip != 3 {
		t.Fatalf("dropped=%v tip=%d", dropped, index.Tip)
	}
}

func TestConcurrentAppends(t *testing.T) {
	index := New()
	if err := index.Append(Block{Height: 1, Hash: "h1"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 64)
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs <- index.Append(Block{Height: 2, Hash: fmt.Sprintf("h2-%d", i), Parent: "h1"})
		}(i)
	}
	wg.Wait()
	close(errs)
	var successes int
	for err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("exactly one append should succeed, got %d", successes)
	}
	if index.Tip != 2 {
		t.Fatalf("tip=%d", index.Tip)
	}
	if index.ByHash[index.Blocks[2].Hash] != 2 {
		t.Fatalf("hash index inconsistent")
	}
}

func TestConcurrentReorgs(t *testing.T) {
	index := seed(t, 6)
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			branch := []Block{
				{Height: 2, Hash: fmt.Sprintf("h2b-%d", i), Parent: "h1"},
				{Height: 3, Hash: fmt.Sprintf("h3b-%d", i), Parent: fmt.Sprintf("h2b-%d", i)},
			}
			_, _ = index.Reorg(branch)
		}(i)
	}
	wg.Wait()

	// Final state must be one complete branch: linear, hashes consistent.
	if index.Tip != 3 {
		t.Fatalf("tip=%d, want 3", index.Tip)
	}
	seen := map[string]bool{}
	for h := int64(1); h <= index.Tip; h++ {
		b, ok := index.Blocks[h]
		if !ok {
			t.Fatalf("height %d missing", h)
		}
		if b.Height != h {
			t.Fatalf("height mismatch at %d", h)
		}
		if seen[b.Hash] {
			t.Fatalf("duplicate hash %s", b.Hash)
		}
		seen[b.Hash] = true
		if index.ByHash[b.Hash] != h {
			t.Fatalf("hash index inconsistent for %s", b.Hash)
		}
	}
	if _, ok := index.ByHash["h4"]; ok {
		t.Fatalf("stale hash h4 remains")
	}
}

func TestErrorsAreTyped(t *testing.T) {
	var e errInvalid
	if !errors.As(errInvalid("x"), &e) {
		t.Fatal("errInvalid should be usable with errors.As")
	}
}
