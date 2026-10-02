package indexroom

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrInvalidSnapshot marks a rejected snapshot document: malformed or
// truncated JSON, trailing data after the object, an unknown version,
// missing, mistyped, unknown, or duplicated fields, a null in any field
// that must carry a value (version, tip, height, hash, parent, or a txs
// element), heights that do not ascend consecutively from 1, an empty or
// duplicated hash, a broken parent link, a version-2 timestamp that is
// missing or neither null nor a non-negative integer, or a tip that
// disagrees with the block list. A rejected restore never changes the index.
var ErrInvalidSnapshot = errors.New("indexroom: invalid snapshot")

const (
	// snapshotVersion1 is the timestamp-less layout. It stays readable
	// indefinitely; its blocks restore with missing timestamps.
	snapshotVersion1 = 1
	// snapshotVersion2 adds a required timestamp field to every block:
	// either a non-negative Unix-seconds integer or null for missing time.
	snapshotVersion2 = 2
)

// nullJSON is the literal JSON null token.
const nullJSON = "null"

// snapshotDoc is the version-1 wire form of an exported main chain.
type snapshotDoc struct {
	Version int64           `json:"version"`
	Tip     int64           `json:"tip"`
	Blocks  []snapshotBlock `json:"blocks"`
}

// snapshotBlock is the version-1 wire form of one block.
type snapshotBlock struct {
	Height int64    `json:"height"`
	Hash   string   `json:"hash"`
	Parent string   `json:"parent"`
	Txs    []string `json:"txs"`
}

// snapshotDocV2 is the timestamp-bearing wire form.
type snapshotDocV2 struct {
	Version int64             `json:"version"`
	Tip     int64             `json:"tip"`
	Blocks  []snapshotBlockV2 `json:"blocks"`
}

// snapshotBlockV2 is the version-2 wire form of one block. A nil Timestamp
// serializes as null.
type snapshotBlockV2 struct {
	Height    int64    `json:"height"`
	Hash      string   `json:"hash"`
	Parent    string   `json:"parent"`
	Txs       []string `json:"txs"`
	Timestamp *int64   `json:"timestamp"`
}

// Export writes the current main chain to w as a JSON snapshot: a single
// object with version, tip, and the blocks ordered by ascending height, each
// carrying its transactions in order. The same main chain always produces
// the same bytes, regardless of the reorg, restore, or query history that
// led to it. Only blocks in effect at export time appear; nothing the index
// once held and later dropped is included.
//
// A chain whose blocks all lack timestamps keeps the original version-1
// layout, byte for byte. As soon as any block carries a timestamp the
// document is version 2 and every block includes a timestamp field: a
// non-negative integer or null for a missing timestamp.
//
// Export observes one complete chain state and never modifies the index.
// The chain is copied under the lock and serialized afterwards, so queries
// and appends proceed while the stream is still being written. A write
// failure is reported and leaves the index untouched.
func (index *Index) Export(w io.Writer) error {
	index.mu.Lock()
	version := int64(snapshotVersion1)
	for height := int64(1); height <= index.Tip; height++ {
		if index.Blocks[height].Time != nil {
			version = snapshotVersion2
			break
		}
	}
	var raw []byte
	var err error
	if version == snapshotVersion1 {
		doc := snapshotDoc{
			Version: snapshotVersion1,
			Tip:     index.Tip,
			Blocks:  make([]snapshotBlock, 0, int(index.Tip)),
		}
		for height := int64(1); height <= index.Tip; height++ {
			block := index.Blocks[height]
			txs := make([]string, len(block.Txs))
			copy(txs, block.Txs)
			doc.Blocks = append(doc.Blocks, snapshotBlock{
				Height: block.Height,
				Hash:   block.Hash,
				Parent: block.Parent,
				Txs:    txs,
			})
		}
		index.mu.Unlock()
		raw, err = json.Marshal(doc)
	} else {
		doc := snapshotDocV2{
			Version: snapshotVersion2,
			Tip:     index.Tip,
			Blocks:  make([]snapshotBlockV2, 0, int(index.Tip)),
		}
		for height := int64(1); height <= index.Tip; height++ {
			block := index.Blocks[height]
			txs := make([]string, len(block.Txs))
			copy(txs, block.Txs)
			entry := snapshotBlockV2{
				Height: block.Height,
				Hash:   block.Hash,
				Parent: block.Parent,
				Txs:    txs,
			}
			if block.Time != nil {
				t := *block.Time
				entry.Timestamp = &t
			}
			doc.Blocks = append(doc.Blocks, entry)
		}
		index.mu.Unlock()
		raw, err = json.Marshal(doc)
	}
	if err != nil {
		panic(err) // snapshot docs only contain marshalable fields
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

	// Top-level fields are captured raw so blocks can be parsed with the
	// strict schema matching the declared version, whatever the field order.
	var (
		version, tip         int64
		rawBlocks            json.RawMessage
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
			v, err := decodeSnapshotIntField(dec, key)
			if err != nil {
				return 0, nil, err
			}
			version = v
			haveVersion = true
		case "tip":
			v, err := decodeSnapshotIntField(dec, key)
			if err != nil {
				return 0, nil, err
			}
			tip = v
			haveTip = true
		case "blocks":
			if err := dec.Decode(&rawBlocks); err != nil {
				return 0, nil, classifySnapshotErr(fmt.Errorf("field %q: %w", key, err))
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

	blockDec := json.NewDecoder(bytes.NewReader(rawBlocks))
	blocks, err := parseSnapshotBlocks(blockDec, version)
	if err != nil {
		return 0, nil, err
	}
	if _, err := blockDec.Token(); err != io.EOF {
		if err == nil {
			return 0, nil, invalidSnapshot("trailing data after the blocks array")
		}
		return 0, nil, classifySnapshotErr(err)
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
// hashes, parent links, and (for version 2) timestamps as it goes.
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
		var block Block
		if version == snapshotVersion2 {
			block, err = parseSnapshotBlockV2(dec)
		} else {
			block, err = parseSnapshotBlock(dec)
		}
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

// parseSnapshotBlock reads one version-1 block object, requiring exactly the
// fields height, hash, parent, and txs.
func parseSnapshotBlock(dec *json.Decoder) (Block, error) {
	var block Block
	tok, err := dec.Token()
	if err != nil {
		return block, classifySnapshotErr(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return block, invalidSnapshot("block must be a JSON object")
	}
	var haveHeight, haveHash, haveParent, haveTxs bool
	var txNulls []int
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
			v, err := decodeSnapshotIntField(dec, key)
			if err != nil {
				return block, err
			}
			block.Height = v
			haveHeight = true
		case "hash":
			v, err := decodeSnapshotStringField(dec, key)
			if err != nil {
				return block, err
			}
			block.Hash = v
			haveHash = true
		case "parent":
			v, err := decodeSnapshotStringField(dec, key)
			if err != nil {
				return block, err
			}
			block.Parent = v
			haveParent = true
		case "txs":
			txs, nulls, err := parseSnapshotTxs(dec)
			if err != nil {
				return block, err
			}
			block.Txs = txs
			txNulls = nulls
			haveTxs = true
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
	// Reject null tx elements now that the block height is known, so the
	// error can pinpoint both the block and the 0-based position.
	if len(txNulls) > 0 {
		return block, invalidSnapshot("txs element at block height %d, position %d must not be null", block.Height, txNulls[0])
	}
	return block, nil
}

// parseSnapshotBlockV2 reads one version-2 block object, requiring exactly
// the fields height, hash, parent, txs, and timestamp. Timestamp must be
// null or a non-negative integer; the block returns a nil time for null.
func parseSnapshotBlockV2(dec *json.Decoder) (Block, error) {
	var block Block
	tok, err := dec.Token()
	if err != nil {
		return block, classifySnapshotErr(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return block, invalidSnapshot("block must be a JSON object")
	}
	var haveHeight, haveHash, haveParent, haveTxs, haveTimestamp bool
	var txNulls []int
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
			v, err := decodeSnapshotIntField(dec, key)
			if err != nil {
				return block, err
			}
			block.Height = v
			haveHeight = true
		case "hash":
			v, err := decodeSnapshotStringField(dec, key)
			if err != nil {
				return block, err
			}
			block.Hash = v
			haveHash = true
		case "parent":
			v, err := decodeSnapshotStringField(dec, key)
			if err != nil {
				return block, err
			}
			block.Parent = v
			haveParent = true
		case "txs":
			txs, nulls, err := parseSnapshotTxs(dec)
			if err != nil {
				return block, err
			}
			block.Txs = txs
			txNulls = nulls
			haveTxs = true
		case "timestamp":
			t, err := parseSnapshotTimestamp(dec)
			if err != nil {
				return block, err
			}
			block.Time = t
			haveTimestamp = true
		default:
			return block, invalidSnapshot("unknown block field %q", key)
		}
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return block, classifySnapshotErr(err)
	}
	for field, present := range map[string]bool{
		"height":    haveHeight,
		"hash":      haveHash,
		"parent":    haveParent,
		"txs":       haveTxs,
		"timestamp": haveTimestamp,
	} {
		if !present {
			return block, invalidSnapshot("block is missing field %q", field)
		}
	}
	// Reject null tx elements now that the block height is known, so the
	// error can pinpoint both the block and the 0-based position.
	if len(txNulls) > 0 {
		return block, invalidSnapshot("txs element at block height %d, position %d must not be null", block.Height, txNulls[0])
	}
	return block, nil
}

// parseSnapshotTimestamp reads one timestamp value: null for a missing
// timestamp, or a non-negative integer. Zero is stored as a real timestamp,
// distinct from null.
func parseSnapshotTimestamp(dec *json.Decoder) (*int64, error) {
	var t *int64
	if err := dec.Decode(&t); err != nil {
		return nil, classifySnapshotErr(fmt.Errorf("field %q: %w", "timestamp", err))
	}
	if t != nil && *t < 0 {
		return nil, invalidSnapshot("timestamp must not be negative")
	}
	return t, nil
}

// parseSnapshotTxs reads one txs array, preserving duplicates and empty
// identifiers exactly as written. Explicit null elements are recorded by
// 0-based position so callers can reject them once the block height is known.
func parseSnapshotTxs(dec *json.Decoder) (txs []string, nulls []int, err error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, nil, classifySnapshotErr(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return nil, nil, invalidSnapshot("field %q must be an array of strings", "txs")
	}
	txs = []string{}
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, classifySnapshotErr(fmt.Errorf("txs element: %w", err))
		}
		if string(raw) == nullJSON {
			nulls = append(nulls, len(txs))
			txs = append(txs, "")
			continue
		}
		var tx string
		if err := json.Unmarshal(raw, &tx); err != nil {
			return nil, nil, classifySnapshotErr(fmt.Errorf("txs element: %w", err))
		}
		txs = append(txs, tx)
	}
	if _, err := dec.Token(); err != nil { // closing ']'
		return nil, nil, classifySnapshotErr(err)
	}
	return txs, nulls, nil
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

// decodeSnapshotIntField decodes an integer field, rejecting an explicit null
// that would otherwise silently become zero and look like a valid value.
func decodeSnapshotIntField(dec *json.Decoder, field string) (int64, error) {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return 0, classifySnapshotErr(fmt.Errorf("field %q: %w", field, err))
	}
	if string(raw) == nullJSON {
		return 0, invalidSnapshot("field %q must not be null", field)
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, classifySnapshotErr(fmt.Errorf("field %q: %w", field, err))
	}
	return v, nil
}

// decodeSnapshotStringField decodes a string field, rejecting an explicit null
// that would otherwise silently become the empty string.
func decodeSnapshotStringField(dec *json.Decoder, field string) (string, error) {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return "", classifySnapshotErr(fmt.Errorf("field %q: %w", field, err))
	}
	if string(raw) == nullJSON {
		return "", invalidSnapshot("field %q must not be null", field)
	}
	var v string
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", classifySnapshotErr(fmt.Errorf("field %q: %w", field, err))
	}
	return v, nil
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
