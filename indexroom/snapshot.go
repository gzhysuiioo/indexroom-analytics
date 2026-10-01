package indexroom

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// snapshotVersion is the only snapshot format version this build writes and
// accepts.
const snapshotVersion = 1

// snapshotFile is the offline representation of one complete main-chain
// state. Blocks are ordered by ascending height and never contain reorged
// away blocks.
type snapshotFile struct {
	Version int64           `json:"version"`
	Tip     int64           `json:"tip"`
	Blocks  []snapshotBlock `json:"blocks"`
}

// snapshotBlock is one block in a snapshot, with its transactions in their
// original chain order.
type snapshotBlock struct {
	Height int64    `json:"height"`
	Hash   string   `json:"hash"`
	Parent string   `json:"parent"`
	Txs    []string `json:"txs"`
}

// ExportSnapshot writes the current main chain to w as a version-1 JSON
// snapshot. The chain is copied in one critical section, so the output
// always reflects one complete state even when appends or reorgs happen
// concurrently; slow writes never block the index and the index is never
// modified. The same main chain always produces byte-identical output.
//
// A write failure is reported and leaves the index untouched.
func (index *Index) ExportSnapshot(w io.Writer) error {
	index.mu.Lock()
	blocks := make([]snapshotBlock, 0, len(index.Blocks))
	for height := int64(1); height <= index.Tip; height++ {
		block := index.Blocks[height]
		// make+copy guarantees a non-nil slice, so a chain without
		// transactions is exported as "txs":[] rather than null.
		txs := make([]string, len(block.Txs))
		copy(txs, block.Txs)
		blocks = append(blocks, snapshotBlock{
			Height: block.Height,
			Hash:   block.Hash,
			Parent: block.Parent,
			Txs:    txs,
		})
	}
	tip := index.Tip
	index.mu.Unlock()

	snap := snapshotFile{Version: snapshotVersion, Tip: tip, Blocks: blocks}
	if err := json.NewEncoder(w).Encode(snap); err != nil {
		return fmt.Errorf("indexroom: snapshot export failed: %w", err)
	}
	return nil
}

// RestoreSnapshot reads a snapshot from r and replaces the index's main
// chain with exactly the snapshot contents. The input must be one complete
// snapshot object with nothing but whitespace after it. The snapshot is
// parsed and validated in full before the index is touched, so any read
// failure or invalid input leaves the current main chain — and every
// previously issued query cursor — fully intact.
//
// On success the index holds exactly the snapshot chain: heights and hashes
// absent from the snapshot do not linger. The per-instance cursor key is
// retained, so cursors issued against this index keep working whenever the
// pinned range is unchanged; cursors from other indexes remain rejected.
// The index keeps accepting appends, reorgs, and queries afterwards.
func (index *Index) RestoreSnapshot(r io.Reader) error {
	snap, err := readSnapshot(r)
	if err != nil {
		return err
	}

	index.mu.Lock()
	defer index.mu.Unlock()

	blocks := make(map[int64]Block, len(snap.Blocks))
	byHash := make(map[string]int64, len(snap.Blocks))
	for _, sb := range snap.Blocks {
		txs := make([]string, len(sb.Txs))
		copy(txs, sb.Txs)
		blocks[sb.Height] = Block{
			Height: sb.Height,
			Hash:   sb.Hash,
			Parent: sb.Parent,
			Txs:    txs,
		}
		byHash[sb.Hash] = sb.Height
	}
	index.Blocks = blocks
	index.ByHash = byHash
	index.Tip = snap.Tip
	return nil
}

// readSnapshot parses and strictly validates one snapshot object from r.
// It never touches the index.
func readSnapshot(r io.Reader) (*snapshotFile, error) {
	dec := json.NewDecoder(r)
	snap, err := decodeSnapshotObject(dec)
	if err != nil {
		return nil, snapshotError(err)
	}
	if err := validateSnapshot(snap); err != nil {
		return nil, err
	}
	// Nothing but whitespace may follow the snapshot object.
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != nil {
		if errors.Is(err, io.EOF) {
			return snap, nil
		}
		return nil, snapshotError(err)
	}
	return nil, errInvalid("invalid snapshot: trailing data after the snapshot object")
}

// validateSnapshot checks the parsed snapshot against the chain invariants.
func validateSnapshot(snap *snapshotFile) error {
	if snap.Version != snapshotVersion {
		return errInvalid(fmt.Sprintf(
			"invalid snapshot: unsupported version %d (want %d)", snap.Version, snapshotVersion))
	}
	if snap.Tip < 0 {
		return errInvalid("invalid snapshot: tip must not be negative")
	}
	if len(snap.Blocks) == 0 {
		if snap.Tip != 0 {
			return errInvalid(fmt.Sprintf(
				"invalid snapshot: tip %d does not match the last block height 0", snap.Tip))
		}
		return nil
	}
	if snap.Tip != snap.Blocks[len(snap.Blocks)-1].Height {
		return errInvalid(fmt.Sprintf(
			"invalid snapshot: tip %d does not match the last block height %d",
			snap.Tip, snap.Blocks[len(snap.Blocks)-1].Height))
	}
	seen := make(map[string]struct{}, len(snap.Blocks))
	for i, block := range snap.Blocks {
		wantHeight := int64(i + 1)
		if block.Height != wantHeight {
			return errInvalid(fmt.Sprintf(
				"invalid snapshot: block at index %d has height %d; heights must start at 1 and be consecutive",
				i, block.Height))
		}
		if block.Hash == "" {
			return errInvalid(fmt.Sprintf(
				"invalid snapshot: block at height %d has an empty hash", wantHeight))
		}
		if _, dup := seen[block.Hash]; dup {
			return errInvalid(fmt.Sprintf(
				"invalid snapshot: duplicate block hash %q at height %d", block.Hash, wantHeight))
		}
		seen[block.Hash] = struct{}{}
		if i > 0 && block.Parent != snap.Blocks[i-1].Hash {
			return errInvalid(fmt.Sprintf(
				"invalid snapshot: block at height %d has parent %q; expected %q",
				wantHeight, block.Parent, snap.Blocks[i-1].Hash))
		}
	}
	return nil
}

// decodeSnapshotObject reads the top-level snapshot object, rejecting
// unknown and duplicate fields.
func decodeSnapshotObject(dec *json.Decoder) (*snapshotFile, error) {
	var snap snapshotFile
	seen := make(map[string]bool)
	err := decodeObject(dec, func(key string, dec *json.Decoder) error {
		seen[key] = true
		switch key {
		case "version":
			if err := dec.Decode(&snap.Version); err != nil {
				return errInvalid(fmt.Sprintf("invalid snapshot: field \"version\": %v", err))
			}
		case "tip":
			if err := dec.Decode(&snap.Tip); err != nil {
				return errInvalid(fmt.Sprintf("invalid snapshot: field \"tip\": %v", err))
			}
		case "blocks":
			blocks, err := decodeBlocks(dec)
			if err != nil {
				return err
			}
			snap.Blocks = blocks
		default:
			return errInvalid(fmt.Sprintf("invalid snapshot: unknown field %q", key))
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	for _, field := range []string{"version", "tip", "blocks"} {
		if !seen[field] {
			return nil, errInvalid(fmt.Sprintf("invalid snapshot: missing required field %q", field))
		}
	}
	return &snap, nil
}

// decodeBlocks reads the blocks array, one strict object per block.
func decodeBlocks(dec *json.Decoder) ([]snapshotBlock, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return nil, errInvalid("invalid snapshot: field \"blocks\" must be an array")
	}
	blocks := []snapshotBlock{}
	for dec.More() {
		var block snapshotBlock
		seen := make(map[string]bool)
		err := decodeObject(dec, func(key string, dec *json.Decoder) error {
			seen[key] = true
			switch key {
			case "height":
				if err := dec.Decode(&block.Height); err != nil {
					return errInvalid(fmt.Sprintf("invalid snapshot: field \"height\": %v", err))
				}
			case "hash":
				if err := dec.Decode(&block.Hash); err != nil {
					return errInvalid(fmt.Sprintf("invalid snapshot: field \"hash\": %v", err))
				}
			case "parent":
				if err := dec.Decode(&block.Parent); err != nil {
					return errInvalid(fmt.Sprintf("invalid snapshot: field \"parent\": %v", err))
				}
			case "txs":
				txs, err := decodeTxs(dec)
				if err != nil {
					return err
				}
				block.Txs = txs
			default:
				return errInvalid(fmt.Sprintf("invalid snapshot: unknown field %q in block", key))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
		for _, field := range []string{"height", "hash", "parent", "txs"} {
			if !seen[field] {
				return nil, errInvalid(fmt.Sprintf("invalid snapshot: block is missing required field %q", field))
			}
		}
		blocks = append(blocks, block)
	}
	tok, err = dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != ']' {
		return nil, errInvalid("invalid snapshot: malformed blocks array")
	}
	return blocks, nil
}

// decodeTxs reads a transaction identifier array; every element must be a
// string, and duplicates and empty strings are preserved as-is.
func decodeTxs(dec *json.Decoder) ([]string, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return nil, errInvalid("invalid snapshot: field \"txs\" must be an array")
	}
	txs := []string{}
	for dec.More() {
		var tx string
		if err := dec.Decode(&tx); err != nil {
			return nil, errInvalid(fmt.Sprintf("invalid snapshot: txs must contain only strings: %v", err))
		}
		txs = append(txs, tx)
	}
	tok, err = dec.Token()
	if err != nil {
		return nil, err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != ']' {
		return nil, errInvalid("invalid snapshot: malformed txs array")
	}
	return txs, nil
}

// decodeObject reads one JSON object, invoking field for every key in order.
// Duplicate keys are rejected. The field callback must consume the value.
func decodeObject(dec *json.Decoder, field func(key string, dec *json.Decoder) error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return errInvalid("invalid snapshot: expected a JSON object")
	}
	seen := make(map[string]bool)
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return errInvalid("invalid snapshot: expected a field name")
		}
		if seen[key] {
			return errInvalid(fmt.Sprintf("invalid snapshot: duplicate field %q", key))
		}
		seen[key] = true
		if err := field(key, dec); err != nil {
			return err
		}
	}
	tok, err = dec.Token()
	if err != nil {
		return err
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '}' {
		return errInvalid("invalid snapshot: malformed object")
	}
	return nil
}

// snapshotError maps decoder failures to specific rejections while
// preserving genuine I/O errors from the underlying reader.
func snapshotError(err error) error {
	if _, ok := err.(errInvalid); ok {
		return err
	}
	if errors.Is(err, io.EOF) {
		return errInvalid("invalid snapshot: unexpected end of input")
	}
	var syntax *json.SyntaxError
	var unmarshal *json.UnmarshalTypeError
	if errors.As(err, &syntax) || errors.As(err, &unmarshal) {
		return errInvalid(fmt.Sprintf("invalid snapshot: %v", err))
	}
	return fmt.Errorf("indexroom: snapshot read failed: %w", err)
}
