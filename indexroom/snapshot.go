package indexroom

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"unicode/utf8"
)

// ErrInvalidSnapshot marks a rejected snapshot document: malformed or
// truncated JSON, trailing data after the object, an unknown version,
// missing, mistyped, unknown, or duplicated fields, a null where version,
// tip, height, hash, parent, or a transaction identifier is required,
// heights that do not ascend consecutively from 1, an empty or duplicated
// hash, a broken parent link, a version-2 timestamp that is missing or
// neither null nor a non-negative integer, a hash, parent, or transaction
// identifier whose JSON string carries invalid UTF-8 bytes or unpaired or
// misordered Unicode surrogate escapes, or a tip that disagrees with the
// block list. A rejected restore never changes the index.
var ErrInvalidSnapshot = errors.New("indexroom: invalid snapshot")

const (
	// snapshotVersion1 is the timestamp-less layout. It stays readable
	// indefinitely; its blocks restore with missing timestamps.
	snapshotVersion1 = 1
	// snapshotVersion2 adds a required timestamp field to every block:
	// either a non-negative Unix-seconds integer or null for missing time.
	snapshotVersion2 = 2
)

// snapshotDoc is the wire form of an exported main chain. Both layout
// versions share one shape: Version selects whether every block carries its
// Timestamp field.
type snapshotDoc struct {
	Version int64           `json:"version"`
	Tip     int64           `json:"tip"`
	Blocks  []snapshotBlock `json:"blocks"`
}

// snapshotBlock is the wire form of one block, shared by both versions.
// Height, hash, parent, and the transaction list follow the same rules in
// either layout. The timestamp is version 2 only and carried as a raw
// literal: nil is omitted altogether (version 1), "null" marks a missing
// time, and integer bytes render the Unix-seconds value (version 2). It
// stays the final field in every layout.
type snapshotBlock struct {
	Height    int64           `json:"height"`
	Hash      string          `json:"hash"`
	Parent    string          `json:"parent"`
	Txs       []string        `json:"txs"`
	Timestamp json.RawMessage `json:"timestamp,omitempty"`
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
	doc := index.snapshotChain()
	raw, err := json.Marshal(doc)
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

// snapshotChain copies the current main chain into the export wire
// form: blocks by ascending height, each with its transactions copied in
// original order. This is the single place that turns stored blocks into the
// shared document fields, so both layout versions keep identical rules for
// height, hash, parent, and the transaction list; the only version-dependent
// field is the timestamp literal.
//
// The version is selected from the same chain copy. Any block carrying a
// timestamp — even a zero — makes the document version 2, after which every
// block gets a timestamp field, null where its time is missing; a chain
// whose blocks all lack times stays version 1 with no timestamp field.
func (index *Index) snapshotChain() snapshotDoc {
	index.mu.Lock()
	defer index.mu.Unlock()
	version := int64(snapshotVersion1)
	for height := int64(1); height <= index.Tip; height++ {
		if index.Blocks[height].Time != nil {
			version = snapshotVersion2
			break
		}
	}
	doc := snapshotDoc{
		Version: version,
		Tip:     index.Tip,
		Blocks:  make([]snapshotBlock, 0, int(index.Tip)),
	}
	for height := int64(1); height <= index.Tip; height++ {
		block := index.Blocks[height]
		txs := make([]string, len(block.Txs))
		copy(txs, block.Txs)
		entry := snapshotBlock{
			Height: block.Height,
			Hash:   block.Hash,
			Parent: block.Parent,
			Txs:    txs,
		}
		if version == snapshotVersion2 {
			entry.Timestamp = snapshotTimestampLiteral(block.Time)
		}
		doc.Blocks = append(doc.Blocks, entry)
	}
	return doc
}

// snapshotMissingTimestamp is the raw JSON literal written for a block whose
// time is missing in a version-2 document.
var snapshotMissingTimestamp = json.RawMessage("null")

// snapshotTimestampLiteral renders a stored block time as its version-2
// timestamp literal: null for a missing time, otherwise its non-negative
// Unix-seconds value as a bare integer. Stored times are guaranteed
// non-negative by Append, Reorg, and Restore validation.
func snapshotTimestampLiteral(time *int64) json.RawMessage {
	if time == nil {
		return snapshotMissingTimestamp
	}
	return json.RawMessage(strconv.AppendInt(nil, *time, 10))
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
	// strict schema matching the declared version, whatever the field order,
	// and so null can be told apart from a zero value.
	var (
		rawVersion, rawTip   json.RawMessage
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
			if err := dec.Decode(&rawVersion); err != nil {
				return 0, nil, classifySnapshotErr(fmt.Errorf("field %q: %w", key, err))
			}
			haveVersion = true
		case "tip":
			if err := dec.Decode(&rawTip); err != nil {
				return 0, nil, classifySnapshotErr(fmt.Errorf("field %q: %w", key, err))
			}
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
	version, err := snapshotInt64(rawVersion, "version")
	if err != nil {
		return 0, nil, err
	}
	tip, err := snapshotInt64(rawTip, "tip")
	if err != nil {
		return 0, nil, err
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
	fields, err := readSnapshotBlockObject(dec, "height", "hash", "parent", "txs")
	if err != nil {
		return block, err
	}
	return finishSnapshotBlock(fields)
}

// parseSnapshotBlockV2 reads one version-2 block object, requiring exactly
// the fields height, hash, parent, txs, and timestamp. Timestamp must be
// null or a non-negative integer; the block returns a nil time for null.
func parseSnapshotBlockV2(dec *json.Decoder) (Block, error) {
	var block Block
	fields, err := readSnapshotBlockObject(dec, "height", "hash", "parent", "txs", "timestamp")
	if err != nil {
		return block, err
	}
	if _, ok := fields["timestamp"]; !ok {
		return block, invalidSnapshot("block is missing field %q", "timestamp")
	}
	block, err = finishSnapshotBlock(fields)
	if err != nil {
		return block, err
	}
	t, err := parseSnapshotTimestamp(fields["timestamp"])
	if err != nil {
		return Block{}, err
	}
	block.Time = t
	return block, nil
}

// readSnapshotBlockObject reads one block object and captures each known
// field's raw value. Fields are decoded only after the whole object is read,
// so a null can be rejected per field and a txs element error can name the
// block's height whatever order the fields arrive in.
func readSnapshotBlockObject(dec *json.Decoder, known ...string) (map[string]json.RawMessage, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, classifySnapshotErr(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '{' {
		return nil, invalidSnapshot("block must be a JSON object")
	}
	fields := map[string]json.RawMessage{}
	for dec.More() {
		key, err := snapshotKey(dec)
		if err != nil {
			return nil, err
		}
		if _, dup := fields[key]; dup {
			return nil, invalidSnapshot("duplicate block field %q", key)
		}
		if !slices.Contains(known, key) {
			return nil, invalidSnapshot("unknown block field %q", key)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, classifySnapshotErr(fmt.Errorf("field %q: %w", key, err))
		}
		fields[key] = raw
	}
	if _, err := dec.Token(); err != nil { // closing '}'
		return nil, classifySnapshotErr(err)
	}
	return fields, nil
}

// finishSnapshotBlock decodes the shared block fields from their raw values,
// requiring height, hash, parent, and txs and rejecting null for each.
func finishSnapshotBlock(fields map[string]json.RawMessage) (Block, error) {
	var block Block
	for _, field := range []string{"height", "hash", "parent", "txs"} {
		if _, ok := fields[field]; !ok {
			return block, invalidSnapshot("block is missing field %q", field)
		}
	}
	var err error
	if block.Height, err = snapshotInt64(fields["height"], "height"); err != nil {
		return block, err
	}
	if block.Hash, err = snapshotString(fields["hash"], "hash", block.Height); err != nil {
		return block, err
	}
	if block.Parent, err = snapshotString(fields["parent"], "parent", block.Height); err != nil {
		return block, err
	}
	txs, err := parseSnapshotTxs(json.NewDecoder(bytes.NewReader(fields["txs"])), block.Height)
	if err != nil {
		return block, err
	}
	block.Txs = txs
	return block, nil
}

// snapshotInt64 decodes one required integer field, rejecting null rather
// than letting it pass as zero.
func snapshotInt64(raw json.RawMessage, field string) (int64, error) {
	if isSnapshotNull(raw) {
		return 0, invalidSnapshot("field %q must not be null", field)
	}
	var v int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return 0, classifySnapshotErr(fmt.Errorf("field %q: %w", field, err))
	}
	return v, nil
}

// snapshotString decodes one required string field, rejecting null rather
// than letting it pass as the empty string. The raw literal is also checked
// for invalid UTF-8 bytes and unpaired or misordered \uXXXX surrogate
// escapes, which the decoder would otherwise silently rewrite to U+FFFD;
// height identifies the block the field belongs to in the error.
func snapshotString(raw json.RawMessage, field string, height int64) (string, error) {
	if isSnapshotNull(raw) {
		return "", invalidSnapshot("field %q must not be null", field)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", classifySnapshotErr(fmt.Errorf("field %q: %w", field, err))
	}
	if !validSnapshotString(raw) {
		return "", invalidSnapshot("block at height %d: field %q has invalid UTF-8 or unpaired surrogate escapes", height, field)
	}
	return s, nil
}

// isSnapshotNull reports whether a raw field value is the JSON null literal.
func isSnapshotNull(raw json.RawMessage) bool {
	return string(raw) == "null"
}

// validSnapshotString reports whether raw is a JSON string literal whose
// content is valid UTF-8 and whose \uXXXX escapes contain no unpaired or
// misordered surrogates. The JSON decoder accepts both defects silently,
// rewriting the bad parts to U+FFFD, which could merge distinct identifiers;
// the raw bytes are checked here before the decoded value is trusted. A
// genuine U+FFFD — written literally or as a \ufffd escape — is valid
// UTF-8 and passes.
func validSnapshotString(raw json.RawMessage) bool {
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return false
	}
	body := raw[1 : len(raw)-1]
	for i := 0; i < len(body); {
		b := body[i]
		switch {
		case b == '\\':
			if i+1 >= len(body) {
				return false
			}
			if body[i+1] != 'u' {
				i += 2 // \", \\, \/, \b, \f, \n, \r, \t
				continue
			}
			code, ok := snapshotHex4(body, i+2)
			if !ok {
				return false
			}
			i += 6
			switch {
			case code >= 0xD800 && code <= 0xDBFF:
				// A high surrogate is only valid as the first half of a
				// pair; the low half must follow immediately as an escape.
				if i+6 > len(body) || body[i] != '\\' || body[i+1] != 'u' {
					return false
				}
				low, ok := snapshotHex4(body, i+2)
				if !ok || low < 0xDC00 || low > 0xDFFF {
					return false
				}
				i += 6
			case code >= 0xDC00 && code <= 0xDFFF:
				return false // a low surrogate cannot appear on its own
			}
		case b < utf8.RuneSelf:
			i++
		default:
			// DecodeRune returns RuneError with size 1 for invalid
			// encoding; a genuine U+FFFD decodes with size 3.
			r, size := utf8.DecodeRune(body[i:])
			if r == utf8.RuneError && size <= 1 {
				return false
			}
			i += size
		}
	}
	return true
}

// snapshotHex4 reads four hexadecimal digits starting at b[i].
func snapshotHex4(b []byte, i int) (rune, bool) {
	if i+4 > len(b) {
		return 0, false
	}
	var v rune
	for _, c := range b[i : i+4] {
		var d byte
		switch {
		case '0' <= c && c <= '9':
			d = c - '0'
		case 'a' <= c && c <= 'f':
			d = c - 'a' + 10
		case 'A' <= c && c <= 'F':
			d = c - 'A' + 10
		default:
			return 0, false
		}
		v = v*16 + rune(d)
	}
	return v, true
}

// parseSnapshotTimestamp reads one timestamp value: null for a missing
// timestamp, or a non-negative integer. Zero is stored as a real timestamp,
// distinct from null.
func parseSnapshotTimestamp(raw json.RawMessage) (*int64, error) {
	var t *int64
	if err := json.Unmarshal(raw, &t); err != nil {
		return nil, classifySnapshotErr(fmt.Errorf("field %q: %w", "timestamp", err))
	}
	if t != nil && *t < 0 {
		return nil, invalidSnapshot("timestamp must not be negative")
	}
	return t, nil
}

// parseSnapshotTxs reads one txs array, preserving duplicates and empty
// identifiers exactly as written. A null element is rejected, naming the
// block's height and the element's zero-based position, and so is an
// element whose raw string carries invalid UTF-8 bytes or unpaired or
// misordered surrogate escapes.
func parseSnapshotTxs(dec *json.Decoder, height int64) ([]string, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, classifySnapshotErr(err)
	}
	if delim, ok := tok.(json.Delim); !ok || delim != '[' {
		return nil, invalidSnapshot("field %q must be an array of strings", "txs")
	}
	txs := []string{}
	for dec.More() {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, classifySnapshotErr(fmt.Errorf("txs element: %w", err))
		}
		if isSnapshotNull(raw) {
			return nil, invalidSnapshot("block at height %d: txs element %d must not be null", height, len(txs))
		}
		var tx string
		if err := json.Unmarshal(raw, &tx); err != nil {
			return nil, classifySnapshotErr(fmt.Errorf("txs element: %w", err))
		}
		if !validSnapshotString(raw) {
			return nil, invalidSnapshot("block at height %d: field %q element %d has invalid UTF-8 or unpaired surrogate escapes", height, "txs", len(txs))
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
