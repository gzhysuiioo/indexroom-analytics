package indexroom

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync/atomic"
)

// ErrInvalidArgument marks a rejected query: out-of-range heights or page
// size, a malformed cursor, a cursor issued by another index instance, or
// continuation conditions that differ from the first page. It never reports
// chain data problems; use errors.Is to tell it apart from ErrQueryChanged.
var ErrInvalidArgument = errors.New("indexroom: invalid query argument")

// ErrQueryChanged marks a continuation whose pinned range no longer matches
// the first page: a block in the range was replaced (even under the same
// hash, and even if only its timestamp changed) or the chain was shortened
// below the pinned upper bound. No results are returned; the caller must
// restart from a first-page query.
var ErrQueryChanged = errors.New("indexroom: query data changed")

// queryTxsHookLocked is a test-only rendezvous invoked once per QueryTxs
// call while index.mu is held: for a first page after the tip-bound height has
// been resolved (and non-empty ranges only), and for a continuation only after
// every pinned-range check has passed, so the call is committed to a
// successful page against the chain state in effect. It is nil in production;
// reorg regression tests park a query here to force a concurrent Reorg to wait
// (or park the Reorg and make the query wait), making the "one complete
// main-chain view per page" observation deterministic instead of relying on
// scheduling luck.
var queryTxsHookLocked func(from, to int64, continuation bool)

const (
	// DefaultPageSize is the page length when TxQuery.PageSize is zero.
	DefaultPageSize = 100
	// MaxPageSize is the largest accepted TxQuery.PageSize.
	MaxPageSize = 1000
)

// TxQuery describes a transaction lookup against the main chain.
type TxQuery struct {
	// From and To bound the searched heights, both inclusive. A zero From
	// starts at height 1. A zero To pins the chain tip seen by the first
	// page; an explicit To above the tip is clamped to it. Explicit heights
	// must be positive, and an explicit To must not be below From.
	From int64
	To   int64
	// TxIDs restricts results to these identifiers, matched by exact string
	// comparison; an empty slice disables the filter. Duplicates and order
	// inside the slice are irrelevant.
	TxIDs []string
	// PageSize picks the page length, 1..MaxPageSize; zero means
	// DefaultPageSize. It may change between pages of the same query.
	PageSize int
	// Cursor continues an earlier query; empty asks for the first page. On
	// continuation the height range and TxIDs must equal the first page's.
	Cursor string
}

// TxHit is one transaction occurrence on the main chain. The same
// identifier appears once per occurrence, in different blocks or several
// times inside one block.
type TxHit struct {
	Height    int64
	BlockHash string
	TxID      string
	Position  int
}

// TxPage is one page of query results together with statistics over the
// whole filtered range. Hits are ordered by height, then by position inside
// the block.
type TxPage struct {
	Hits []TxHit
	// TotalMatches counts every matching occurrence in the pinned range; it
	// does not change between pages of the same query.
	TotalMatches int64
	// MatchedBlocks counts the blocks holding at least one match.
	MatchedBlocks int64
	// ToHeight is the upper bound pinned by the first page: the requested
	// To clamped to the chain tip seen at that moment.
	ToHeight int64
	// NextCursor continues the query; empty means this is the last page.
	NextCursor string
}

// QueryTxs searches the main chain for transaction occurrences matching the
// query. Every call observes one complete chain state: concurrent Append or
// Reorg calls never leak into a page, and the returned records are copies
// the caller may freely mutate. The query itself never modifies the index.
//
// The first page (empty Cursor) pins the filtered range and returns a
// continuation cursor while more results remain. Continuations reuse the
// pinned range — blocks appended above it are invisible to the query — and
// succeed as long as every block inside the range still matches the first
// page; otherwise they fail with ErrQueryChanged.
func (index *Index) QueryTxs(query TxQuery) (TxPage, error) {
	pageSize, err := normalizePageSize(query.PageSize)
	if err != nil {
		return TxPage{}, err
	}
	if query.Cursor != "" {
		return index.continueQuery(query, pageSize)
	}
	return index.firstQuery(query, pageSize)
}

func normalizePageSize(size int) (int, error) {
	if size == 0 {
		return DefaultPageSize, nil
	}
	if size < 0 || size > MaxPageSize {
		return 0, fmt.Errorf("%w: page size must be between 1 and %d", ErrInvalidArgument, MaxPageSize)
	}
	return size, nil
}

// normalizeRange validates the requested heights and resolves the zero From
// default. A zero To is left as-is; it is resolved against the chain tip
// while holding the lock.
func normalizeRange(from, to int64) (int64, int64, error) {
	if from < 0 {
		return 0, 0, fmt.Errorf("%w: from height must be positive", ErrInvalidArgument)
	}
	if to < 0 {
		return 0, 0, fmt.Errorf("%w: to height must be positive", ErrInvalidArgument)
	}
	if from == 0 {
		from = 1
	}
	if to != 0 && to < from {
		return 0, 0, fmt.Errorf("%w: to height is below from height", ErrInvalidArgument)
	}
	return from, to, nil
}

func (index *Index) firstQuery(query TxQuery, pageSize int) (TxPage, error) {
	from, to, err := normalizeRange(query.From, query.To)
	if err != nil {
		return TxPage{}, err
	}

	index.mu.Lock()
	defer index.mu.Unlock()

	if to == 0 || to > index.Tip {
		to = index.Tip
	}
	page := TxPage{Hits: []TxHit{}, ToHeight: to}
	if from > to {
		// Empty index or a start above the tip: a successful empty result.
		return page, nil
	}
	if queryTxsHookLocked != nil {
		queryTxsHookLocked(from, to, false)
	}
	hits, total, blocks := index.collectLocked(from, to, query.TxIDs, 0, pageSize)
	return index.makePageLocked(query, from, to, hits, total, blocks, 0), nil
}

func (index *Index) continueQuery(query TxQuery, pageSize int) (TxPage, error) {
	payload, err := index.decodeCursor(query.Cursor)
	if err != nil {
		return TxPage{}, err
	}
	from, _, err := normalizeRange(query.From, query.To)
	if err != nil {
		return TxPage{}, err
	}
	if from != payload.From {
		return TxPage{}, fmt.Errorf("%w: from height differs from the first page", ErrInvalidArgument)
	}
	if query.To != payload.ReqTo {
		return TxPage{}, fmt.Errorf("%w: to height differs from the first page", ErrInvalidArgument)
	}
	if got := hex.EncodeToString(hashTxSet(query.TxIDs)); got != payload.Set {
		return TxPage{}, fmt.Errorf("%w: transaction filter differs from the first page", ErrInvalidArgument)
	}

	index.mu.Lock()
	defer index.mu.Unlock()

	if index.Tip < payload.To {
		return TxPage{}, fmt.Errorf("%w: chain tip %d is below the pinned upper bound %d",
			ErrQueryChanged, index.Tip, payload.To)
	}
	if got := hex.EncodeToString(index.fingerprintLocked(payload.From, payload.To)); got != payload.FP {
		return TxPage{}, fmt.Errorf("%w: blocks in the pinned range differ from the first page", ErrQueryChanged)
	}
	if queryTxsHookLocked != nil {
		queryTxsHookLocked(payload.From, payload.To, true)
	}
	hits, total, blocks := index.collectLocked(payload.From, payload.To, query.TxIDs, payload.Off, pageSize)
	if payload.Off > total {
		return TxPage{}, fmt.Errorf("%w: cursor offset is beyond the pinned results", ErrQueryChanged)
	}
	return index.makePageLocked(query, payload.From, payload.To, hits, total, blocks, payload.Off), nil
}

// makePageLocked assembles one page from the retained window of hits and
// mints the continuation cursor when matches remain past it. The caller must
// hold index.mu.
func (index *Index) makePageLocked(query TxQuery, from, to int64, hits []TxHit, total, blocks int64, offset int64) TxPage {
	page := TxPage{
		Hits:          hits,
		TotalMatches:  total,
		MatchedBlocks: blocks,
		ToHeight:      to,
	}
	next := offset + int64(len(hits))
	if next < total {
		page.NextCursor = index.encodeCursor(cursorPayload{
			V:     1,
			From:  from,
			ReqTo: query.To,
			To:    to,
			Set:   hex.EncodeToString(hashTxSet(query.TxIDs)),
			FP:    hex.EncodeToString(index.fingerprintLocked(from, to)),
			Off:   next,
		})
	}
	return page
}

// collectLocked scans [from, to] once, counting every matching occurrence
// and the blocks holding at least one match over the whole range, while
// retaining only the hits whose match index falls inside
// [offset, offset+limit). The retained window therefore grows with the page
// size, never with the total number of matches in the range. The caller must
// hold index.mu.
func (index *Index) collectLocked(from, to int64, txIDs []string, offset int64, limit int) (hits []TxHit, total, blocks int64) {
	var filter map[string]struct{}
	if len(txIDs) > 0 {
		filter = make(map[string]struct{}, len(txIDs))
		for _, id := range txIDs {
			filter[id] = struct{}{}
		}
	}
	hits = []TxHit{}
	for height := from; height <= to; height++ {
		block := index.Blocks[height]
		matched := false
		for position, tx := range block.Txs {
			if filter != nil {
				if _, ok := filter[tx]; !ok {
					continue
				}
			}
			if total >= offset && len(hits) < limit {
				hits = append(hits, TxHit{Height: height, BlockHash: block.Hash, TxID: tx, Position: position})
			}
			total++
			matched = true
		}
		if matched {
			blocks++
		}
	}
	return hits, total, blocks
}

// fingerprintLocked hashes the exact content of every block in [from, to]:
// height, hash, parent, the ordered transaction list, and the timestamp
// (distinguishing a missing timestamp from zero). The caller must hold
// index.mu.
func (index *Index) fingerprintLocked(from, to int64) []byte {
	h := sha256.New()
	var lenBuf [8]byte
	writeString := func(s string) {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		h.Write(lenBuf[:])
		h.Write([]byte(s))
	}
	for height := from; height <= to; height++ {
		block := index.Blocks[height]
		binary.BigEndian.PutUint64(lenBuf[:], uint64(block.Height))
		h.Write(lenBuf[:])
		writeString(block.Hash)
		writeString(block.Parent)
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(block.Txs)))
		h.Write(lenBuf[:])
		for _, tx := range block.Txs {
			writeString(tx)
		}
		if block.Time == nil {
			h.Write([]byte{0})
		} else {
			h.Write([]byte{1})
			binary.BigEndian.PutUint64(lenBuf[:], uint64(*block.Time))
			h.Write(lenBuf[:])
		}
	}
	return h.Sum(nil)
}

// hashTxSet hashes the deduplicated, sorted identifier set so that cursor
// validation is insensitive to duplicates and ordering.
func hashTxSet(txIDs []string) []byte {
	unique := make(map[string]struct{}, len(txIDs))
	sorted := make([]string, 0, len(txIDs))
	for _, id := range txIDs {
		if _, ok := unique[id]; ok {
			continue
		}
		unique[id] = struct{}{}
		sorted = append(sorted, id)
	}
	sort.Strings(sorted)
	h := sha256.New()
	var lenBuf [8]byte
	for _, id := range sorted {
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(id)))
		h.Write(lenBuf[:])
		h.Write([]byte(id))
	}
	return h.Sum(nil)
}

// cursorPayload is the signed state carried between pages of one query.
type cursorPayload struct {
	V     int    `json:"v"`
	From  int64  `json:"from"`
	ReqTo int64  `json:"reqTo"`
	To    int64  `json:"to"`
	Set   string `json:"set"`
	FP    string `json:"fp"`
	Off   int64  `json:"off"`
}

const cursorPrefix = "q1"

// encodeCursor serializes the payload and signs it with the per-instance
// key, so cursors are self-contained yet unusable on any other index.
func (index *Index) encodeCursor(payload cursorPayload) string {
	raw, err := json.Marshal(payload)
	if err != nil {
		panic(err) // cursorPayload only contains marshalable fields
	}
	body := base64.RawURLEncoding.EncodeToString(raw)
	mac := hmac.New(sha256.New, index.key[:])
	mac.Write([]byte(cursorPrefix + "." + body))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return cursorPrefix + "." + body + "." + sig
}

// decodeCursor verifies the signature before trusting any payload field;
// every failure is an argument error, never a data-changed error.
func (index *Index) decodeCursor(cursor string) (cursorPayload, error) {
	var payload cursorPayload
	parts := strings.Split(cursor, ".")
	if len(parts) != 3 || parts[0] != cursorPrefix {
		return payload, fmt.Errorf("%w: malformed cursor", ErrInvalidArgument)
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return payload, fmt.Errorf("%w: malformed cursor", ErrInvalidArgument)
	}
	mac := hmac.New(sha256.New, index.key[:])
	mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return payload, fmt.Errorf("%w: cursor not recognized by this index", ErrInvalidArgument)
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return payload, fmt.Errorf("%w: malformed cursor", ErrInvalidArgument)
	}
	if err := json.Unmarshal(raw, &payload); err != nil || payload.V != 1 {
		return payload, fmt.Errorf("%w: malformed cursor", ErrInvalidArgument)
	}
	return payload, nil
}

var keyFallback atomic.Uint64

// newKey derives the per-instance cursor signing key. crypto/rand is not
// expected to fail; the counter fallback still keeps keys unique per
// instance so cursors never validate across indexes.
func newKey() [32]byte {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		binary.BigEndian.PutUint64(key[:8], keyFallback.Add(1))
	}
	return key
}
