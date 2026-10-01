// Package indexroom implements chain ingestion with reorg handling and
// consistent paginated transaction queries over the main chain.
package indexroom

import "sync"

// Block is one ingested block with its transactions.
type Block struct {
	Height int64
	Hash   string
	Parent string
	Txs    []string
}

// Index is the durable chain view.
//
// Append, Reorg, QueryTxs, Export, and Restore are safe for concurrent use;
// the exported fields are meant for read-only inspection after all calls
// have finished.
type Index struct {
	Blocks map[int64]Block
	ByHash map[string]int64
	Tip    int64

	mu  sync.Mutex
	key [32]byte
}

// New returns an empty index.
func New() *Index {
	return &Index{Blocks: map[int64]Block{}, ByHash: map[string]int64{}, key: newKey()}
}

// Append accepts a block only when it extends the current tip, keeping the
// chain linear. Re-submitting a block identical to one already on the main
// chain is a successful no-op. A rejected append leaves the index untouched.
func (index *Index) Append(block Block) error {
	index.mu.Lock()
	defer index.mu.Unlock()
	return index.appendLocked(block)
}

func (index *Index) appendLocked(block Block) error {
	if block.Hash == "" {
		return errInvalid("block needs a hash")
	}
	if index.Tip == 0 {
		// Empty index: only the first block at height 1 is accepted; its
		// parent becomes the chain start identifier and need not be indexed.
		if block.Height != 1 {
			return errInvalid("first block must be at height 1")
		}
		index.storeLocked(block)
		return nil
	}
	if old, exists := index.Blocks[block.Height]; exists {
		if sameBlock(old, block) {
			return nil
		}
		return errInvalid("height already indexed with different content")
	}
	if block.Height != index.Tip+1 {
		return errInvalid("block height must extend the current tip")
	}
	if block.Parent != index.Blocks[index.Tip].Hash {
		return errInvalid("parent does not match the current tip")
	}
	if _, exists := index.ByHash[block.Hash]; exists {
		return errInvalid("hash already indexed at another height")
	}
	index.storeLocked(block)
	return nil
}

// Reorg replaces the tip range with an alternate branch and reports the
// dropped old heights in ascending order. The branch is validated in full
// before anything is applied, so a rejected reorg leaves the index exactly
// as it was. Heights whose old block is identical to the new one are not
// reported; re-submitting the branch already in effect returns an empty list.
func (index *Index) Reorg(blocks []Block) ([]int64, error) {
	index.mu.Lock()
	defer index.mu.Unlock()

	if len(blocks) == 0 {
		return nil, errInvalid("empty branch")
	}
	ancestor, ok := index.ByHash[blocks[0].Parent]
	if !ok {
		return nil, errInvalid("branch parent is unknown")
	}
	if blocks[0].Height != ancestor+1 {
		return nil, errInvalid("branch must start at the ancestor height plus one")
	}
	seen := make(map[string]bool, len(blocks))
	for i, block := range blocks {
		if block.Hash == "" {
			return nil, errInvalid("block needs a hash")
		}
		if seen[block.Hash] {
			return nil, errInvalid("duplicate hash inside branch")
		}
		seen[block.Hash] = true
		if height, exists := index.ByHash[block.Hash]; exists && height <= ancestor {
			return nil, errInvalid("hash conflicts with a retained block")
		}
		if i > 0 {
			prev := blocks[i-1]
			if block.Height != prev.Height+1 {
				return nil, errInvalid("branch heights must be consecutive")
			}
			if block.Parent != prev.Hash {
				return nil, errInvalid("branch parent does not match the previous block")
			}
		}
	}

	// Everything validated; compute the dropped heights before mutating.
	newTip := blocks[len(blocks)-1].Height
	dropped := []int64{}
	for height := ancestor + 1; height <= index.Tip; height++ {
		if height > newTip {
			dropped = append(dropped, height)
			continue
		}
		if !sameBlock(index.Blocks[height], blocks[height-blocks[0].Height]) {
			dropped = append(dropped, height)
		}
	}
	for height := ancestor + 1; height <= index.Tip; height++ {
		delete(index.ByHash, index.Blocks[height].Hash)
		delete(index.Blocks, height)
	}
	index.Tip = ancestor
	for _, block := range blocks {
		index.storeLocked(block)
	}
	return dropped, nil
}

// storeLocked records a validated block, copying its transactions so later
// caller-side mutation of the slice cannot alter the stored block.
func (index *Index) storeLocked(block Block) {
	if block.Txs != nil {
		txs := make([]string, len(block.Txs))
		copy(txs, block.Txs)
		block.Txs = txs
	}
	index.Blocks[block.Height] = block
	index.ByHash[block.Hash] = block.Height
	index.Tip = block.Height
}

// sameBlock reports whether two blocks are identical: same height, hash,
// parent, and transactions in order. An empty tx list equals a missing one.
func sameBlock(a, b Block) bool {
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
