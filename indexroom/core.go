// Package indexroom implements chain ingestion with reorg handling.
package indexroom

import "sort"

// Block is one ingested block with its transactions.
type Block struct {
	Height int64
	Hash   string
	Parent string
	Txs    []string
}

// Index is the durable chain view.
type Index struct {
	Blocks map[int64]Block
	ByHash map[string]int64
	Tip    int64
}

// New returns an empty index.
func New() *Index { return &Index{Blocks: map[int64]Block{}, ByHash: map[string]int64{}} }

// Append accepts a block only when it extends the current tip, keeping the chain linear.
func (index *Index) Append(block Block) error {
	if block.Height <= 0 || block.Hash == "" {
		return errInvalid("block needs a height and hash")
	}
	if _, exists := index.Blocks[block.Height]; exists {
		return errInvalid("height already indexed")
	}
	if index.Tip != 0 && block.Parent != index.Blocks[index.Tip].Hash {
		return errInvalid("parent does not match the current tip")
	}
	index.Blocks[block.Height] = block
	index.ByHash[block.Hash] = block.Height
	index.Tip = block.Height
	return nil
}

// Reorg replaces the tip range with an alternate branch and reports dropped heights.
func (index *Index) Reorg(blocks []Block) ([]int64, error) {
	if len(blocks) == 0 {
		return nil, errInvalid("empty branch")
	}
	parent, ok := index.ByHash[blocks[0].Parent]
	if !ok {
		return nil, errInvalid("branch parent is unknown")
	}
	var dropped []int64
	for height := parent + 1; height <= index.Tip; height++ {
		dropped = append(dropped, height)
		delete(index.ByHash, index.Blocks[height].Hash)
		delete(index.Blocks, height)
	}
	index.Tip = parent
	for _, block := range blocks {
		if err := index.Append(block); err != nil {
			return dropped, err
		}
	}
	sort.Slice(dropped, func(i, j int) bool { return dropped[i] < dropped[j] })
	return dropped, nil
}

type errInvalid string

func (e errInvalid) Error() string { return string(e) }
