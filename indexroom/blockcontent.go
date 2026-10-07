package indexroom

import (
	"crypto/sha256"
	"encoding/binary"
	"io"
)

// This file is the single home of the "same block content" rule. Every site
// that must decide whether two views of a block carry identical content uses
// it, so the rule is maintained once:
//
//   - Append resubmission (appendLocked) treats a repeated height as a
//     no-op only when equalBlockContent says equal;
//   - Reorg reports a dropped height using exactly the same comparison;
//   - QueryTxs continuation cursors summarize a pinned range by hashing the
//     same encoding with hashRangeBlockContent, so "equal" and "the
//     fingerprint did not change" can never drift apart.
//
// Content means, together: height, hash, parent hash, every transaction
// identifier in its original order, and whether a timestamp is present plus
// its exact value. Equality is never the block hash alone. Two encoding
// details are load-bearing:
//
//   - A missing transaction list (nil) and an empty one encode the same, so
//     they are equal; empty identifiers, repetitions, case, and surrounding
//     whitespace are all kept verbatim — ["ab","c"] and ["a","bc"] differ,
//     and so does swapping or repeating entries.
//   - A missing timestamp (nil) is a distinct encoding from a real timestamp
//     of zero; two real timestamps are equal only when their seconds match.

// writeBlockContent writes one block's canonical content to w, field by
// field, each length-prefixed so concatenations cannot collide across field
// boundaries: in particular ["ab","c"] and ["a","bc"] serialize differently.
// The exact byte sequence is the contract pinned by existing continuation
// cursors, so it must not change.
func writeBlockContent(w io.Writer, block Block) {
	var lenBuf [8]byte
	writeString := func(s string) {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		w.Write(lenBuf[:])
		w.Write([]byte(s))
	}
	binary.BigEndian.PutUint64(lenBuf[:], uint64(block.Height))
	w.Write(lenBuf[:])
	writeString(block.Hash)
	writeString(block.Parent)
	// Only lengths and elements are emitted: an absent slice and an empty
	// slice both yield a zero transaction count and no identifiers.
	binary.BigEndian.PutUint64(lenBuf[:], uint64(len(block.Txs)))
	w.Write(lenBuf[:])
	for _, tx := range block.Txs {
		writeString(tx)
	}
	// Presence is encoded before the value, so a missing timestamp never
	// equals a real zero.
	if block.Time == nil {
		w.Write([]byte{0})
		return
	}
	w.Write([]byte{1})
	binary.BigEndian.PutUint64(lenBuf[:], uint64(*block.Time))
	w.Write(lenBuf[:])
}

// blockContentDigest hashes one block's canonical content. The encoding is
// injective, so two blocks digest alike exactly when equalBlockContent would
// accept them.
func blockContentDigest(block Block) [sha256.Size]byte {
	h := sha256.New()
	writeBlockContent(h, block)
	var sum [sha256.Size]byte
	h.Sum(sum[:0])
	return sum
}

// equalBlockContent reports whether two blocks carry identical content under
// the single rule at the top of this file.
func equalBlockContent(a, b Block) bool {
	return blockContentDigest(a) == blockContentDigest(b)
}

// hashRangeBlockContent folds the canonical content of every block in the
// inclusive range [from, to] into one digest, in ascending height order, by
// streaming every block's canonical bytes through one hasher. It is both the
// fingerprint minted into a first-page cursor and the value rechecked on
// every continuation; keeping this streaming form byte-for-byte stable is
// what lets cursors issued before this refactor keep working. The caller
// must hold index.mu.
func (index *Index) hashRangeBlockContent(from, to int64) []byte {
	h := sha256.New()
	for height := from; height <= to; height++ {
		writeBlockContent(h, index.Blocks[height])
	}
	return h.Sum(nil)
}
