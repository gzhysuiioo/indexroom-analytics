package indexroom

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/gob"
	"errors"
	"sort"
)

// ErrTxQueryDataChanged indicates that the chain state covered by a query
// cursor no longer matches the state the cursor was issued against: a block
// in the fixed height range was replaced, removed, or its ordered
// transactions changed. The caller must restart the query from the first
// page; continuing with this cursor can never succeed.
var ErrTxQueryDataChanged = errors.New("tx query data changed: blocks in the fixed range differ from the first query")

// TxRecord is one transaction occurrence at a specific block position.
type TxRecord struct {
	// Height of the block containing the transaction.
	Height int64
	// BlockHash is the hash of that block.
	BlockHash string
	// TxID is the transaction identifier as stored on the block.
	TxID string
	// Position is the zero-based index of the transaction within the block.
	Position int
}

// TxQuery selects a range of blocks and a set of transaction identifiers.
type TxQuery struct {
	// StartHeight is the inclusive lower bound. Zero means height 1.
	StartHeight int64
	// EndHeight is the inclusive upper bound. Zero means the chain tip at
	// the time of the first query; the bound is then fixed for the whole
	// query. An end above the tip is capped to the tip.
	EndHeight int64
	// TxIDs restricts results to these identifiers. An empty or nil set
	// matches every transaction. Order and duplicates do not affect the
	// result; identifiers are compared as exact strings.
	TxIDs []string
	// PageSize is the maximum number of records per page. Zero means 100;
	// the valid range is 1..1000.
	PageSize int
}

// TxPage is one page of query results together with whole-range statistics
// and a continuation cursor.
type TxPage struct {
	// Records are the occurrences for this page, ordered by height then
	// position within the block.
	Records []TxRecord
	// TotalMatches is the number of transaction occurrences in the fixed
	// range, across all pages. It does not change when paging.
	TotalMatches int
	// MatchedBlocks is the number of blocks in the fixed range that contain
	// at least one matching transaction. It does not change when paging.
	MatchedBlocks int
	// UpperBound is the fixed upper height bound of the range: the end
	// requested on the first query, capped to the tip seen then.
	UpperBound int64
	// Cursor continues the query. Empty when there is no next page.
	Cursor string
}

// QueryTxs returns one page of transaction occurrences matching q.
//
// An empty cursor starts a new query; the first page fixes the height range
// and the upper bound for all subsequent pages. A non-empty cursor continues
// a previous query and must be paired with the same height range and
// transaction filter (order and duplicates in the filter set do not matter).
//
// All records, statistics and cursors for one call reflect a single chain
// state, so concurrent appends or reorgs cannot mix states. A continuation
// whose fixed range no longer matches the first query fails with
// ErrTxQueryDataChanged; a corrupt, foreign, or inconsistent cursor fails
// with a parameter error. Neither kind of failure alters the index.
func (index *Index) QueryTxs(q TxQuery, cursor string) (*TxPage, error) {
	index.mu.Lock()
	defer index.mu.Unlock()
	index.ensureHMACKeyLocked()

	start := q.StartHeight
	if start == 0 {
		start = 1
	}
	if start < 1 {
		return nil, errInvalid("start height must be positive")
	}
	end := q.EndHeight
	if end < 0 {
		return nil, errInvalid("end height must be positive")
	}
	pageSize := q.PageSize
	if pageSize == 0 {
		pageSize = 100
	}
	if pageSize < 1 || pageSize > 1000 {
		return nil, errInvalid("page size must be between 1 and 1000")
	}
	ids := canonicalTxIDs(q.TxIDs)

	if cursor == "" {
		return index.firstTxPageLocked(q, start, end, pageSize, ids)
	}
	return index.continueTxPageLocked(q, cursor, start, end, pageSize, ids)
}

func (index *Index) firstTxPageLocked(q TxQuery, start, endReq int64, pageSize int, ids []string) (*TxPage, error) {
	// When both ends are given explicitly, the end must not precede the start.
	if q.StartHeight != 0 && q.EndHeight != 0 && q.EndHeight < q.StartHeight {
		return nil, errInvalid("end height must not be less than start")
	}
	tip := index.Tip
	end := endReq
	if end == 0 {
		end = tip
	}
	if end > tip {
		end = tip
	}
	if end < start {
		// Empty index or a start above the tip: successful empty result.
		return &TxPage{
			Records:       []TxRecord{},
			TotalMatches:  0,
			MatchedBlocks: 0,
			UpperBound:    end,
			Cursor:        "",
		}, nil
	}

	fp := index.rangeFingerprintLocked(start, end)
	records, total, blocks := index.scanTxRangeLocked(start, end, ids)
	page, nextHeight, nextPos, hasNext := txPageAfter(records, pageSize, 0, -1)
	var nextCursor string
	if hasNext {
		nextCursor = index.encodeTxCursorLocked(txCursorData{
			Version:     txCursorVersion,
			Start:       start,
			End:         end,
			GivenEnd:    q.EndHeight,
			IDs:         ids,
			AfterHeight: nextHeight,
			AfterPos:    nextPos,
			FP:          fp,
		})
	}
	return &TxPage{
		Records:       page,
		TotalMatches:  total,
		MatchedBlocks: blocks,
		UpperBound:    end,
		Cursor:        nextCursor,
	}, nil
}

func (index *Index) continueTxPageLocked(q TxQuery, cursorStr string, start, endReq int64, pageSize int, ids []string) (*TxPage, error) {
	cur, err := index.decodeTxCursorLocked(cursorStr)
	if err != nil {
		return nil, errInvalid("invalid query cursor: " + err.Error())
	}
	if cur.Version != txCursorVersion {
		return nil, errInvalid("unsupported query cursor version")
	}
	if cur.Start < 1 || cur.End < cur.Start {
		return nil, errInvalid("query cursor covers an invalid height range")
	}
	if cur.AfterHeight < 0 || cur.AfterPos < -1 {
		return nil, errInvalid("query cursor position is invalid")
	}
	if start != cur.Start {
		return nil, errInvalid("start height does not match the first query")
	}
	end := endReq
	if end == 0 {
		end = cur.End
	}
	// The continuation must reproduce the first query's end condition: the
	// same explicit end, the fixed upper bound, or zero (which defaults to
	// the fixed range). A different explicit end is a different condition.
	if end != cur.End && end != cur.GivenEnd {
		return nil, errInvalid("end height does not match the first query")
	}
	if !txIDsEqual(ids, cur.IDs) {
		return nil, errInvalid("transaction filter does not match the first query")
	}

	fp := index.rangeFingerprintLocked(cur.Start, cur.End)
	if !bytes.Equal(fp, cur.FP) {
		return nil, ErrTxQueryDataChanged
	}

	records, total, blocks := index.scanTxRangeLocked(cur.Start, cur.End, ids)
	page, nextHeight, nextPos, hasNext := txPageAfter(records, pageSize, cur.AfterHeight, cur.AfterPos)
	var nextCursor string
	if hasNext {
		nextCursor = index.encodeTxCursorLocked(txCursorData{
			Version:     txCursorVersion,
			Start:       cur.Start,
			End:         cur.End,
			GivenEnd:    cur.GivenEnd,
			IDs:         cur.IDs,
			AfterHeight: nextHeight,
			AfterPos:    nextPos,
			FP:          fp,
		})
	}
	return &TxPage{
		Records:       page,
		TotalMatches:  total,
		MatchedBlocks: blocks,
		UpperBound:    cur.End,
		Cursor:        nextCursor,
	}, nil
}

// ensureHMACKeyLocked lazily creates a per-instance signing key so cursors
// minted by one index cannot be accepted by another. Callers hold index.mu.
func (index *Index) ensureHMACKeyLocked() {
	if index.hmacKey == nil {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic("indexroom: crypto/rand failed: " + err.Error())
		}
		index.hmacKey = key
	}
}

// canonicalTxIDs returns the sorted, deduplicated set of identifiers. An
// empty set is represented by nil, which means "no filter".
func canonicalTxIDs(ids []string) []string {
	if len(ids) == 0 {
		return nil
	}
	out := make([]string, len(ids))
	copy(out, ids)
	sort.Strings(out)
	n := 0
	for i := range out {
		if i == 0 || out[i] != out[i-1] {
			out[n] = out[i]
			n++
		}
	}
	return out[:n]
}

func txIDsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// rangeFingerprintLocked hashes the complete state of the blocks in
// [start, end]: presence, height, hash, parent, and the ordered transaction
// identifiers. A change to any of these — including transactions changing
// while the hash stays the same — changes the fingerprint. Callers hold
// index.mu.
func (index *Index) rangeFingerprintLocked(start, end int64) []byte {
	h := sha256.New()
	var num [8]byte
	binary.BigEndian.PutUint64(num[:], uint64(start))
	h.Write(num[:])
	binary.BigEndian.PutUint64(num[:], uint64(end))
	h.Write(num[:])
	for height := start; height <= end; height++ {
		block, ok := index.Blocks[height]
		if !ok {
			// A missing block is part of the fingerprint: chain shortening
			// must invalidate the cursor.
			h.Write([]byte{0})
			continue
		}
		h.Write([]byte{1})
		binary.BigEndian.PutUint64(num[:], uint64(block.Height))
		h.Write(num[:])
		writeLengthPrefixed(h, block.Hash)
		writeLengthPrefixed(h, block.Parent)
		binary.BigEndian.PutUint64(num[:], uint64(len(block.Txs)))
		h.Write(num[:])
		for _, tx := range block.Txs {
			writeLengthPrefixed(h, tx)
		}
	}
	return h.Sum(nil)
}

func writeLengthPrefixed(h interface{ Write([]byte) (int, error) }, s string) {
	var num [8]byte
	binary.BigEndian.PutUint64(num[:], uint64(len(s)))
	h.Write(num[:])
	h.Write([]byte(s))
}

// scanTxRangeLocked collects every matching occurrence in [start, end] in
// (height, position) order, together with the total occurrence count and the
// number of blocks containing at least one match. Callers hold index.mu.
func (index *Index) scanTxRangeLocked(start, end int64, ids []string) (records []TxRecord, total, blocks int) {
	var idSet map[string]bool
	if len(ids) > 0 {
		idSet = make(map[string]bool, len(ids))
		for _, id := range ids {
			idSet[id] = true
		}
	}
	records = []TxRecord{}
	for height := start; height <= end; height++ {
		block, ok := index.Blocks[height]
		if !ok {
			continue
		}
		matched := false
		for pos, tx := range block.Txs {
			if idSet != nil && !idSet[tx] {
				continue
			}
			records = append(records, TxRecord{
				Height:    height,
				BlockHash: block.Hash,
				TxID:      tx,
				Position:  pos,
			})
			total++
			matched = true
		}
		if matched {
			blocks++
		}
	}
	return records, total, blocks
}

// txPageAfter returns the pageSize records strictly after (afterHeight,
// afterPos), plus the position of the last returned record for the next
// cursor. Records are ordered by (height, position).
func txPageAfter(records []TxRecord, pageSize int, afterHeight int64, afterPos int64) (page []TxRecord, nextHeight int64, nextPos int64, hasNext bool) {
	i := 0
	for i < len(records) {
		r := records[i]
		if r.Height > afterHeight || (r.Height == afterHeight && int64(r.Position) > afterPos) {
			break
		}
		i++
	}
	end := i + pageSize
	if end > len(records) {
		end = len(records)
	}
	page = make([]TxRecord, end-i)
	copy(page, records[i:end])
	hasNext = end < len(records)
	if hasNext {
		last := records[end-1]
		nextHeight = last.Height
		nextPos = int64(last.Position)
	}
	return page, nextHeight, nextPos, hasNext
}

const txCursorVersion = 1

type txCursorData struct {
	Version     int
	Start       int64
	End         int64
	GivenEnd    int64
	IDs         []string
	AfterHeight int64
	AfterPos    int64
	FP          []byte
}

func (index *Index) encodeTxCursorLocked(cur txCursorData) string {
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(cur); err != nil {
		panic("indexroom: failed to encode query cursor: " + err.Error())
	}
	mac := hmac.New(sha256.New, index.hmacKey)
	mac.Write(buf.Bytes())
	signed := append(buf.Bytes(), mac.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(signed)
}

func (index *Index) decodeTxCursorLocked(s string) (txCursorData, error) {
	signed, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return txCursorData{}, err
	}
	if len(signed) < sha256.Size {
		return txCursorData{}, errors.New("cursor is too short")
	}
	payload := signed[:len(signed)-sha256.Size]
	gotMAC := signed[len(signed)-sha256.Size:]
	mac := hmac.New(sha256.New, index.hmacKey)
	mac.Write(payload)
	if !hmac.Equal(gotMAC, mac.Sum(nil)) {
		return txCursorData{}, errors.New("cursor authentication failed")
	}
	var cur txCursorData
	if err := gob.NewDecoder(bytes.NewReader(payload)).Decode(&cur); err != nil {
		return txCursorData{}, err
	}
	return cur, nil
}
