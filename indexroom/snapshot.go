package indexroom

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrInvalidSnapshot marks a rejected snapshot document: malformed or
// truncated JSON, trailing data after the object, an unknown version,
// missing, mistyped, unknown, or duplicated fields, heights that do not
// ascend consecutively from 1, an empty or duplicated hash, a broken parent
// link, or a tip that disagrees with the block list. A rejected restore
// never changes the index.
var ErrInvalidSnapshot = errors.New("indexroom: invalid snapshot")

// snapshotVersion1 is the original snapshot layout: blocks carry no
// timestamp field.
const snapshotVersion1 = 1

// snapshotVersion2 adds the required per-block timestamp: a non-negative
// Unix seconds integer, or null for a block without one.
const snapshotVersion2 = 2

// snapshotDoc is the wire form of an exported main chain. Blocks is either a
// []snapshotBlock (version 1) or a []snapshotBlockV2 (version 2).
type snapshotDoc struct {
	Version int64 `json:"version"`
	Tip     int64 `json:"tip"`
	Blocks  any   `json:"blocks"`
}

// snapshotBlock is the wire form of one block inside a version 1 snapshot.
type snapshotBlock struct {
	Height int64    `json:"height"`
	Hash   string   `json:"hash"`
	Parent string   `json:"parent"`
	Txs    []string `json:"txs"`
}

// snapshotBlockV2 is the wire form of one block inside a version 2 snapshot,
// adding the required timestamp field.
type snapshotBlockV2 struct {
	snapshotBlock
	// Timestamp is the block's Unix seconds; null means the block has none.
	Timestamp *int64 `json:"timestamp"`
}

// Export writes the current main chain to w as a JSON snapshot: a single
// object with version, tip, and the blocks ordered by ascending height, each
// carrying its transactions in order. A chain in which every block lacks a
// timestamp is written as version 1 with the original byte format; as soon
// as one block carries a known timestamp the chain is written as version 2,
// where every block has a required timestamp field holding a non-negative
// Unix seconds integer or null. The same main chain always produces the same
// bytes, regardless of the reorg, restore, or query history that led to it.
// Only blocks in effect at export time appear; nothing the index once held
// and later dropped is included.
//
// Export observes one complete chain state and never modifies the index.
// The chain is copied under the lock and serialized afterwards, so queries
// and appends proceed while the stream is still being written. A write
// failure is reported and leaves the index untouched.
func (index *Index) Export(w io.Writer) error {
	index.mu.Lock()
	version := int64(snapshotVersion1)
	for height := int64(1); height <= index.Tip; height++ {
		if index.Blocks[height].Timestamp != nil {
			version = snapshotVersion2
			break
		}
	}
	doc := snapshotDoc{
		Version: version,
		Tip:     index.Tip,
	}
	if version == snapshotVersion2 {
		blocks := make([]snapshotBlockV2, 0, int(index.Tip))
		for height := int64(1); height <= index.Tip; height++ {
			block := index.Blocks[height]
			txs := make([]string, len(block.Txs))
			copy(txs, block.Txs)
			blocks = append(blocks, snapshotBlockV2{
				snapshotBlock: snapshotBlock{
					Height: block.Height,
					Hash:   block.Hash,
					Parent: block.Parent,
					Txs:    txs,
				},
				Timestamp: block.Timestamp,
			})
		}
		doc.Blocks = blocks
	} else {
		blocks := make([]snapshotBlock, 0, int(index.Tip))
		for height := int64(1); height <= index.Tip; height++ {
			block := index.Blocks[height]
			txs := make([]string, len(block.Txs))
			copy(txs, block.Txs)
			blocks = append(blocks, snapshotBlock{
				Height: block.Height,
				Hash:   block.Hash,
				Parent: block.Parent,
				Txs:    txs,
			})
		}
		doc.Blocks = blocks
	}
	index.mu.Unlock()

	raw, err := json.Marshal(doc)
	if err != nil {
		panic(err) // snapshotDoc only contains marshalable fields
	}
	n, err := w.Write(raw)
	if err != nil {
		return fmt.Errorf("indexroom: export snapshot: %w", err)
	}
	if n != len(raw) {
		return fmt.Errorf("indexroom: export snapshot: %w", io.ErrShortWrite)
	}
	return nil
}

// Restore replaces the whole main chain with the snapshot read from r. The
// input must be exactly one snapshot object, optionally followed by
// whitespace; every block is validated — including heights ascending from 1,
// non-empty unique hashes, and parent linkage — before anything is applied.
// The first block's parent is exempt from linkage: it names the chain start
// and need not be indexed.
//
// The replacement takes effect as one complete operation: on success the
// index holds exactly the snapshot chain with no leftover heights or hashes,
// and on any failure — a read error or an invalid block discovered at the
// very end — the current chain and every previously issued cursor remain as
// they were. The stream is consumed and validated before the lock is taken,
// so the index stays queryable and appendable while the restore is reading.
//
// After a successful restore, cursors issued earlier by this same index keep
// working wherever the pinned range still matches and the tip still covers
// it; otherwise continuations fail with ErrQueryChanged as usual.
func (index *Index) Restore(r io.Reader) error {
	tip, blocks, err := parseSnapshot(r)
	if err != nil {
		return err
	}
	stored := make(map[int64]Block, len(blocks))
	byHash := make(map[string]int64, len(blocks))
	for _, block := range blocks {
		stored[block.Height] = block
		byHash[block.Hash] = block.Height
	}
	index.mu.Lock()
	index.Blocks = stored
	index.ByHash = byHash
	index.Tip = tip
	index.mu.Unlock()
	return nil
}

// parseSnapshot reads and fully validates one snapshot document. It returns
// the tip height and the validated blocks in ascending height order.
func parseSnapshot(r io.Reader) (int64, []Block, error) {
	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return 0, nil, classifySnapshotErr(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return 0, nil, invalidSnapshot("snapshot must be a single JSON object")
	}

	var (
		version, tip         int64
		blocks               []Block
		haveVersion, haveTip bool
		haveBlocks           bool
		seen                 = map[string]bool{}
	)
	for dec.More() {
		key, err := snapshotKey(dec)
		if err != nil {
			return 0, nil, err
		}
		if seen[key] {
			return 0, nil, invalidSnapshot("duplicate field %q", key)
		}
		seen[key] = true
		switch key {
		case "version":
			if err := dec.Decode(&version); err != nil {
				return 0, nil, classifySnapshotErr(fmt.Errorf("field %q: %w", key, err))
			}
			haveVersion = true
		case "tip":
			if err := dec.Decode(&tip); err != nil {
				return 0, nil, classifySnapshotErr(fmt.Errorf("field %q: %w", key, err))
			}
			haveTip = true
		case "blocks":
			blocks, err = parseSnapshotBlocks(dec, version)
			if err != nil {
				return 0, nil, err
			}
			haveBlocks = true
		default:
			return 0, nil, invalidSnapshot("unknown field %q", key)
		}
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return 0, nil, classifySnapshotErr(err)
	}
	// Only whitespace may follow the snapshot object.
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return 0, nil, invalidSnapshot("trailing data after the snapshot object")
		}
		return 0, nil, classifySnapshotErr(err)
	}

	if !haveVersion {
		return 0, nil, invalidSnapshot("missing field %q", "version")
	}
	if !haveTip {
		return 0, nil, invalidSnapshot("missing field %q", "tip")
	}
	if !haveBlocks {
		return 0, nil, invalidSnapshot("missing field %q", "blocks")
	}
	if version != snapshotVersion1 && version != snapshotVersion2 {
		return 0, nil, invalidSnapshot("unsupported version %d", version)
	}
	lastHeight := int64(0)
	if len(blocks) > 0 {
		lastHeight = blocks[len(blocks)-1].Height
	}
	if tip != lastHeight {
		return 0, nil, invalidSnapshot("tip %d does not match the last block height %d", tip, lastHeight)
	}
	return tip, blocks, nil
}

// parseSnapshotBlocks reads the blocks array value, validating heights,
// hashes, and parent links as it goes. Version 2 blocks must also carry a
// valid timestamp field.
func parseSnapshotBlocks(dec *json.Decoder, version int64) ([]Block, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, classifySnapshotErr(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return nil, invalidSnapshot("field %q must be an array", "blocks")
	}
	blocks := []Block{}
	seenHash := map[string]bool{}
	for dec.More() {
		block, err := parseSnapshotBlock(dec, version)
		if err != nil {
			return nil, err
		}
		want := int64(len(blocks) + 1)
		if block.Height != want {
			return nil, invalidSnapshot("block height %d out of order, expected %d", block.Height, want)
		}
		if block.Hash == "" {
			return nil, invalidSnapshot("block at height %d has an empty hash", block.Height)
		}
		if seenHash[block.Hash] {
			return nil, invalidSnapshot("duplicate hash %q", block.Hash)
		}
		seenHash[block.Hash] = true
		// The first block's parent names the chain start and is exempt from
		// linkage; every later block must point at its predecessor.
		if len(blocks) > 0 && block.Parent != blocks[len(blocks)-1].Hash {
			return nil, invalidSnapshot("block at height %d does not link to its parent", block.Height)
		}
		blocks = append(blocks, block)
	}
	if _, err := dec.Token(); err != nil { // closing ']'
		return nil, classifySnapshotErr(err)
	}
	return blocks, nil
}

// parseSnapshotBlock reads one block object, requiring exactly the fields
// height, hash, parent, and txs; version 2 additionally requires timestamp.
func parseSnapshotBlock(dec *json.Decoder, version int64) (Block, error) {
	var block Block
	tok, err := dec.Token()
	if err != nil {
		return block, classifySnapshotErr(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return block, invalidSnapshot("block must be a JSON object")
	}
	var haveHeight, haveHash, haveParent, haveTxs, haveTimestamp bool
	seen := map[string]bool{}
	for dec.More() {
		key, err := snapshotKey(dec)
		if err != nil {
			return block, err
		}
		if seen[key] {
			return block, invalidSnapshot("duplicate block field %q", key)
		}
		seen[key] = true
		switch key {
		case "height":
			if err := dec.Decode(&block.Height); err != nil {
				return block, classifySnapshotErr(fmt.Errorf("field %q: %w", key, err))
			}
			haveHeight = true
		case "hash":
			if err := dec.Decode(&block.Hash); err != nil {
				return block, classifySnapshotErr(fmt.Errorf("field %q: %w", key, err))
			}
			haveHash = true
		case "parent":
			if err := dec.Decode(&block.Parent); err != nil {
				return block, classifySnapshotErr(fmt.Errorf("field %q: %w", key, err))
			}
			haveParent = true
		case "txs":
			txs, err := parseSnapshotTxs(dec)
			if err != nil {
				return block, err
			}
			block.Txs = txs
			haveTxs = true
		case "timestamp":
			if version != snapshotVersion2 {
				return block, invalidSnapshot("field %q is only valid in version 2", "timestamp")
			}
			var ts *int64
			if err := dec.Decode(&ts); err != nil {
				return block, classifySnapshotErr(fmt.Errorf("field %q: %w", key, err))
			}
			if ts != nil && *ts < 0 {
				return block, invalidSnapshot("block timestamp must be a non-negative integer or null")
			}
			block.Timestamp = ts
			haveTimestamp = true
		default:
			return block, invalidSnapshot("unknown block field %q", key)
		}
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return block, classifySnapshotErr(err)
	}
	for field, present := range map[string]bool{
		"height": haveHeight,
		"hash":   haveHash,
		"parent": haveParent,
		"txs":    haveTxs,
	} {
		if !present {
			return block, invalidSnapshot("block is missing field %q", field)
		}
	}
	if version == snapshotVersion2 && !haveTimestamp {
		return block, invalidSnapshot("block is missing field %q", "timestamp")
	}
	return block, nil
}

// parseSnapshotTxs reads one txs array, preserving duplicates and empty
// identifiers exactly as written.
func parseSnapshotTxs(dec *json.Decoder) ([]string, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, classifySnapshotErr(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return nil, invalidSnapshot("field %q must be an array of strings", "txs")
	}
	txs := []string{}
	for dec.More() {
		var tx string
		if err := dec.Decode(&tx); err != nil {
			return nil, classifySnapshotErr(fmt.Errorf("txs element: %w", err))
		}
		txs = append(txs, tx)
	}
	if _, err := dec.Token(); err != nil { // closing ']'
		return nil, classifySnapshotErr(err)
	}
	return txs, nil
}

// snapshotKey reads the next object key.
func snapshotKey(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", classifySnapshotErr(err)
	}
	key, ok := tok.(string)
	if !ok {
		return "", invalidSnapshot("object key must be a string")
	}
	return key, nil
}

// invalidSnapshot builds an ErrInvalidSnapshot error with a specific reason.
func invalidSnapshot(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidSnapshot, fmt.Sprintf(format, args...))
}

// classifySnapshotErr maps decoder failures onto the snapshot error model:
// malformed JSON, wrong value types, and truncation are snapshot errors,
// while anything else is an underlying read failure reported as-is.
func classifySnapshotErr(err error) error {
	var syntaxErr *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.As(err, &syntaxErr), errors.As(err, &typeErr):
		return fmt.Errorf("%w: %v", ErrInvalidSnapshot, err)
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return fmt.Errorf("%w: unexpected end of input", ErrInvalidSnapshot)
	default:
		return fmt.Errorf("indexroom: read snapshot: %w", err)
	}
}
