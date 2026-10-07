package indexroom

import (
	"reflect"
	"sort"
	"testing"
)

// This file is the regression net for the correspondence between the two
// public read models: QueryTxs (paginated occurrences) and QueryTimeStats
// (time-segmented counters). Every expectation below is derived by an
// independent oracle built straight from the input fixture blocks — never by
// calling either query — so the two production queries cannot pass by sharing
// one omission.
//
// The protected behavior, in one place:
//
//   - After reading every detail page, TotalMatches equals the number of
//     detail rows, MatchedBlocks equals the distinct blocks the rows touch,
//     and the distinct detail identifiers equal Totals.DistinctTxIDs.
//   - Both whole-page counters are identical on every page; they describe
//     the whole pinned range, never the current page.
//   - Each returned time segment's three counters equal the complete detail
//     rows whose block timestamp falls in that segment, under the same height
//     range and filters.
//   - One identifier repeated inside one block stays separate rows and
//     counts by occurrence, while the block counts once; the window-wide
//     distinct count dedups globally and is NOT the sum of per-segment
//     distinct counts.
//   - Ascending and descending pagination return the same occurrence set;
//     positions stay the original in-block indexes; resizing pages midway
//     changes none of the correspondence.
//   - Identifier-set order and duplicates are irrelevant; no set differs
//     from an empty set, which differs from {""}; case and surrounding
//     whitespace stay exact.
//   - Time stays half-open [start,end); the last segment is cut to the real
//     end; timestamps may decrease with height; missing timestamps enter no
//     segment and no time-filtered detail while still counted as missing; a
//     real timestamp of zero participates normally.
//   - A legal no-match query is a successful empty first page without a
//     cursor, together with zeroed segments covering the whole window.

// corrOcc is one matching occurrence as the independent oracle derived it
// from an input block: identity is the (height, in-block position) pair, and
// txID/hash/when are copied from the fixture for content verification.
type corrOcc struct {
	height int64
	pos    int
	txID   string
	hash   string
	when   int64
}

type corrSeg struct {
	start, end int64
	occ        []corrOcc
	blocks     map[int64]struct{}
	ids        map[string]struct{}
}

// corrOracle is the expected answer computed solely from the input blocks.
type corrOracle struct {
	from, to int64
	missing  int64
	occ      []corrOcc
	blocks   map[int64]struct{}
	ids      map[string]struct{}
	segments []corrSeg
}

// corrAnchors pins the scenario's intended numbers explicitly, so an
// accidental fixture edit cannot silently move the oracle and the test
// together. counts holds the expected TxCount of every segment in order.
type corrAnchors struct {
	total, matchedBlocks, missing int64
	windowDistinct                int64
	segmentDistinctSum            int64
	counts                        []int64
}

type corrScenario struct {
	name      string
	blocks    []Block
	from, to  int64
	txIDs     []string
	start     int64
	end       int64
	step      int64
	firstPage int
	// pageSizes cycles across continuation pages to prove that changing the
	// page size between pages keeps the counters and the stitched set.
	pageSizes []int
	anchors   corrAnchors
}

// corrRichBlocks is the shared fixture for the correspondence scenarios:
//
//	h1 time   0: ["a","b","a",""]   real zero, same id twice in one block
//	h2 time 100: ["a","c","a"]      same timestamp as h5, same ids across blocks
//	h3 no time: ["a","z"]           missing timestamp; holds filter matches
//	h4 time  90: ["b","c"]          timestamp decreases with height
//	h5 time 100: ["a"]
//	h6 time 160: ["a","b"]
//	h7 time 200: ["x"]              excluded when the window end is 200
//	h8 no time: [""]                missing timestamp carrying the empty id
func corrRichBlocks() []Block {
	return []Block{
		{Height: 1, Hash: "h1", Parent: "genesis", Txs: []string{"a", "b", "a", ""}, Time: intptr(0)},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a", "c", "a"}, Time: intptr(100)},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a", "z"}, Time: nil},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"b", "c"}, Time: intptr(90)},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"a"}, Time: intptr(100)},
		{Height: 6, Hash: "h6", Parent: "h5", Txs: []string{"a", "b"}, Time: intptr(160)},
		{Height: 7, Hash: "h7", Parent: "h6", Txs: []string{"x"}, Time: intptr(200)},
		{Height: 8, Hash: "h8", Parent: "h7", Txs: []string{""}, Time: nil},
	}
}

func corrBuildIndex(t *testing.T, blocks []Block) *Index {
	t.Helper()
	index := New()
	for _, b := range blocks {
		if err := index.Append(b); err != nil {
			t.Fatalf("setup append at height %d: %v", b.Height, err)
		}
	}
	return index
}

// corrBuildOracle recomputes the expected answer directly from the input
// blocks. Height resolution, filter membership, the half-open window and the
// segment assignment mirror the product specification in test code rather
// than reusing production helpers.
func corrBuildOracle(t *testing.T, s corrScenario) corrOracle {
	t.Helper()
	tip := int64(len(s.blocks))
	for i, b := range s.blocks {
		if b.Height != int64(i+1) {
			t.Fatalf("fixture must be a contiguous chain: index %d holds height %d", i, b.Height)
		}
	}
	from := s.from
	if from == 0 {
		from = 1
	}
	to := s.to
	if to == 0 || to > tip {
		to = tip
	}

	var members map[string]struct{}
	if len(s.txIDs) > 0 {
		members = make(map[string]struct{}, len(s.txIDs))
		for _, id := range s.txIDs {
			members[id] = struct{}{}
		}
	}
	matches := func(id string) bool {
		if members == nil {
			return true
		}
		_, ok := members[id]
		return ok
	}

	width := s.end - s.start
	n := width / s.step
	if width%s.step != 0 {
		n++
	}
	oc := corrOracle{
		from:     from,
		to:       to,
		blocks:   map[int64]struct{}{},
		ids:      map[string]struct{}{},
		segments: make([]corrSeg, n),
	}
	for i := range oc.segments {
		seg := corrSeg{
			start:  s.start + s.step*int64(i),
			end:    s.start + s.step*int64(i+1),
			blocks: map[int64]struct{}{},
			ids:    map[string]struct{}{},
		}
		if seg.end > s.end {
			seg.end = s.end
		}
		oc.segments[i] = seg
	}
	if from > to {
		return oc
	}
	for h := from; h <= to; h++ {
		block := s.blocks[h-1]
		if block.Time == nil {
			// Missing timestamps never reach a segment or a time-filtered
			// detail, but are still counted over the selected range.
			oc.missing++
			continue
		}
		when := *block.Time
		if when < s.start || when >= s.end {
			continue
		}
		idx := (when - s.start) / s.step
		for pos, tx := range block.Txs {
			if !matches(tx) {
				continue
			}
			occ := corrOcc{height: h, pos: pos, txID: tx, hash: block.Hash, when: when}
			oc.occ = append(oc.occ, occ)
			oc.blocks[h] = struct{}{}
			oc.ids[tx] = struct{}{}
			seg := &oc.segments[idx]
			seg.occ = append(seg.occ, occ)
			seg.blocks[h] = struct{}{}
			seg.ids[tx] = struct{}{}
		}
	}
	return oc
}

// corrAssertStats checks QueryTimeStats against the oracle and the explicit
// anchors, including whole-window segment coverage and the truncated end.
func corrAssertStats(t *testing.T, stats TimeStats, oc corrOracle, want corrAnchors) {
	t.Helper()
	if stats.FromHeight != oc.from || stats.ToHeight != oc.to {
		t.Fatalf("resolved range [%d,%d], want [%d,%d]", stats.FromHeight, stats.ToHeight, oc.from, oc.to)
	}
	if stats.MissingTimeBlocks != oc.missing || stats.MissingTimeBlocks != want.missing {
		t.Fatalf("missing blocks=%d, want %d (anchor %d)", stats.MissingTimeBlocks, oc.missing, want.missing)
	}
	if len(stats.Buckets) != len(oc.segments) {
		t.Fatalf("segments=%d, want %d", len(stats.Buckets), len(oc.segments))
	}
	var distinctSum int64
	var countSum int64
	var blockSum int64
	for i, got := range stats.Buckets {
		exp := oc.segments[i]
		if got.Start != exp.start || got.End != exp.end {
			t.Fatalf("segment %d bounds [%d,%d), want [%d,%d)", i, got.Start, got.End, exp.start, exp.end)
		}
		if got.TxCount != int64(len(exp.occ)) {
			t.Fatalf("segment %d [%d,%d) TxCount=%d, want %d details", i, exp.start, exp.end, got.TxCount, len(exp.occ))
		}
		if got.Blocks != int64(len(exp.blocks)) {
			t.Fatalf("segment %d Blocks=%d, want %d distinct detail blocks", i, got.Blocks, len(exp.blocks))
		}
		if got.DistinctTxIDs != int64(len(exp.ids)) {
			t.Fatalf("segment %d DistinctTxIDs=%d, want %d", i, got.DistinctTxIDs, len(exp.ids))
		}
		distinctSum += got.DistinctTxIDs
		countSum += got.TxCount
		blockSum += got.Blocks
	}
	if want.counts != nil {
		gotCounts := make([]int64, len(stats.Buckets))
		for i, b := range stats.Buckets {
			gotCounts[i] = b.TxCount
		}
		if !reflect.DeepEqual(gotCounts, want.counts) {
			t.Fatalf("segment TxCounts=%v, want %v", gotCounts, want.counts)
		}
	}
	if stats.Totals.TxCount != int64(len(oc.occ)) || stats.Totals.TxCount != want.total || countSum != want.total {
		t.Fatalf("window TxCount stats=%d oracle=%d anchor=%d segmentSum=%d",
			stats.Totals.TxCount, len(oc.occ), want.total, countSum)
	}
	if stats.Totals.Blocks != int64(len(oc.blocks)) || stats.Totals.Blocks != want.matchedBlocks || blockSum != want.matchedBlocks {
		t.Fatalf("window Blocks stats=%d oracle=%d anchor=%d segmentSum=%d",
			stats.Totals.Blocks, len(oc.blocks), want.matchedBlocks, blockSum)
	}
	if stats.Totals.DistinctTxIDs != int64(len(oc.ids)) || stats.Totals.DistinctTxIDs != want.windowDistinct {
		t.Fatalf("window DistinctTxIDs=%d, want oracle %d anchor %d",
			stats.Totals.DistinctTxIDs, len(oc.ids), want.windowDistinct)
	}
	if distinctSum != want.segmentDistinctSum {
		t.Fatalf("sum of per-segment distinct=%d, want anchor %d", distinctSum, want.segmentDistinctSum)
	}
	// The whole-window distinct count dedups across segments: where the same
	// id recurs in multiple segments it must stay below the per-segment sum.
	if want.segmentDistinctSum != want.windowDistinct && stats.Totals.DistinctTxIDs >= distinctSum {
		t.Fatalf("window distinct %d should be below segment distinct sum %d", stats.Totals.DistinctTxIDs, distinctSum)
	}
}

// corrCollectPages walks the cursor chain, cycling continuationSizes across
// pages after the first.
func corrCollectPages(t *testing.T, index *Index, base TxQuery, firstSize int, continuationSizes []int) []TxPage {
	t.Helper()
	query := base
	query.PageSize = firstSize
	var pages []TxPage
	for i := 0; ; i++ {
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("QueryTxs page %d failed: %v", i, err)
		}
		pages = append(pages, page)
		if page.NextCursor == "" {
			return pages
		}
		query.Cursor = page.NextCursor
		if len(continuationSizes) > 0 {
			query.PageSize = continuationSizes[i%len(continuationSizes)]
		}
	}
}

// occKey identifies one occurrence independently of its row spelling.
type occKey struct {
	height int64
	pos    int
}

func corrKeys(hits []TxHit) []occKey {
	keys := make([]occKey, len(hits))
	for i, h := range hits {
		keys[i] = occKey{h.Height, h.Position}
	}
	return keys
}

// corrAssertPages verifies every page's whole-range counters, the stitched
// ordering per direction, and each row's content against the INPUT blocks,
// then requires the stitched rows to equal the oracle occurrence sequence
// (descending must be its exact reverse). It returns the stitched hits.
func corrAssertPages(t *testing.T, blocks []Block, oc corrOracle, pages []TxPage, order TxOrder, sizes []int) []TxHit {
	t.Helper()
	var stitched []TxHit
	var consumed int64
	for i, page := range pages {
		// Whole-range counters describe every match and never drift per page.
		if page.TotalMatches != int64(len(oc.occ)) {
			t.Fatalf("page %d TotalMatches=%d, want %d", i, page.TotalMatches, len(oc.occ))
		}
		if page.MatchedBlocks != int64(len(oc.blocks)) {
			t.Fatalf("page %d MatchedBlocks=%d, want %d", i, page.MatchedBlocks, len(oc.blocks))
		}
		if page.ToHeight != oc.to {
			t.Fatalf("page %d ToHeight=%d, want %d", i, page.ToHeight, oc.to)
		}
		wantSize := sizes[0]
		if i > 0 && len(sizes) > 1 {
			wantSize = sizes[1:][(i-1)%(len(sizes)-1)]
		}
		if i+1 < len(pages) {
			if page.NextCursor == "" {
				t.Fatalf("page %d is not last but carries no cursor", i)
			}
			if len(page.Hits) != wantSize {
				t.Fatalf("non-final page %d holds %d hits, want full page of %d", i, len(page.Hits), wantSize)
			}
		} else if page.NextCursor != "" {
			t.Fatalf("last page %d still carries a cursor", i)
		}
		consumed += int64(len(page.Hits))

		// Every row must agree with the actual input block: hash, identifier
		// at the original in-block position, a present timestamp inside the
		// window. Positions are never renumbered by descending order.
		for _, hit := range page.Hits {
			if hit.Height < 1 || hit.Height > int64(len(blocks)) {
				t.Fatalf("hit height %d outside the fixture", hit.Height)
			}
			block := blocks[hit.Height-1]
			if hit.BlockHash != block.Hash {
				t.Fatalf("hit at height %d reports hash %q, input block has %q", hit.Height, hit.BlockHash, block.Hash)
			}
			if hit.Position < 0 || hit.Position >= len(block.Txs) {
				t.Fatalf("hit position %d out of range in block %d (%d txs)", hit.Position, hit.Height, len(block.Txs))
			}
			if hit.TxID != block.Txs[hit.Position] {
				t.Fatalf("hit at %d[%d] is %q, input block has %q", hit.Height, hit.Position, hit.TxID, block.Txs[hit.Position])
			}
			if block.Time == nil {
				t.Fatalf("missing-timestamp block %d produced a time-filtered detail", hit.Height)
			}
		}
		stitched = append(stitched, page.Hits...)
	}
	if consumed != int64(len(oc.occ)) {
		t.Fatalf("stitched %d detail rows, oracle has %d occurrences", consumed, len(oc.occ))
	}
	if len(stitched) != len(oc.occ) {
		t.Fatalf("stitched rows=%d, oracle occurrences=%d", len(stitched), len(oc.occ))
	}
	for i := range stitched {
		oi := i
		if order == OrderDesc {
			oi = len(oc.occ) - 1 - i
		}
		got, want := stitched[i], oc.occ[oi]
		if got.Height != want.height || got.Position != want.pos || got.TxID != want.txID || got.BlockHash != want.hash {
			t.Fatalf("%s stitched row %d=%+v, want %+v", orderName(order), i, got, want)
		}
		if i > 0 {
			prev := stitched[i-1]
			if order == OrderAsc {
				if prev.Height > got.Height || (prev.Height == got.Height && prev.Position >= got.Position) {
					t.Fatalf("ascending rows out of order at %d: %+v after %+v", i, got, prev)
				}
			} else {
				if prev.Height < got.Height || (prev.Height == got.Height && prev.Position <= got.Position) {
					t.Fatalf("descending rows out of order at %d: %+v after %+v", i, got, prev)
				}
			}
		}
	}
	return stitched
}

func orderName(order TxOrder) string {
	if order == OrderDesc {
		return "descending"
	}
	return "ascending"
}

// corrAssertSameOccurrenceSet requires both read directions to return the
// same occurrence multiset; descending only reverses order, never content.
func corrAssertSameOccurrenceSet(t *testing.T, ascHits, descHits []TxHit) {
	t.Helper()
	asc, desc := corrKeys(ascHits), corrKeys(descHits)
	if len(asc) != len(desc) {
		t.Fatalf("asc returned %d occurrences, desc %d", len(asc), len(desc))
	}
	sort.Slice(asc, func(i, j int) bool {
		if asc[i].height != asc[j].height {
			return asc[i].height < asc[j].height
		}
		return asc[i].pos < asc[j].pos
	})
	sort.Slice(desc, func(i, j int) bool {
		if desc[i].height != desc[j].height {
			return desc[i].height < desc[j].height
		}
		return desc[i].pos < desc[j].pos
	})
	if !reflect.DeepEqual(asc, desc) {
		t.Fatalf("asc occurrence set %v != desc occurrence set %v", asc, desc)
	}
	// Descending stitched rows are precisely the ascending rows reversed:
	// same set, and block positions kept at their original values throughout.
	for i := range ascHits {
		a, d := ascHits[i], descHits[len(descHits)-1-i]
		if a != d {
			t.Fatalf("desc reverse mismatch at %d: asc %+v vs desc %+v", i, a, d)
		}
	}
}

// corrAssertDetailsMatchSegments re-buckets the stitched detail rows by the
// timestamps of the INPUT blocks, independently of QueryTimeStats' own scan,
// and requires every returned segment counter to match the details landing in
// it. It also re-derives the whole-window occurrence, block and distinct
// counts from the details alone.
func corrAssertDetailsMatchSegments(t *testing.T, blocks []Block, stats TimeStats, hits []TxHit) {
	t.Helper()
	segHits := make([][]TxHit, len(stats.Buckets))
	segBlocks := make([]map[string]struct{}, len(stats.Buckets))
	segIDs := make([]map[string]struct{}, len(stats.Buckets))
	for i := range segBlocks {
		segBlocks[i] = map[string]struct{}{}
		segIDs[i] = map[string]struct{}{}
	}
	windowBlocks := map[string]struct{}{}
	windowIDs := map[string]struct{}{}
	for _, hit := range hits {
		block := blocks[hit.Height-1]
		if block.Time == nil {
			t.Fatalf("detail row from missing-time block %d", hit.Height)
		}
		when := *block.Time
		idx := -1
		for i, seg := range stats.Buckets {
			if when >= seg.Start && when < seg.End {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Fatalf("detail at height %d time %d falls outside every returned segment", hit.Height, when)
		}
		segHits[idx] = append(segHits[idx], hit)
		segBlocks[idx][hit.BlockHash] = struct{}{}
		segIDs[idx][hit.TxID] = struct{}{}
		windowBlocks[hit.BlockHash] = struct{}{}
		windowIDs[hit.TxID] = struct{}{}
	}
	for i, seg := range stats.Buckets {
		if seg.TxCount != int64(len(segHits[i])) {
			t.Fatalf("segment [%d,%d) TxCount=%d but %d complete details land in it",
				seg.Start, seg.End, seg.TxCount, len(segHits[i]))
		}
		if seg.Blocks != int64(len(segBlocks[i])) {
			t.Fatalf("segment [%d,%d) Blocks=%d but details touch %d distinct blocks",
				seg.Start, seg.End, seg.Blocks, len(segBlocks[i]))
		}
		if seg.DistinctTxIDs != int64(len(segIDs[i])) {
			t.Fatalf("segment [%d,%d) DistinctTxIDs=%d but details hold %d distinct ids",
				seg.Start, seg.End, seg.DistinctTxIDs, len(segIDs[i]))
		}
	}
	if stats.Totals.TxCount != int64(len(hits)) {
		t.Fatalf("Totals.TxCount=%d but the complete details hold %d rows", stats.Totals.TxCount, len(hits))
	}
	if stats.Totals.Blocks != int64(len(windowBlocks)) {
		t.Fatalf("Totals.Blocks=%d but details touch %d distinct blocks", stats.Totals.Blocks, len(windowBlocks))
	}
	if stats.Totals.DistinctTxIDs != int64(len(windowIDs)) {
		t.Fatalf("Totals.DistinctTxIDs=%d but details hold %d distinct ids", stats.Totals.DistinctTxIDs, len(windowIDs))
	}
}

// TestQueryTxsTimeStatsCorrespondence is the end-to-end regression: over a
// matrix of scenarios it runs QueryTimeStats once and reads ALL QueryTxs
// pages in both directions (resizing pages along the way), then requires the
// oracle, the statistics, and the stitched details to agree per segment and
// over the whole window.
func TestQueryTxsTimeStatsCorrespondence(t *testing.T) {
	rich := corrRichBlocks()
	scenarios := []corrScenario{
		{
			name:   "unfiltered window four segments with all edge shapes",
			blocks: rich, start: 0, end: 200, step: 50,
			firstPage: 3, pageSizes: []int{3, 2, 5, 1, 4},
			anchors: corrAnchors{
				total: 12, matchedBlocks: 5, missing: 2,
				windowDistinct: 4, segmentDistinctSum: 9,
				counts: []int64{4, 2, 4, 2},
			},
		},
		{
			name:   "filter a repeated across blocks and segments",
			blocks: rich, txIDs: []string{"a"}, start: 0, end: 200, step: 50,
			firstPage: 2, pageSizes: []int{2, 3, 1},
			anchors: corrAnchors{
				total: 6, matchedBlocks: 4, missing: 2,
				windowDistinct: 1, segmentDistinctSum: 3,
				counts: []int64{2, 0, 3, 1},
			},
		},
		{
			name:   "filter set with duplicates is canonical",
			blocks: rich, txIDs: []string{"c", "a", "c", "a"}, start: 0, end: 200, step: 50,
			firstPage: 2, pageSizes: []int{2, 4, 2},
			anchors: corrAnchors{
				total: 8, matchedBlocks: 5, missing: 2,
				windowDistinct: 2, segmentDistinctSum: 5,
				counts: []int64{2, 1, 4, 1},
			},
		},
		{
			name:   "truncated final segment and a zero middle segment",
			blocks: rich, start: 100, end: 165, step: 30,
			firstPage: 2, pageSizes: []int{2, 4, 1},
			anchors: corrAnchors{
				total: 6, matchedBlocks: 3, missing: 2,
				windowDistinct: 3, segmentDistinctSum: 4,
				counts: []int64{4, 0, 2},
			},
		},
		{
			name:   "height range intersects the window",
			blocks: rich, from: 3, to: 5, start: 0, end: 200, step: 50,
			firstPage: 1, pageSizes: []int{1, 3},
			anchors: corrAnchors{
				total: 3, matchedBlocks: 2, missing: 1,
				windowDistinct: 3, segmentDistinctSum: 3,
				counts: []int64{0, 2, 1, 0},
			},
		},
		{
			name:   "only the empty identifier survives",
			blocks: rich, txIDs: []string{"", ""}, start: 0, end: 200, step: 50,
			firstPage: 1, pageSizes: []int{1},
			anchors: corrAnchors{
				total: 1, matchedBlocks: 1, missing: 2,
				windowDistinct: 1, segmentDistinctSum: 1,
				counts: []int64{1, 0, 0, 0},
			},
		},
		{
			name:   "tip bound resolves the zero to height",
			blocks: rich, to: 0, start: 0, end: 200, step: 50,
			firstPage: 5, pageSizes: []int{5, 2, 7},
			anchors: corrAnchors{
				total: 12, matchedBlocks: 5, missing: 2,
				windowDistinct: 4, segmentDistinctSum: 9,
				counts: []int64{4, 2, 4, 2},
			},
		},
		{
			name:   "legal filter without a match is an empty answer",
			blocks: rich, txIDs: []string{"zzz"}, start: 0, end: 200, step: 50,
			firstPage: 10, pageSizes: []int{10},
			anchors: corrAnchors{
				total: 0, matchedBlocks: 0, missing: 2,
				windowDistinct: 0, segmentDistinctSum: 0,
				counts: []int64{0, 0, 0, 0},
			},
		},
		{
			name:   "real zero timestamp joins the zero-start segment",
			blocks: rich, start: 0, end: 1, step: 1,
			firstPage: 2, pageSizes: []int{2, 3},
			anchors: corrAnchors{
				total: 4, matchedBlocks: 1, missing: 2,
				windowDistinct: 3, segmentDistinctSum: 3,
				counts: []int64{4},
			},
		},
		{
			name:   "decreasing timestamp block is still found",
			blocks: rich, start: 90, end: 100, step: 5,
			firstPage: 1, pageSizes: []int{1, 2},
			anchors: corrAnchors{
				// h4 at time 90 holds b,c in the first segment [90,95);
				// h2/h5 at the excluded end 100 do not, even though their
				// heights are higher, so the second segment is zero.
				total: 2, matchedBlocks: 1, missing: 2,
				windowDistinct: 2, segmentDistinctSum: 2,
				counts: []int64{2, 0},
			},
		},
	}

	for _, s := range scenarios {
		t.Run(s.name, func(t *testing.T) {
			index := corrBuildIndex(t, s.blocks)
			oc := corrBuildOracle(t, s)
			stats, err := index.QueryTimeStats(TimeStatsQuery{
				From: s.from, To: s.to, TxIDs: s.txIDs,
				Start: s.start, End: s.end, StepSeconds: s.step,
			})
			if err != nil {
				t.Fatalf("QueryTimeStats failed: %v", err)
			}
			corrAssertStats(t, stats, oc, s.anchors)

			base := TxQuery{
				From: s.from, To: s.to, TxIDs: s.txIDs,
				TimeStart: intptr(s.start), TimeEnd: intptr(s.end),
			}
			sizes := append([]int{s.firstPage}, s.pageSizes...)
			ascPages := corrCollectPages(t, index, base, s.firstPage, s.pageSizes)
			ascHits := corrAssertPages(t, s.blocks, oc, ascPages, OrderAsc, sizes)
			corrAssertDetailsMatchSegments(t, s.blocks, stats, ascHits)

			descBase := base
			descBase.Order = OrderDesc
			descPages := corrCollectPages(t, index, descBase, s.firstPage, s.pageSizes)
			descHits := corrAssertPages(t, s.blocks, oc, descPages, OrderDesc, sizes)
			corrAssertDetailsMatchSegments(t, s.blocks, stats, descHits)

			corrAssertSameOccurrenceSet(t, ascHits, descHits)
		})
	}
}

// TestQueryTxsTimeStatsFilterSpellingCorrespondence pins the identifier-set
// semantics through BOTH queries at once: nil/empty/{""} are three different
// things, order and duplicates are irrelevant, and case plus surrounding
// whitespace keep matching exactly. Equivalent spellings must produce
// byte-identical statistics and identical complete details.
func TestQueryTxsTimeStatsFilterSpellingCorrespondence(t *testing.T) {
	blocks := []Block{
		{Height: 1, Hash: "h1", Parent: "g", Txs: []string{"a", "a", "", "A", " a "}, Time: intptr(0)},
		{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"", "a"}, Time: intptr(10)},
		{Height: 3, Hash: "h3", Parent: "h2", Txs: []string{"a", "", "zzz"}, Time: nil},
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"x"}, Time: intptr(20)},
	}
	index := corrBuildIndex(t, blocks)
	const start, end, step = int64(0), int64(20), int64(10)

	statsOf := func(t *testing.T, txIDs []string) TimeStats {
		t.Helper()
		stats, err := index.QueryTimeStats(TimeStatsQuery{
			TxIDs: txIDs, Start: start, End: end, StepSeconds: step,
		})
		if err != nil {
			t.Fatalf("stats for %q failed: %v", txIDs, err)
		}
		return stats
	}
	detailsOf := func(t *testing.T, txIDs []string) []TxHit {
		t.Helper()
		pages := corrCollectPages(t, index, TxQuery{
			TxIDs: txIDs, TimeStart: intptr(start), TimeEnd: intptr(end), PageSize: MaxPageSize,
		}, MaxPageSize, nil)
		var hits []TxHit
		for _, p := range pages {
			hits = append(hits, p.Hits...)
		}
		return hits
	}

	// Explicit anchors: totals (occurrences, distinct, blocks), per-segment
	// occurrence counts, and the missing-time block which ignores filters.
	cases := []struct {
		name       string
		txIDs      []string
		total      int64
		distinct   int64
		blocks     int64
		segCounts  [2]int64
		detailRows int
	}{
		{"nil set is unfiltered", nil, 7, 4, 2, [2]int64{5, 2}, 7},
		{"empty slice is unfiltered", []string{}, 7, 4, 2, [2]int64{5, 2}, 7},
		{"sole empty string", []string{""}, 2, 1, 2, [2]int64{1, 1}, 2},
		{"lowercase a", []string{"a"}, 3, 1, 2, [2]int64{2, 1}, 3},
		{"a and empty", []string{"a", ""}, 5, 2, 2, [2]int64{3, 2}, 5},
		{"uppercase A is distinct", []string{"A"}, 1, 1, 1, [2]int64{1, 0}, 1},
		{"padded a is distinct", []string{" a "}, 1, 1, 1, [2]int64{1, 0}, 1},
		{"trailing space never trims", []string{"a "}, 0, 0, 0, [2]int64{0, 0}, 0},
		{"boundaries matter", []string{"ab"}, 0, 0, 0, [2]int64{0, 0}, 0},
		{"absent id", []string{"nope"}, 0, 0, 0, [2]int64{0, 0}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := corrScenario{blocks: blocks, txIDs: tc.txIDs, start: start, end: end, step: step, firstPage: MaxPageSize}
			oc := corrBuildOracle(t, s)
			stats := statsOf(t, tc.txIDs)
			if stats.MissingTimeBlocks != 1 {
				t.Fatalf("MissingTimeBlocks=%d, want 1 regardless of filter", stats.MissingTimeBlocks)
			}
			var segDistinctSum int64
			for _, seg := range oc.segments {
				segDistinctSum += int64(len(seg.ids))
			}
			corrAssertStats(t, stats, oc, corrAnchors{
				total: tc.total, matchedBlocks: tc.blocks, missing: 1,
				windowDistinct: tc.distinct, segmentDistinctSum: segDistinctSum,
				counts: tc.segCounts[:],
			})
			hits := detailsOf(t, tc.txIDs)
			if len(hits) != tc.detailRows {
				t.Fatalf("complete details=%d rows, want %d", len(hits), tc.detailRows)
			}
			corrAssertDetailsMatchSegments(t, blocks, stats, hits)
			// Cross-query identity on the page counters themselves.
			page, err := index.QueryTxs(TxQuery{
				TxIDs: tc.txIDs, TimeStart: intptr(start), TimeEnd: intptr(end),
			})
			if err != nil {
				t.Fatal(err)
			}
			if page.TotalMatches != stats.Totals.TxCount || page.MatchedBlocks != stats.Totals.Blocks {
				t.Fatalf("page counters total=%d/blocks=%d disagree with stats %d/%d",
					page.TotalMatches, page.MatchedBlocks, stats.Totals.TxCount, stats.Totals.Blocks)
			}
		})
	}

	// Equivalent spellings must be indistinguishable in both read models,
	// while the caller's slice keeps its contents (exercised elsewhere for
	// stats; here the results must compare equal).
	equivalent := [][]string{
		{"a", ""},
		{"", "a"},
		{"", "a", "a"},
		{"a", "", "a", "", "a"},
		{"a", "a", "", ""},
	}
	first := statsOf(t, equivalent[0])
	firstDetails := detailsOf(t, equivalent[0])
	for _, spelling := range equivalent[1:] {
		stats := statsOf(t, spelling)
		if !reflect.DeepEqual(stats, first) {
			t.Fatalf("stats for %q differ from %q:\n%+v\n%+v", spelling, equivalent[0], stats, first)
		}
		hits := detailsOf(t, spelling)
		if !reflect.DeepEqual(hits, firstDetails) {
			t.Fatalf("details for %q differ from %q", spelling, equivalent[0])
		}
	}

	// The no-filter spellings must not be accepted as the {""} filter by
	// either read model: their answers differ outright.
	unfiltered := statsOf(t, nil)
	emptyOnly := statsOf(t, []string{""})
	if reflect.DeepEqual(unfiltered, emptyOnly) {
		t.Fatal("nil set and {\"\"} produced identical statistics")
	}
	unfilteredDetails := detailsOf(t, nil)
	emptyDetails := detailsOf(t, []string{""})
	if reflect.DeepEqual(unfilteredDetails, emptyDetails) {
		t.Fatal("nil set and {\"\"} produced identical details")
	}
}

// TestQueryTxsTimeStatsNoMatchEmptyShapes pins the successful no-hit shapes:
// QueryTxs returns one empty page without a continuation cursor and zeroed
// whole-range counters, while QueryTimeStats still covers the whole window
// with zeroed segments, including an empty chain and a window no block time
// reaches.
func TestQueryTxsTimeStatsNoMatchEmptyShapes(t *testing.T) {
	rich := corrBuildIndex(t, corrRichBlocks())

	t.Run("filter matches nothing", func(t *testing.T) {
		page, err := rich.QueryTxs(TxQuery{
			TxIDs: []string{"zzz"}, TimeStart: intptr(0), TimeEnd: intptr(200),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Hits) != 0 || page.NextCursor != "" || page.TotalMatches != 0 || page.MatchedBlocks != 0 {
			t.Fatalf("expected an empty cursorless page, got %+v", page)
		}
		if page.ToHeight != 8 {
			t.Fatalf("ToHeight=%d, want pinned tip 8", page.ToHeight)
		}
		stats, err := rich.QueryTimeStats(TimeStatsQuery{TxIDs: []string{"zzz"}, Start: 0, End: 200, StepSeconds: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(stats.Buckets) != 4 || stats.Totals != (TimeTotals{}) || stats.MissingTimeBlocks != 2 {
			t.Fatalf("expected four zeroed segments, got %+v", stats)
		}
		for i, b := range stats.Buckets {
			if b.TxCount != 0 || b.Blocks != 0 || b.DistinctTxIDs != 0 {
				t.Fatalf("zero segment %d carries data: %+v", i, b)
			}
		}
	})

	t.Run("window reaches no block time", func(t *testing.T) {
		page, err := rich.QueryTxs(TxQuery{TimeStart: intptr(1000), TimeEnd: intptr(1100)})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Hits) != 0 || page.NextCursor != "" || page.TotalMatches != 0 {
			t.Fatalf("empty window page=%+v", page)
		}
		stats, err := rich.QueryTimeStats(TimeStatsQuery{Start: 1000, End: 1100, StepSeconds: 25})
		if err != nil {
			t.Fatal(err)
		}
		if len(stats.Buckets) != 4 {
			t.Fatalf("segments=%d, want 4 covering the whole empty window", len(stats.Buckets))
		}
		wantBounds := [][2]int64{{1000, 1025}, {1025, 1050}, {1050, 1075}, {1075, 1100}}
		for i, b := range stats.Buckets {
			if b.Start != wantBounds[i][0] || b.End != wantBounds[i][1] || b != (TimeBucket{Start: b.Start, End: b.End}) {
				t.Fatalf("zero segment %d=%+v", i, b)
			}
		}
		if stats.MissingTimeBlocks != 2 {
			t.Fatalf("missing=%d, blocks without timestamps still count", stats.MissingTimeBlocks)
		}
	})

	t.Run("empty chain", func(t *testing.T) {
		empty := New()
		page, err := empty.QueryTxs(TxQuery{TimeStart: intptr(0), TimeEnd: intptr(30)})
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Hits) != 0 || page.NextCursor != "" || page.ToHeight != 0 || page.TotalMatches != 0 {
			t.Fatalf("empty-chain page=%+v", page)
		}
		stats, err := empty.QueryTimeStats(TimeStatsQuery{Start: 0, End: 30, StepSeconds: 10})
		if err != nil {
			t.Fatal(err)
		}
		if len(stats.Buckets) != 3 || stats.FromHeight != 1 || stats.ToHeight != 0 || stats.Totals != (TimeTotals{}) {
			t.Fatalf("empty-chain stats=%+v, want three zeroed segments", stats)
		}
	})
}

// TestQueryTxsTimeStatsMissingTimestampHandling pins that missing-timestamp
// blocks disappear from time-filtered details and every segment while still
// being reported as MissingTimeBlocks over the selected height range, under
// every filter — and that they reappear in details once the window is
// disabled.
func TestQueryTxsTimeStatsMissingTimestampHandling(t *testing.T) {
	blocks := corrRichBlocks()
	index := corrBuildIndex(t, blocks)

	for name, txIDs := range map[string][]string{
		"unfiltered":       nil,
		"matching missing": {"z"}, // only held by missing-time h3
		"empty id":         {""},  // also held by missing-time h8
		"ordinary id":      {"a"},
		"no chain match":   {"qq"},
	} {
		t.Run(name, func(t *testing.T) {
			stats, err := index.QueryTimeStats(TimeStatsQuery{
				TxIDs: txIDs, Start: 0, End: 200, StepSeconds: 50,
			})
			if err != nil {
				t.Fatal(err)
			}
			if stats.MissingTimeBlocks != 2 {
				t.Fatalf("MissingTimeBlocks=%d, want 2 over the full range", stats.MissingTimeBlocks)
			}
			pages := corrCollectPages(t, index, TxQuery{
				TxIDs: txIDs, TimeStart: intptr(0), TimeEnd: intptr(200), PageSize: 3,
			}, 3, []int{2})
			var hits []TxHit
			for _, p := range pages {
				if p.TotalMatches != stats.Totals.TxCount || p.MatchedBlocks != stats.Totals.Blocks {
					t.Fatalf("page counters %d/%d != stats %d/%d",
						p.TotalMatches, p.MatchedBlocks, stats.Totals.TxCount, stats.Totals.Blocks)
				}
				hits = append(hits, p.Hits...)
			}
			for _, hit := range hits {
				if blocks[hit.Height-1].Time == nil {
					t.Fatalf("missing-time block %d entered the time-filtered details", hit.Height)
				}
			}
			if int64(len(hits)) != stats.Totals.TxCount {
				t.Fatalf("details %d rows != stats TxCount %d", len(hits), stats.Totals.TxCount)
			}
		})
	}

	// A height range containing only untimed blocks reports them missing but
	// yields zeroed segments and an empty page.
	ranged, err := index.QueryTimeStats(TimeStatsQuery{From: 8, To: 8, Start: 0, End: 200, StepSeconds: 50})
	if err != nil {
		t.Fatal(err)
	}
	if ranged.MissingTimeBlocks != 1 || ranged.Totals != (TimeTotals{}) || len(ranged.Buckets) != 4 {
		t.Fatalf("untimed-only range stats=%+v", ranged)
	}
	page, err := index.QueryTxs(TxQuery{From: 8, To: 8, TimeStart: intptr(0), TimeEnd: intptr(200)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 0 || page.NextCursor != "" || page.TotalMatches != 0 || page.MatchedBlocks != 0 {
		t.Fatalf("untimed-only range page=%+v", page)
	}

	// With the window disabled the untimed blocks return to the details: the
	// complete chain holds 16 occurrences across all 8 blocks.
	disabled, err := index.QueryTxs(TxQuery{PageSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	pages := collectPages(t, index, TxQuery{PageSize: 3})
	var allHits []TxHit
	for _, p := range pages {
		if p.TotalMatches != disabled.TotalMatches || p.MatchedBlocks != disabled.MatchedBlocks {
			t.Fatalf("disabled-window counters drifted across pages: %+v vs %+v", p, disabled)
		}
		allHits = append(allHits, p.Hits...)
	}
	if int64(len(allHits)) != 16 || disabled.TotalMatches != 16 || disabled.MatchedBlocks != 8 {
		t.Fatalf("disabled window: rows=%d total=%d blocks=%d, want 16/8", len(allHits), disabled.TotalMatches, disabled.MatchedBlocks)
	}
}

// TestQueryTxsTimeStatsRealZeroBoundaries pins the real-zero-vs-missing
// distinction jointly: a window starting at zero admits the zero block, a
// window starting above zero does not, and the untimed block is excluded from
// both while counted missing in statistics.
func TestQueryTxsTimeStatsRealZeroBoundaries(t *testing.T) {
	blocks := []Block{
		{Height: 1, Hash: "z1", Parent: "g", Txs: []string{"a", ""}, Time: intptr(0)},
		{Height: 2, Hash: "z2", Parent: "z1", Txs: []string{"a"}, Time: nil},
	}
	index := corrBuildIndex(t, blocks)

	included, err := index.QueryTimeStats(TimeStatsQuery{Start: 0, End: 1, StepSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if included.Buckets[0] != (TimeBucket{Start: 0, End: 1, TxCount: 2, DistinctTxIDs: 2, Blocks: 1}) ||
		included.MissingTimeBlocks != 1 || included.Totals.Blocks != 1 {
		t.Fatalf("zero-start stats=%+v", included)
	}
	page, err := index.QueryTxs(TxQuery{TimeStart: intptr(0), TimeEnd: intptr(1), PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if page.TotalMatches != 2 || page.MatchedBlocks != 1 {
		t.Fatalf("zero-start page=%+v", page)
	}
	// Two rows across two resized pages, positions kept as 0 and 1.
	query := TxQuery{TimeStart: intptr(0), TimeEnd: intptr(1), PageSize: 1}
	first, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hits[0].Position != 0 || len(first.Hits) != 1 {
		t.Fatalf("first zero-window row=%+v", first.Hits[0])
	}
	query.Cursor = first.NextCursor
	query.PageSize = 9
	rest, err := index.QueryTxs(query)
	if err != nil {
		t.Fatal(err)
	}
	if len(rest.Hits) != 1 || rest.Hits[0].Position != 1 || rest.NextCursor != "" ||
		rest.TotalMatches != 2 || rest.MatchedBlocks != 1 {
		t.Fatalf("resized zero-window continuation=%+v", rest)
	}

	// The same block is a non-match for a strictly-above-zero window; details
	// are empty while the untimed block is still reported missing.
	excluded, err := index.QueryTimeStats(TimeStatsQuery{Start: 1, End: 2, StepSeconds: 1})
	if err != nil {
		t.Fatal(err)
	}
	if excluded.Buckets[0].TxCount != 0 || excluded.Buckets[0].Blocks != 0 || excluded.MissingTimeBlocks != 1 {
		t.Fatalf("above-zero stats=%+v", excluded)
	}
	emptyPage, err := index.QueryTxs(TxQuery{TimeStart: intptr(1), TimeEnd: intptr(2)})
	if err != nil {
		t.Fatal(err)
	}
	if len(emptyPage.Hits) != 0 || emptyPage.NextCursor != "" || emptyPage.TotalMatches != 0 || emptyPage.MatchedBlocks != 0 {
		t.Fatalf("above-zero page=%+v, want an empty cursorless page", emptyPage)
	}
}
