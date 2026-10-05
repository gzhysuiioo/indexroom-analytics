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
	// Reverse reads the pinned range from the chain tip downward: heights
	// descend, and positions inside one block descend too. Ordering depends
	// only on height and the original in-block position; block timestamps —
	// missing, equal, or decreasing with height — never affect it. Recorded
	// positions stay the original zero-based offsets and are not renumbered.
	// The direction is pinned by the first page: a continuation must repeat
	// it, and presenting a reverse cursor with Reverse false (or vice versa)
	// fails with ErrInvalidArgument. To switch direction, restart with an
	// empty cursor.
	Reverse bool
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
// the block; a reverse query returns them in the opposite order — height
// descending, then position descending — over the same pinned range.
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
// With Reverse unset, hits run in ascending height and then ascending
// in-block position. With Reverse set, the same range is read from the
// chain tip downward: height descending, then position descending inside
// one block. Only height and the original in-block position determine the
// order; block timestamps — missing, equal, or decreasing with height —
// never affect it. Positions are the original zero-based offsets and are
// not renumbered, and repeated occurrences of one identifier are never
// merged. Whole-range statistics (TotalMatches, MatchedBlocks) are
// direction-free and identical in both directions.
//
// The first page (empty Cursor) pins the filtered range and the reading
// direction, and returns a continuation cursor while more results remain.
// Continuations reuse the pinned range — blocks appended above it are
// invisible to the query — and must keep the same direction; presenting an
// ascending cursor with Reverse set, or a reverse cursor without it, fails
// with ErrInvalidArgument and returns no page. Continuations succeed as
// long as every block inside the range still matches the first page;
// otherwise they fail with ErrQueryChanged. To read appended blocks or to
// switch direction, restart with an empty cursor.
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
	filter := newTxFilter(query.TxIDs)
	hits, total, blocks := index.scanPageLocked(from, to, filter, 0, pageSize, query.Reverse)
	return index.buildPageLocked(query.To, filter, from, to, hits, total, blocks, 0, pageSize, query.Reverse), nil
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
	if query.Reverse != payload.Rev {
		// The cursor pins its reading direction; changing it is a different
		// query and must restart from an empty cursor.
		if payload.Rev {
			return TxPage{}, fmt.Errorf("%w: cursor reads in reverse, continuation must set Reverse", ErrInvalidArgument)
		}
		return TxPage{}, fmt.Errorf("%w: cursor reads in ascending order, continuation must not set Reverse", ErrInvalidArgument)
	}
	// One canonicalization drives both set-equivalence and the rescanning
	// filter; reordering or repeating identifiers changes nothing, while any
	// added, removed, or replaced identifier rejects the continuation before
	// the chain is examined.
	filter := newTxFilter(query.TxIDs)
	if filter.hexDigest() != payload.Set {
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
	// Re-scan the still-identical pinned range: the cursor records the
	// absolute offset, but no full match list is kept between pages.
	hits, total, blocks := index.scanPageLocked(payload.From, payload.To, filter, payload.Off, pageSize, payload.Rev)
	if payload.Off > total {
		return TxPage{}, fmt.Errorf("%w: cursor offset is beyond the pinned results", ErrQueryChanged)
	}
	return index.buildPageLocked(query.To, filter, payload.From, payload.To, hits, total, blocks, payload.Off, pageSize, payload.Rev), nil
}

// buildPageLocked assembles one page from a page-sized scan and mints the
// continuation cursor when matches remain. total and blocks describe the
// whole pinned range; hits holds the current page (already in the requested
// direction), offset counts occurrences already delivered from the
// traversal start (low end ascending, high end reverse). reqTo is the
// caller's verbatim To (zero for tip-bound queries). The caller must hold
// index.mu.
func (index *Index) buildPageLocked(reqTo int64, filter txFilter, from, to int64, hits []TxHit, total, blocks, offset int64, pageSize int, reverse bool) TxPage {
	page := TxPage{
		Hits:          hits,
		TotalMatches:  total,
		MatchedBlocks: blocks,
		ToHeight:      to,
	}
	if offset+int64(len(hits)) < total {
		page.NextCursor = index.encodeCursor(cursorPayload{
			V:     1,
			From:  from,
			ReqTo: reqTo,
			To:    to,
			Set:   filter.hexDigest(),
			FP:    hex.EncodeToString(index.fingerprintLocked(from, to)),
			Off:   offset + int64(len(hits)),
			Rev:   reverse,
		})
	}
	return page
}

// scanPageLocked walks the whole pinned range, counting every matching
// occurrence and every block holding one. The counts are direction-free:
// they always describe the range in ascending height/position order.
//
// offset counts occurrences already delivered before this page, measured
// from the traversal start: for ascending reads it is a low-end ascending
// offset and the page keeps [offset, offset+pageSize) in ascending order;
// for reverse reads it counts from the high end (offset 0 is the tip-side
// match) and the page walks downward from there, height descending and then
// position descending. Either way the retained slice holds at most pageSize
// records, positions are the original zero-based offsets (never renumbered),
// and ordering depends only on height and in-block position — block
// timestamps, missing, equal, or decreasing, never affect it. The caller
// must hold index.mu.
func (index *Index) scanPageLocked(from, to int64, filter txFilter, offset int64, pageSize int, reverse bool) (hits []TxHit, total, blocks int64) {
	// First pass in ascending order establishes the direction-free totals,
	// which the cursor offset is interpreted against in both directions.
	for height := from; height <= to; height++ {
		block := index.Blocks[height]
		matched := false
		for _, tx := range block.Txs {
			if filter.matches(tx) {
				total++
				matched = true
			}
		}
		if matched {
			blocks++
		}
	}

	hits = make([]TxHit, 0, pageSize)
	if !reverse {
		// Ascending window: walk low to high and keep [offset, offset+pageSize).
		var seen int64
		for height := from; height <= to && int64(len(hits)) < int64(pageSize); height++ {
			block := index.Blocks[height]
			for position, tx := range block.Txs {
				if !filter.matches(tx) {
					continue
				}
				if seen >= offset && int64(len(hits)) < int64(pageSize) {
					hits = append(hits, TxHit{Height: height, BlockHash: block.Hash, TxID: tx, Position: position})
				}
				seen++
			}
		}
		return hits, total, blocks
	}

	// Reverse window: offset counts occurrences already delivered from the
	// top of the range, so this page covers ascending offsets
	// [total-offset-pageSize, total-offset), listed from the high end down.
	// Number matches from the top while walking height/position descending:
	// skip the first offset matches (already delivered), retain up to
	// pageSize, stop. Positions stay the original zero-based offsets and are
	// never renumbered.
	var seen int64
	for height := to; height >= from && int64(len(hits)) < int64(pageSize); height-- {
		block := index.Blocks[height]
		for position := len(block.Txs) - 1; position >= 0; position-- {
			tx := block.Txs[position]
			if !filter.matches(tx) {
				continue
			}
			if seen < offset {
				seen++
				continue
			}
			if int64(len(hits)) < int64(pageSize) {
				hits = append(hits, TxHit{Height: height, BlockHash: block.Hash, TxID: tx, Position: position})
			}
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

// cursorPayload is the signed state carried between pages of one query.
type cursorPayload struct {
	V     int    `json:"v"`
	From  int64  `json:"from"`
	ReqTo int64  `json:"reqTo"`
	To    int64  `json:"to"`
	Set   string `json:"set"`
	FP    string `json:"fp"`
	Off   int64  `json:"off"`
	// Rev records the reading direction pinned by the first page. It is
	// omitted when false, so ascending cursors keep their pre-reverse
	// byte encoding and existing cursors continue to validate.
	Rev bool `json:"rev,omitempty"`
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
