// Package indexroom implements chain ingestion with reorg handling.
package indexroom

import (
	"sort"
	"sync"
)

// Block is one ingested block with its transactions.
type Block struct {
	Height int64
	Hash   string
	Parent string
	Txs    []string
}

// Index is the durable chain view. The exported fields are a read-only view of
// the chain state once all concurrent calls have finished; mutating them from
// outside while calls are in progress is not supported.
type Index struct {
	mu     sync.Mutex
	Blocks map[int64]Block
	ByHash map[string]int64
	Tip    int64
}

// New returns an empty index.
func New() *Index {
	return &Index{Blocks: map[int64]Block{}, ByHash: map[string]int64{}}
}

// Append accepts a block only when it extends the current tip, keeping the chain
// linear. Submitting a block that is already stored verbatim is a successful
// no-op; any conflicting content at an occupied height or hash is refused
// without touching the existing state.
func (index *Index) Append(block Block) error {
	index.mu.Lock()
	defer index.mu.Unlock()

	if block.Height <= 0 {
		return errInvalid("block needs a positive height")
	}
	if block.Hash == "" {
		return errInvalid("block needs a hash")
	}

	if index.Tip == 0 {
		// The first block of an empty index must sit at height 1; its parent
		// is the chain start identifier and does not need to be indexed.
		if block.Height != 1 {
			return errInvalid("first block must be at height 1")
		}
	} else {
		if block.Height <= index.Tip {
			existing, exists := index.Blocks[block.Height]
			if exists && blocksEqual(existing, block) {
				return nil
			}
			return errInvalid("height already indexed with different content")
		}
		if block.Height != index.Tip+1 {
			return errInvalid("block height does not extend the current tip")
		}
		if block.Parent != index.Blocks[index.Tip].Hash {
			return errInvalid("parent does not match the current tip")
		}
	}

	if height, exists := index.ByHash[block.Hash]; exists {
		if height == block.Height {
			if existing, ok := index.Blocks[block.Height]; ok && blocksEqual(existing, block) {
				return nil
			}
			return errInvalid("hash already indexed with different content")
		}
		return errInvalid("hash already indexed at another height")
	}

	index.storeLocked(block)
	return nil
}

// Reorg replaces the tip range with an alternate branch and reports dropped
// heights. The whole branch is validated against the current chain before any
// state changes, so a rejected reorg leaves the index exactly as it was.
// Re-submitting the branch that is already in effect is a successful no-op.
func (index *Index) Reorg(blocks []Block) ([]int64, error) {
	index.mu.Lock()
	defer index.mu.Unlock()

	if len(blocks) == 0 {
		return nil, errInvalid("empty branch")
	}

	ancestor, err := index.validateBranchLocked(blocks)
	if err != nil {
		return nil, err
	}

	var dropped []int64
	for height := ancestor + 1; height <= index.Tip; height++ {
		old := index.Blocks[height]
		if idx := int(height - (ancestor + 1)); idx < len(blocks) && blocksEqual(old, blocks[idx]) {
			// The new block at this height is identical to the old one; keep
			// it in place and do not report it as replaced.
			continue
		}
		dropped = append(dropped, height)
		delete(index.ByHash, old.Hash)
		delete(index.Blocks, height)
	}

	for _, block := range blocks {
		index.storeLocked(block)
	}

	sort.Slice(dropped, func(i, j int) bool { return dropped[i] < dropped[j] })
	return dropped, nil
}

// validateBranchLocked checks the branch against the current chain without
// modifying anything. It returns the ancestor height the branch forks from.
func (index *Index) validateBranchLocked(blocks []Block) (int64, error) {
	first := blocks[0]
	if first.Height <= 0 {
		return 0, errInvalid("branch block needs a positive height")
	}
	if first.Hash == "" {
		return 0, errInvalid("branch block needs a hash")
	}
	ancestor, ok := index.ByHash[first.Parent]
	if !ok {
		return 0, errInvalid("branch parent is unknown")
	}
	if first.Height != ancestor+1 {
		return 0, errInvalid("branch must start at the ancestor height plus one")
	}

	seen := make(map[string]struct{}, len(blocks))
	for i, block := range blocks {
		if block.Height <= 0 {
			return 0, errInvalid("branch block needs a positive height")
		}
		if block.Hash == "" {
			return 0, errInvalid("branch block needs a hash")
		}
		if i > 0 {
			prev := blocks[i-1]
			if block.Height != prev.Height+1 {
				return 0, errInvalid("branch heights must be consecutive")
			}
			if block.Parent != prev.Hash {
				return 0, errInvalid("branch parent does not link to the previous block")
			}
		}
		if _, dup := seen[block.Hash]; dup {
			return 0, errInvalid("branch contains a duplicate hash")
		}
		seen[block.Hash] = struct{}{}
		if height, exists := index.ByHash[block.Hash]; exists && height <= ancestor {
			return 0, errInvalid("branch hash conflicts with a retained block")
		}
	}
	return ancestor, nil
}

// storeLocked inserts a block that has already passed validation. The
// transaction list is copied so later caller mutation cannot affect stored
// content.
func (index *Index) storeLocked(block Block) {
	block.Txs = append([]string(nil), block.Txs...)
	index.Blocks[block.Height] = block
	index.ByHash[block.Hash] = block.Height
	index.Tip = block.Height
}

// blocksEqual reports whether two blocks are the same block: height, hash,
// parent and the transaction list in its original order. A nil transaction
// list and an empty one are the same.
func blocksEqual(a, b Block) bool {
	if a.Height != b.Height || a.Hash != b.Hash || a.Parent != b.Parent {
		return false
	}
	if len(a.Txs) != len(b.Txs) {
		return false
	}
	for i := range a.Txs {
		if a.Txs[i] != b.Txs[i] {
			return false
		}
	}
	return true
}

type errInvalid string

func (e errInvalid) Error() string { return string(e) }
