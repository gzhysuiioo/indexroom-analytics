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

// TxOrder selects the occurrence order of a QueryTxs result.
type TxOrder int

const (
	// OrderAsc returns occurrences by increasing height and, inside one
	// block, by increasing position. It is the zero value.
	OrderAsc TxOrder = 0
	// OrderDesc returns occurrences by decreasing height and, inside one
	// block, by decreasing position. The order is decided by height and the
	// original block positions alone; block timestamps never participate.
	OrderDesc TxOrder = 1
)

func (order TxOrder) valid() bool {
	return order == OrderAsc || order == OrderDesc
}

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
	// Order selects the occurrence order over the whole pinned range:
	// OrderAsc (the zero value) by increasing height/position, OrderDesc by
	// decreasing height/position. A continuation must repeat the first
	// page's order, otherwise QueryTxs returns ErrInvalidArgument.
	Order TxOrder
	// TimeStart and TimeEnd optionally bound the searched block timestamps as
	// a half-open window [TimeStart, TimeEnd), reusing QueryTimeStats
	// semantics: non-negative Unix seconds, start included, end excluded.
	// Both pointers nil disables time filtering, so an unused window stays
	// distinct from a window whose start is genuinely zero; providing exactly
	// one bound is ErrInvalidArgument. While enabled, a block without a
	// timestamp never matches, while a real timestamp of zero is judged like
	// any other value. The filter only selects which occurrences survive:
	// ordering still depends solely on height and block position. A
	// continuation must repeat whether the filter is enabled and the exact
	// same window, otherwise QueryTxs returns ErrInvalidArgument.
	TimeStart *int64
	TimeEnd   *int64
	// MinBlocks optionally requires a transaction identifier to appear in at
	// least this many distinct blocks of the pinned range before any of its
	// occurrences are kept. Zero (the default) disables the filter; a
	// negative value is ErrInvalidArgument. Block membership is counted over
	// the whole pinned height range under the same TxIDs and time-window
	// conditions as the scan itself: repeated occurrences inside one block
	// count once, and blocks outside the range, without a timestamp, or
	// outside the window never help an identifier qualify. Qualification is
	// decided by the entire pinned range, never by the current page alone;
	// once an identifier qualifies, every one of its matching occurrences is
	// kept, with no deduplication. A continuation must repeat the first
	// page's value, otherwise QueryTxs returns ErrInvalidArgument.
	MinBlocks int64
	// Cursor continues an earlier query; empty asks for the first page. On
	// continuation the height range, TxIDs, Order, time window and MinBlocks
	// must equal the first page's.
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
// whole filtered range. Hits are ordered by height then position inside the
// block: both increasing under OrderAsc and both decreasing under OrderDesc.
// A hit's Position stays its zero-based index inside the block; descending
// order never renumbers it.
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
// With Order OrderAsc (the zero value) hits arrive by increasing height and
// position inside the block; OrderDesc walks the same pinned range the other
// way, from the tip-bound upper height downward, positions inside each block
// likewise decreasing. Ordering depends only on height and position — never
// on block timestamps — and ascending queries behave exactly as before.
//
// TimeStart and TimeEnd together enable an optional timestamp window
// [TimeStart, TimeEnd) with the same semantics as QueryTimeStats: it
// intersects the height range and the TxIDs filter rather than replacing
// them, timestamps need not be monotonic, a missing timestamp never
// matches, and a real zero is judged normally. Time filtering never changes
// the order, the block positions, or the deduplication of occurrences.
//
// MinBlocks optionally keeps only the identifiers appearing in at least
// that many distinct blocks of the pinned range, counted under the same
// TxIDs and time-window conditions. Qualification is decided over the whole
// pinned range before paging, so every page reports the same TotalMatches
// and MatchedBlocks, and a qualifying identifier keeps all of its matching
// occurrences in their original order and positions.
//
// The first page (empty Cursor) pins the filtered range and returns a
// continuation cursor while more results remain. Continuations reuse the
// pinned range — blocks appended above it are invisible to the query — and
// succeed as long as every block inside the range still matches the first
// page; otherwise they fail with ErrQueryChanged. The order, the time
// filter's enabled state and its window are pinned too: a cursor minted in
// one direction or with one window may not be continued with another.
func (index *Index) QueryTxs(query TxQuery) (TxPage, error) {
	pageSize, err := normalizePageSize(query.PageSize)
	if err != nil {
		return TxPage{}, err
	}
	if !query.Order.valid() {
		return TxPage{}, fmt.Errorf("%w: order must be OrderAsc or OrderDesc", ErrInvalidArgument)
	}
	window, err := normalizeTimeWindow(query.TimeStart, query.TimeEnd)
	if err != nil {
		return TxPage{}, err
	}
	if query.MinBlocks < 0 {
		return TxPage{}, fmt.Errorf("%w: min blocks must not be negative", ErrInvalidArgument)
	}
	if query.Cursor != "" {
		return index.continueQuery(query, pageSize, window)
	}
	return index.firstQuery(query, pageSize, window)
}

// timeWindow is the canonical form of TxQuery's optional timestamp window.
// enabled is false only when both bounds are absent, so an unused window
// stays distinct from [0, end); when enabled, start and end hold non-negative
// Unix seconds with start < end.
type timeWindow struct {
	enabled bool
	start   int64
	end     int64
}

// windowBoundError identifies which shared window rule a pair of bounds
// violates, so each query entry keeps its own error wording and reporting
// order while the rules themselves live in exactly one place.
type windowBoundError int

const (
	windowBoundOK windowBoundError = iota
	windowBoundNegativeStart
	windowBoundNegativeEnd
	windowBoundStartNotBelowEnd
)

// checkWindowBounds applies the window legality rules shared by QueryTxs
// and QueryTimeStats: both bounds must be non-negative Unix seconds and the
// included start must be below the excluded end. Violations are reported in
// a fixed order — negative start, then negative end, then empty window — so
// every caller keeps reporting the same first problem as before.
func checkWindowBounds(start, end int64) windowBoundError {
	switch {
	case start < 0:
		return windowBoundNegativeStart
	case end < 0:
		return windowBoundNegativeEnd
	case start >= end:
		return windowBoundStartNotBelowEnd
	}
	return windowBoundOK
}

// normalizeTimeWindow validates the optional half-open window: both bounds
// must be absent together, and present bounds must satisfy the shared rules
// of checkWindowBounds. Exactly one bound is ErrInvalidArgument, as is any
// bound violation.
func normalizeTimeWindow(startPtr, endPtr *int64) (timeWindow, error) {
	if (startPtr == nil) != (endPtr == nil) {
		return timeWindow{}, fmt.Errorf("%w: time window needs both start and end or neither", ErrInvalidArgument)
	}
	if startPtr == nil {
		return timeWindow{}, nil
	}
	switch checkWindowBounds(*startPtr, *endPtr) {
	case windowBoundNegativeStart:
		return timeWindow{}, fmt.Errorf("%w: time window start must not be negative", ErrInvalidArgument)
	case windowBoundNegativeEnd:
		return timeWindow{}, fmt.Errorf("%w: time window end must not be negative", ErrInvalidArgument)
	case windowBoundStartNotBelowEnd:
		return timeWindow{}, fmt.Errorf("%w: time window start must be below end", ErrInvalidArgument)
	}
	return timeWindow{enabled: true, start: *startPtr, end: *endPtr}, nil
}

// contains is the single block-hit rule shared by QueryTxs and
// QueryTimeStats: an enabled window rejects missing timestamps, while start
// is included and end excluded. A disabled window matches regardless of the
// timestamp.
func (w timeWindow) contains(when *int64) bool {
	if !w.enabled {
		return true
	}
	if when == nil {
		return false
	}
	return *when >= w.start && *when < w.end
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

func (index *Index) firstQuery(query TxQuery, pageSize int, window timeWindow) (TxPage, error) {
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
	qualified := index.qualifyingIDsLocked(from, to, filter, window, query.MinBlocks)
	hits, total, blocks := index.scanPageQualifiedLocked(from, to, filter, window, query.Order, 0, pageSize, qualified)
	return index.buildPageLocked(query.To, filter, window, from, to, hits, total, blocks, 0, pageSize, query.Order, query.MinBlocks), nil
}

func (index *Index) continueQuery(query TxQuery, pageSize int, window timeWindow) (TxPage, error) {
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
	// One canonicalization drives both set-equivalence and the rescanning
	// filter; reordering or repeating identifiers changes nothing, while any
	// added, removed, or replaced identifier rejects the continuation before
	// the chain is examined.
	filter := newTxFilter(query.TxIDs)
	if filter.hexDigest() != payload.Set {
		return TxPage{}, fmt.Errorf("%w: transaction filter differs from the first page", ErrInvalidArgument)
	}
	if query.Order != payload.Order {
		return TxPage{}, fmt.Errorf("%w: read order differs from the first page", ErrInvalidArgument)
	}
	// The time window is pinned like the other conditions: enabling it on a
	// cursor minted without one (or the reverse), or shifting either bound,
	// rejects the continuation before the chain is examined. The caller must
	// restart from an empty cursor.
	if window != payload.window() {
		return TxPage{}, fmt.Errorf("%w: time window differs from the first page", ErrInvalidArgument)
	}
	// MinBlocks is pinned like the other conditions: a cursor minted with
	// one threshold may not be continued with another.
	if query.MinBlocks != payload.MinBlocks {
		return TxPage{}, fmt.Errorf("%w: min blocks differs from the first page", ErrInvalidArgument)
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
	qualified := index.qualifyingIDsLocked(payload.From, payload.To, filter, window, payload.MinBlocks)
	hits, total, blocks := index.scanPageQualifiedLocked(payload.From, payload.To, filter, window, payload.Order, payload.Off, pageSize, qualified)
	if payload.Off > total {
		return TxPage{}, fmt.Errorf("%w: cursor offset is beyond the pinned results", ErrQueryChanged)
	}
	return index.buildPageLocked(query.To, filter, window, payload.From, payload.To, hits, total, blocks, payload.Off, pageSize, payload.Order, payload.MinBlocks), nil
}

// buildPageLocked assembles one page from a page-sized scan and mints the
// continuation cursor when matches remain. total and blocks describe the
// whole pinned range; hits holds only the window [offset, offset+pageSize)
// in the scan's order. reqTo is the caller's verbatim To (zero for
// tip-bound queries). The caller must hold index.mu.
func (index *Index) buildPageLocked(reqTo int64, filter txFilter, window timeWindow, from, to int64, hits []TxHit, total, blocks, offset int64, pageSize int, order TxOrder, minBlocks int64) TxPage {
	page := TxPage{
		Hits:          hits,
		TotalMatches:  total,
		MatchedBlocks: blocks,
		ToHeight:      to,
	}
	if offset+int64(len(hits)) < total {
		payload := cursorPayload{
			V:         1,
			From:      from,
			ReqTo:     reqTo,
			To:        to,
			Set:       filter.hexDigest(),
			FP:        hex.EncodeToString(index.fingerprintLocked(from, to)),
			Order:     order,
			Off:       offset + int64(len(hits)),
			MinBlocks: minBlocks,
		}
		if window.enabled {
			start, end := window.start, window.end
			payload.TimeStart, payload.TimeEnd = &start, &end
		}
		page.NextCursor = index.encodeCursor(payload)
	}
	return page
}

// qualifyingIDsLocked applies the MinBlocks condition: it counts, for every
// identifier passing the filter, the distinct blocks of [from, to] that hold
// it and also survive the timestamp window, then keeps the identifiers
// reaching minBlocks. Blocks outside the window (or without a timestamp
// while the window is enabled) contribute nothing. A nil map is returned
// when minBlocks is zero, meaning every occurrence passes; an empty non-nil
// map means the filter is active and nothing qualified. The caller must hold
// index.mu.
func (index *Index) qualifyingIDsLocked(from, to int64, filter txFilter, window timeWindow, minBlocks int64) map[string]struct{} {
	if minBlocks <= 0 {
		return nil
	}
	counts := make(map[string]int64)
	for height := from; height <= to; height++ {
		block := index.Blocks[height]
		if !window.contains(block.Time) {
			continue
		}
		// One block contributes at most one to an identifier's count, however
		// many times the identifier repeats inside it.
		seen := make(map[string]struct{}, len(block.Txs))
		for _, tx := range block.Txs {
			if !filter.matches(tx) {
				continue
			}
			if _, ok := seen[tx]; ok {
				continue
			}
			seen[tx] = struct{}{}
			counts[tx]++
		}
	}
	qualified := make(map[string]struct{}, len(counts))
	for id, n := range counts {
		if n >= minBlocks {
			qualified[id] = struct{}{}
		}
	}
	return qualified
}

// scanPageLocked is scanPageQualifiedLocked without a MinBlocks condition:
// every occurrence passing the identifier filter and the time window counts.
func (index *Index) scanPageLocked(from, to int64, filter txFilter, window timeWindow, order TxOrder, offset int64, pageSize int) (hits []TxHit, total, blocks int64) {
	return index.scanPageQualifiedLocked(from, to, filter, window, order, offset, pageSize, nil)
}

// scanPageQualifiedLocked walks the whole pinned range once in the requested
// order, counting every matching occurrence and every block holding one,
// while retaining only the page window [offset, offset+pageSize) in a
// pre-sized slice: at most pageSize TxHit records, independent of how many
// matches the whole range holds. OrderAsc walks increasing heights then
// increasing positions; OrderDesc walks decreasing heights then decreasing
// positions. total, blocks and the recorded positions are the same in either
// order. An occurrence survives only when it passes the identifier filter,
// the block's timestamp window, and — when qualified is non-nil — its
// identifier belongs to the MinBlocks-qualified set computed over the whole
// pinned range; qualifying occurrences are kept individually, never
// deduplicated. The window never participates in ordering. The caller must
// hold index.mu.
func (index *Index) scanPageQualifiedLocked(from, to int64, filter txFilter, window timeWindow, order TxOrder, offset int64, pageSize int, qualified map[string]struct{}) (hits []TxHit, total, blocks int64) {
	hits = make([]TxHit, 0, pageSize)
	height, step := from, int64(1)
	if order == OrderDesc {
		height, step = to, -1
	}
	for ; from <= height && height <= to; height += step {
		block := index.Blocks[height]
		if !window.contains(block.Time) {
			// A block outside the timestamp window contributes no
			// occurrences, whatever transactions it holds.
			continue
		}
		matched := false
		position, pStep := 0, 1
		end := len(block.Txs)
		if order == OrderDesc {
			position, pStep, end = len(block.Txs)-1, -1, -1
		}
		for ; position != end; position += pStep {
			tx := block.Txs[position]
			if !filter.matches(tx) {
				continue
			}
			if qualified != nil {
				if _, ok := qualified[tx]; !ok {
					continue
				}
			}
			if total >= offset && int64(len(hits)) < int64(pageSize) {
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

// fingerprintLocked hashes the exact content of every block in [from, to] by
// feeding each block's canonical content encoding (see encodeBlockContent,
// the one definition of block content shared with sameBlock) into a single
// digest: height, hash, parent, the ordered transaction list, and the
// timestamp — distinguishing a missing timestamp from zero — all take part.
// The caller must hold index.mu.
func (index *Index) fingerprintLocked(from, to int64) []byte {
	h := sha256.New()
	var buf []byte
	for height := from; height <= to; height++ {
		h.Write(encodeBlockContent(buf[:0], index.Blocks[height]))
	}
	return h.Sum(nil)
}

// cursorPayload is the signed state carried between pages of one query.
//
// TimeStart and TimeEnd are both absent for a query without a timestamp
// window and both present for one with one, mirroring TxQuery; a window
// starting at zero still carries an explicit zero pointer, so it can never
// be confused with the disabled case.
type cursorPayload struct {
	V     int     `json:"v"`
	From  int64   `json:"from"`
	ReqTo int64   `json:"reqTo"`
	To    int64   `json:"to"`
	Set   string  `json:"set"`
	FP    string  `json:"fp"`
	Order TxOrder `json:"ord"`
	Off   int64   `json:"off"`
	// MinBlocks carries the first page's threshold; omitted when zero so
	// cursors minted before the field existed keep validating.
	MinBlocks int64  `json:"minB,omitempty"`
	TimeStart *int64 `json:"tStart,omitempty"`
	TimeEnd   *int64 `json:"tEnd,omitempty"`
}

// window reconstructs the pinned timestamp window. A signed payload is only
// ever minted by buildPageLocked with both pointers present together;
// malformed combinations fail closed as a disabled-window mismatch rather
// than panicking.
func (p cursorPayload) window() timeWindow {
	if p.TimeStart == nil || p.TimeEnd == nil {
		return timeWindow{}
	}
	return timeWindow{enabled: true, start: *p.TimeStart, end: *p.TimeEnd}
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
