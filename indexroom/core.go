// Package indexroom implements chain ingestion with optional block
// timestamps, reorg handling, consistent paginated transaction queries, and
// time-bucketed transaction statistics over the main chain.
package indexroom

import (
	"fmt"
	"sync"
	"unicode/utf8"
)

// Block is one ingested block with its transactions.
//
// Time is the optional block timestamp as non-negative Unix seconds. A nil
// pointer means the block carries no timestamp and is distinct from a
// timestamp of zero. Append and Reorg reject negative values; stored blocks
// keep their own copy, so mutating the pointer or its target after a call
// never affects the index. Block timestamps need not be monotonic: equal
// values and values that decrease with height are both accepted.
type Block struct {
	Height int64
	// Hash, Parent, and every Txs identifier must be valid UTF-8; Append and
	// Reorg reject a block carrying invalid bytes rather than letting a
	// snapshot export silently rewrite them to U+FFFD. Txs keeps empty and
	// duplicated identifiers, order, case, and surrounding whitespace
	// exactly as given.
	Hash   string
	Parent string
	Txs    []string
	Time   *int64
}

// Index is the durable chain view.
//
// Append, Reorg, QueryTxs, QueryTimeStats, Export, and Restore are safe for
// concurrent use; the exported fields are meant for read-only inspection
// after all calls have finished.
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

// reorgAppliedHookLocked is a test-only rendezvous invoked from Reorg once
// the alternate branch has fully replaced the old tip range, while index.mu
// is still held and before the call returns. It is nil in production; reorg
// regression tests park a successful reorg here with the new chain already in
// effect, so a QueryTimeStats issued at that instant is forced to wait and,
// once it runs, must observe the complete new chain.
var reorgAppliedHookLocked func(newTip int64)

// appendArrivalHook is a test-only rendezvous invoked from Append BEFORE the
// index lock is taken and before any validation or mutation. It is nil in
// production; concurrent-append regression tests park every contender at the
// gate until all submissions of one round have arrived, then serialize them
// in a content-chosen (but semantically arbitrary) order, so the
// simultaneous-append guarantee is exercised deterministically instead of
// relying on scheduling luck.
var appendArrivalHook func(block Block)

// appendEnteredHookLocked is the matching in-lock rendezvous, invoked once
// the gated contender has acquired index.mu and before validation runs. It is
// nil in production; tests park the chosen first contender here while the
// others block on the mutex, then let it commit — forcing the exact instant
// at which one candidate becomes the main-chain block.
var appendEnteredHookLocked func(block Block)

// Append accepts a block only when it extends the current tip, keeping the
// chain linear. Re-submitting a block identical to one already on the main
// chain is a successful no-op. A block whose hash, parent, or any transaction
// identifier carries invalid UTF-8 is rejected with an error naming the
// height and field (and the zero-based tx position); a rejected append leaves
// the index untouched.
func (index *Index) Append(block Block) error {
	if appendArrivalHook != nil {
		appendArrivalHook(block)
	}
	index.mu.Lock()
	defer index.mu.Unlock()
	if appendEnteredHookLocked != nil {
		appendEnteredHookLocked(block)
	}
	return index.appendLocked(block)
}

func (index *Index) appendLocked(block Block) error {
	if err := validateBlockEncoding(block); err != nil {
		return err
	}
	if block.Hash == "" {
		return errInvalid("block needs a hash")
	}
	if block.Time != nil && *block.Time < 0 {
		return errInvalid("block time must not be negative")
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
// before anything is applied — including UTF-8 validity of every block's
// hash, parent, and transaction identifiers, even in the last block — so a
// rejected reorg leaves the index exactly as it was and reports no dropped
// heights. Heights whose old block is identical to the new one are not
// reported; re-submitting the branch already in effect returns an empty list.
func (index *Index) Reorg(blocks []Block) ([]int64, error) {
	index.mu.Lock()
	defer index.mu.Unlock()

	if len(blocks) == 0 {
		return nil, errInvalid("empty branch")
	}
	// Every identifier must be valid UTF-8 before any lookup or mutation.
	// The first block's parent need not be indexed, but it still has to be
	// encodable, and an error in the last block must reject the whole branch.
	for _, block := range blocks {
		if err := validateBlockEncoding(block); err != nil {
			return nil, err
		}
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
		if block.Time != nil && *block.Time < 0 {
			return nil, errInvalid("block time must not be negative")
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
	if reorgAppliedHookLocked != nil {
		reorgAppliedHookLocked(index.Tip)
	}
	return dropped, nil
}

// storeLocked records a validated block, copying its transactions and
// timestamp so later caller-side mutation cannot alter the stored block.
func (index *Index) storeLocked(block Block) {
	if block.Txs != nil {
		txs := make([]string, len(block.Txs))
		copy(txs, block.Txs)
		block.Txs = txs
	}
	if block.Time != nil {
		t := *block.Time
		block.Time = &t
	}
	index.Blocks[block.Height] = block
	index.ByHash[block.Hash] = block.Height
	index.Tip = block.Height
}

// validateBlockEncoding rejects a block whose hash, parent, or any
// transaction identifier contains invalid UTF-8 bytes. JSON snapshots can
// only carry valid UTF-8 — the encoder would silently rewrite bad bytes to
// U+FFFD, merging distinct identifiers after a restore — so such a block can
// never enter the index. The error names the block height and the field; a
// bad transaction identifier also names its zero-based position. A genuine
// U+FFFD is valid UTF-8 and is never rejected by this check.
func validateBlockEncoding(block Block) error {
	if !utf8.ValidString(block.Hash) {
		return errInvalid(fmt.Sprintf("block at height %d: field %q contains invalid UTF-8 bytes", block.Height, "hash"))
	}
	if !utf8.ValidString(block.Parent) {
		return errInvalid(fmt.Sprintf("block at height %d: field %q contains invalid UTF-8 bytes", block.Height, "parent"))
	}
	for position, tx := range block.Txs {
		if !utf8.ValidString(tx) {
			return errInvalid(fmt.Sprintf("block at height %d: field %q element %d contains invalid UTF-8 bytes", block.Height, "txs", position))
		}
	}
	return nil
}

// sameBlock reports whether two blocks are identical: same height, hash,
// parent, transactions in order, and timestamp presence and value. An empty
// tx list equals a missing one, but a missing timestamp never equals zero.
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
	if (a.Time == nil) != (b.Time == nil) {
		return false
	}
	return a.Time == nil || *a.Time == *b.Time
}

type errInvalid string

func (e errInvalid) Error() string { return string(e) }
