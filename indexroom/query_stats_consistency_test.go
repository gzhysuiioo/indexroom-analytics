package indexroom

import (
	"fmt"
	"reflect"
	"testing"
)

// consistencyChain builds the shared fixture chain for the
// QueryTxs/QueryTimeStats consistency regression tests:
//
//	height 1: [a, b, a]      t=100  — one identifier twice in one block
//	height 2: [A, " a", "a "] t=95  — case/whitespace variants, time decreases with height
//	height 3: [b]            no timestamp
//	height 4: [a, c]         t=0    — a real zero timestamp
//	height 5: [c, c, ""]     t=109  — repeated identifier and an empty identifier
//	height 6: [b, a]         t=110  — sits at the excluded end of the usual window
//	height 7: [d]            t=105
func consistencyChain(t *testing.T) *Index {
	t.Helper()
	index := New()
	blocks := []struct {
		txs  []string
		time *int64
	}{
		{[]string{"a", "b", "a"}, intptr(100)},
		{[]string{"A", " a", "a "}, intptr(95)},
		{[]string{"b"}, nil},
		{[]string{"a", "c"}, intptr(0)},
		{[]string{"c", "c", ""}, intptr(109)},
		{[]string{"b", "a"}, intptr(110)},
		{[]string{"d"}, intptr(105)},
	}
	for i, entry := range blocks {
		height := int64(i + 1)
		parent := "genesis"
		if height > 1 {
			parent = fmt.Sprintf("h%d", height-1)
		}
		block := Block{
			Height: height,
			Hash:   fmt.Sprintf("h%d", height),
			Parent: parent,
			Txs:    entry.txs,
			Time:   entry.time,
		}
		if err := index.Append(block); err != nil {
			t.Fatalf("setup append at %d: %v", height, err)
		}
	}
	return index
}

// oracleScan recomputes the expected matching occurrences straight from the
// blocks stored in the index, without going through QueryTxs or
// QueryTimeStats, so a bug that makes both queries miss the same data still
// fails the test. The height range follows query semantics: a zero from
// means height 1, a zero to means the current tip. A nil window disables
// time filtering. It also reports how many blocks inside the resolved range
// carry no timestamp, mirroring TimeStats.MissingTimeBlocks.
func oracleScan(index *Index, from, to int64, txIDs []string, window *timeWindow) (hits []TxHit, missingTime int64) {
	if from == 0 {
		from = 1
	}
	if to == 0 || to > index.Tip {
		to = index.Tip
	}
	var set map[string]bool
	if len(txIDs) > 0 {
		set = make(map[string]bool, len(txIDs))
		for _, id := range txIDs {
			set[id] = true
		}
	}
	for height := from; height <= to; height++ {
		block := index.Blocks[height]
		if block.Time == nil {
			missingTime++
			if window != nil && window.enabled {
				// No timestamp: never matches an enabled window.
				continue
			}
		} else if window != nil && window.enabled && (*block.Time < window.start || *block.Time >= window.end) {
			continue
		}
		for position, tx := range block.Txs {
			if set != nil && !set[tx] {
				continue
			}
			hits = append(hits, TxHit{Height: height, BlockHash: block.Hash, TxID: tx, Position: position})
		}
	}
	return hits, missingTime
}

// summarizeHits counts the distinct blocks and distinct identifiers among
// the occurrences; every occurrence counts toward the total, so an
// identifier repeated inside one block raises the total but not the block
// count, and one recurring across blocks raises the window distinct count
// only once.
func summarizeHits(hits []TxHit) (blocks, distinct int64) {
	heightSet := make(map[int64]struct{})
	idSet := make(map[string]struct{})
	for _, hit := range hits {
		heightSet[hit.Height] = struct{}{}
		idSet[hit.TxID] = struct{}{}
	}
	return int64(len(heightSet)), int64(len(idSet))
}

// oracleBuckets distributes the windowed occurrences over the segments
// [start, start+step), ... cut off at end, computing every count from the
// stored block timestamps. The final segment may be narrower than step.
func oracleBuckets(index *Index, hits []TxHit, start, end, step int64) []TimeBucket {
	var buckets []TimeBucket
	for bStart := start; bStart < end; bStart += step {
		bEnd := bStart + step
		if bEnd > end {
			bEnd = end
		}
		buckets = append(buckets, TimeBucket{Start: bStart, End: bEnd})
	}
	distinct := make([]map[string]struct{}, len(buckets))
	blockSets := make([]map[int64]struct{}, len(buckets))
	for _, hit := range hits {
		when := index.Blocks[hit.Height].Time
		if when == nil {
			continue
		}
		i := (*when - start) / step
		buckets[i].TxCount++
		if distinct[i] == nil {
			distinct[i] = make(map[string]struct{})
			blockSets[i] = make(map[int64]struct{})
		}
		distinct[i][hit.TxID] = struct{}{}
		blockSets[i][hit.Height] = struct{}{}
	}
	for i := range buckets {
		buckets[i].DistinctTxIDs = int64(len(distinct[i]))
		buckets[i].Blocks = int64(len(blockSets[i]))
	}
	return buckets
}

// flattenHits concatenates the hits of every page in order.
func flattenHits(pages []TxPage) []TxHit {
	var hits []TxHit
	for _, page := range pages {
		hits = append(hits, page.Hits...)
	}
	return hits
}

// reversed returns the occurrences in the exact opposite order, the expected
// descending read of the same pinned range: heights and positions decrease,
// while each hit keeps its original block position.
func reversed(hits []TxHit) []TxHit {
	if hits == nil {
		return nil
	}
	out := make([]TxHit, len(hits))
	for i, hit := range hits {
		out[len(hits)-1-i] = hit
	}
	return out
}

// collectPagesResizing reads the whole query like collectPages but rotates
// through the given page sizes, so continuations exercise changing page
// lengths inside one pinned query.
func collectPagesResizing(t *testing.T, index *Index, query TxQuery, sizes []int) []TxPage {
	t.Helper()
	var pages []TxPage
	for i := 0; ; i++ {
		query.PageSize = sizes[i%len(sizes)]
		page, err := index.QueryTxs(query)
		if err != nil {
			t.Fatalf("query page %d failed: %v", i, err)
		}
		pages = append(pages, page)
		if page.NextCursor == "" {
			return pages
		}
		query.Cursor = page.NextCursor
	}
}

// assertConsistent pins one combination of height range, identifier set and
// time window, then checks that QueryTimeStats and the fully paginated
// QueryTxs agree with each other and with an independent recomputation from
// the stored blocks. Both read directions and rotated page sizes are
// exercised, and every stats bucket is cross-checked against a detail query
// over exactly its own sub-window.
func assertConsistent(t *testing.T, index *Index, from, to int64, txIDs []string, start, end, step int64) {
	t.Helper()

	window := &timeWindow{enabled: true, start: start, end: end}
	wantHits, wantMissing := oracleScan(index, from, to, txIDs, window)
	wantBlocks, wantDistinct := summarizeHits(wantHits)
	wantBuckets := oracleBuckets(index, wantHits, start, end, step)

	resolvedFrom := from
	if resolvedFrom == 0 {
		resolvedFrom = 1
	}
	resolvedTo := to
	if resolvedTo == 0 || resolvedTo > index.Tip {
		resolvedTo = index.Tip
	}

	// The statistics answer must match the oracle exactly: bucket bounds
	// cover the whole window, per-bucket counts come from the stored
	// blocks, totals deduplicate over the whole window, and missing-time
	// blocks are counted over the resolved height range.
	stats, err := index.QueryTimeStats(TimeStatsQuery{
		From: from, To: to, TxIDs: txIDs,
		Start: start, End: end, StepSeconds: step,
	})
	if err != nil {
		t.Fatalf("QueryTimeStats failed: %v", err)
	}
	if !reflect.DeepEqual(stats.Buckets, wantBuckets) {
		t.Fatalf("buckets=%+v, want %+v", stats.Buckets, wantBuckets)
	}
	wantTotals := TimeTotals{TxCount: int64(len(wantHits)), DistinctTxIDs: wantDistinct, Blocks: wantBlocks}
	if stats.Totals != wantTotals {
		t.Fatalf("totals=%+v, want %+v", stats.Totals, wantTotals)
	}
	if stats.FromHeight != resolvedFrom || stats.ToHeight != resolvedTo {
		t.Fatalf("resolved heights=[%d,%d], want [%d,%d]",
			stats.FromHeight, stats.ToHeight, resolvedFrom, resolvedTo)
	}
	if stats.MissingTimeBlocks != wantMissing {
		t.Fatalf("missing time blocks=%d, want %d", stats.MissingTimeBlocks, wantMissing)
	}
	// Occurrence and block totals are the sums of the bucket values (one
	// occurrence lands in exactly one bucket); the distinct total is the
	// window-wide dedup and may be smaller than the bucket sum.
	var sumTx, sumBlocks, sumDistinct int64
	for _, bucket := range stats.Buckets {
		sumTx += bucket.TxCount
		sumBlocks += bucket.Blocks
		sumDistinct += bucket.DistinctTxIDs
	}
	if sumTx != stats.Totals.TxCount || sumBlocks != stats.Totals.Blocks {
		t.Fatalf("bucket sums tx=%d blocks=%d, totals=%+v", sumTx, sumBlocks, stats.Totals)
	}
	if sumDistinct < stats.Totals.DistinctTxIDs {
		t.Fatalf("bucket distinct sum %d below window distinct %d", sumDistinct, stats.Totals.DistinctTxIDs)
	}

	// The paginated detail read, ascending with a rotating page size, must
	// produce exactly the oracle occurrences; every page must report the
	// whole-range totals, not just its own slice.
	base := TxQuery{From: from, To: to, TxIDs: txIDs, TimeStart: &start, TimeEnd: &end}
	pages := collectPagesResizing(t, index, base, []int{1, 3, 2, 5})
	if got := flattenHits(pages); !reflect.DeepEqual(got, wantHits) {
		t.Fatalf("ascending hits=%v, want %v", got, wantHits)
	}
	for i, page := range pages {
		if page.TotalMatches != int64(len(wantHits)) || page.MatchedBlocks != wantBlocks {
			t.Fatalf("page %d stats total=%d blocks=%d, want %d/%d",
				i, page.TotalMatches, page.MatchedBlocks, len(wantHits), wantBlocks)
		}
		if page.ToHeight != resolvedTo {
			t.Fatalf("page %d ToHeight=%d, want %d", i, page.ToHeight, resolvedTo)
		}
	}

	// Descending reads the same occurrence set the other way around,
	// positions inside a block kept at their original values.
	descQuery := base
	descQuery.Order = OrderDesc
	descPages := collectPagesResizing(t, index, descQuery, []int{2, 4, 1})
	if got, want := flattenHits(descPages), reversed(wantHits); !reflect.DeepEqual(got, want) {
		t.Fatalf("descending hits=%v, want %v", got, want)
	}
	for i, page := range descPages {
		if page.TotalMatches != int64(len(wantHits)) || page.MatchedBlocks != wantBlocks {
			t.Fatalf("desc page %d stats total=%d blocks=%d, want %d/%d",
				i, page.TotalMatches, page.MatchedBlocks, len(wantHits), wantBlocks)
		}
	}

	// Every bucket must equal the detail query restricted to that bucket's
	// own sub-window under the same height range and identifier set.
	for i, bucket := range stats.Buckets {
		bStart, bEnd := bucket.Start, bucket.End
		sub := TxQuery{From: from, To: to, TxIDs: txIDs, TimeStart: &bStart, TimeEnd: &bEnd}
		subHits := flattenHits(collectPages(t, index, sub))
		if int64(len(subHits)) != bucket.TxCount {
			t.Fatalf("bucket %d [%d,%d): details=%d, bucket TxCount=%d",
				i, bStart, bEnd, len(subHits), bucket.TxCount)
		}
		subBlocks, subDistinct := summarizeHits(subHits)
		if subBlocks != bucket.Blocks || subDistinct != bucket.DistinctTxIDs {
			t.Fatalf("bucket %d [%d,%d): detail blocks/ids=%d/%d, bucket=%d/%d",
				i, bStart, bEnd, subBlocks, subDistinct, bucket.Blocks, bucket.DistinctTxIDs)
		}
		// The sub-window details are exactly the oracle occurrences whose
		// block time falls inside this bucket.
		var wantSub []TxHit
		for _, hit := range wantHits {
			when := index.Blocks[hit.Height].Time
			if when != nil && *when >= bStart && *when < bEnd {
				wantSub = append(wantSub, hit)
			}
		}
		if !reflect.DeepEqual(subHits, wantSub) {
			t.Fatalf("bucket %d [%d,%d): hits=%v, want %v", i, bStart, bEnd, subHits, wantSub)
		}
	}
}

func TestQueryTxsAndTimeStatsConsistency(t *testing.T) {
	scenarios := []struct {
		name       string
		from, to   int64
		txIDs      []string
		start, end int64
		step       int64
	}{
		// Full range, no identifier filter: duplicates inside one block,
		// case/whitespace variants, the missing-time block, the real zero,
		// and the end-excluded block all participate.
		{"unfiltered", 0, 0, nil, 0, 110, 50},
		// A repeated, reordered identifier set canonicalizes to {a, b}.
		{"filtered set", 0, 0, []string{"b", "a", "b"}, 0, 110, 50},
		// An empty identifier list disables the filter; a list holding only
		// the empty string selects just the empty-identifier occurrence.
		{"empty string filter", 0, 0, []string{""}, 0, 110, 50},
		// Exact matching: case and surrounding whitespace matter.
		{"case sensitive", 0, 0, []string{"A"}, 0, 110, 50},
		{"whitespace sensitive", 0, 0, []string{" a", "a "}, 0, 110, 50},
		// A sub-range of heights: the missing-time block at height 3 falls
		// outside, the end-excluded block at height 6 stays excluded.
		{"height sub-range", 4, 6, nil, 0, 110, 50},
		// A window containing only the real zero timestamp.
		{"zero start window", 0, 0, nil, 0, 1, 1},
		// A legal window nothing falls into: empty details, zero buckets
		// still covering the whole window.
		{"no hits", 0, 0, []string{"a"}, 1000, 2000, 300},
		// One bucket per second over a narrow slice of the chain.
		{"fine buckets", 0, 0, nil, 94, 100, 1},
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			index := consistencyChain(t)
			assertConsistent(t, index, scenario.from, scenario.to,
				scenario.txIDs, scenario.start, scenario.end, scenario.step)
		})
	}
}

func TestQueryTxsAndTimeStatsConsistencyEmptyChain(t *testing.T) {
	// No blocks at all: the detail read is a single empty page without a
	// cursor, and the statistics still cover the whole window with zeros.
	index := New()
	assertConsistent(t, index, 0, 0, nil, 0, 100, 30)

	page, err := index.QueryTxs(TxQuery{TimeStart: intptr(0), TimeEnd: intptr(100)})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Hits) != 0 || page.NextCursor != "" || page.TotalMatches != 0 || page.MatchedBlocks != 0 {
		t.Fatalf("empty chain page=%+v, want an empty page without a cursor", page)
	}
}

func TestQueryTimeStatsWindowDistinctIsNotBucketSum(t *testing.T) {
	// "a" occurs at t=0 (height 4, bucket [0,50)) and twice at t=100
	// (height 1, bucket [100,110)): each bucket reports one distinct
	// identifier, but the window-wide distinct count stays one.
	index := consistencyChain(t)
	stats, err := index.QueryTimeStats(TimeStatsQuery{
		TxIDs: []string{"a"}, Start: 0, End: 110, StepSeconds: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	var bucketSum int64
	for _, bucket := range stats.Buckets {
		bucketSum += bucket.DistinctTxIDs
	}
	if bucketSum != 2 {
		t.Fatalf("bucket distinct sum=%d, want 2", bucketSum)
	}
	if stats.Totals.DistinctTxIDs != 1 {
		t.Fatalf("window distinct=%d, want 1 (not the bucket sum)", stats.Totals.DistinctTxIDs)
	}
	// The paginated details agree: three occurrences, one identifier.
	hits := flattenHits(collectPages(t, index, TxQuery{
		TxIDs: []string{"a"}, TimeStart: intptr(0), TimeEnd: intptr(110), PageSize: 1,
	}))
	_, distinct := summarizeHits(hits)
	if len(hits) != 3 || distinct != 1 || int64(len(hits)) != stats.Totals.TxCount {
		t.Fatalf("detail hits=%v distinct=%d, stats totals=%+v", hits, distinct, stats.Totals)
	}
}

func TestQueryTxsAndTimeStatsFilterSetSemantics(t *testing.T) {
	index := consistencyChain(t)

	// Order and repetitions inside the identifier set change nothing, for
	// the statistics and for the paginated details alike.
	base := TimeStatsQuery{TxIDs: []string{"b", "a"}, Start: 0, End: 110, StepSeconds: 50}
	want, err := index.QueryTimeStats(base)
	if err != nil {
		t.Fatal(err)
	}
	shuffled := base
	shuffled.TxIDs = []string{"a", "b", "a", "b"}
	got, err := index.QueryTimeStats(shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stats with reordered set=%+v, want %+v", got, want)
	}
	wantHits := flattenHits(collectPages(t, index, TxQuery{
		TxIDs: []string{"b", "a"}, TimeStart: intptr(0), TimeEnd: intptr(110),
	}))
	gotHits := flattenHits(collectPages(t, index, TxQuery{
		TxIDs: []string{"a", "b", "a"}, TimeStart: intptr(0), TimeEnd: intptr(110),
	}))
	if !reflect.DeepEqual(gotHits, wantHits) {
		t.Fatalf("hits with reordered set=%v, want %v", gotHits, wantHits)
	}

	// A cursor minted under one spelling of the set continues under
	// another spelling of the same set.
	first, err := index.QueryTxs(TxQuery{
		TxIDs: []string{"b", "a"}, TimeStart: intptr(0), TimeEnd: intptr(110), PageSize: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}
	if _, err := index.QueryTxs(TxQuery{
		TxIDs: []string{"a", "b", "a"}, TimeStart: intptr(0), TimeEnd: intptr(110),
		PageSize: 2, Cursor: first.NextCursor,
	}); err != nil {
		t.Fatalf("continuation with a respelled set failed: %v", err)
	}

	// No filter at all is not the same as filtering for the empty string:
	// the unrestricted read returns every occurrence, the {""} read only
	// the empty-identifier one at height 5.
	all, err := index.QueryTxs(TxQuery{TimeStart: intptr(0), TimeEnd: intptr(110)})
	if err != nil {
		t.Fatal(err)
	}
	emptyOnly, err := index.QueryTxs(TxQuery{
		TxIDs: []string{""}, TimeStart: intptr(0), TimeEnd: intptr(110),
	})
	if err != nil {
		t.Fatal(err)
	}
	if emptyOnly.TotalMatches != 1 || emptyOnly.MatchedBlocks != 1 {
		t.Fatalf("empty-string filter total=%d blocks=%d, want 1/1",
			emptyOnly.TotalMatches, emptyOnly.MatchedBlocks)
	}
	if all.TotalMatches == emptyOnly.TotalMatches {
		t.Fatalf("unrestricted total %d must differ from the empty-string filter", all.TotalMatches)
	}
	emptyStats, err := index.QueryTimeStats(TimeStatsQuery{
		TxIDs: []string{""}, Start: 0, End: 110, StepSeconds: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if emptyStats.Totals != (TimeTotals{TxCount: 1, DistinctTxIDs: 1, Blocks: 1}) {
		t.Fatalf("empty-string stats totals=%+v, want 1/1/1", emptyStats.Totals)
	}
}

func TestQueryTxsAndTimeStatsMissingTimeAndRealZero(t *testing.T) {
	index := consistencyChain(t)

	// The missing-time block at height 3 counts toward MissingTimeBlocks
	// under any identifier filter, even one that matches nothing, and
	// never enters a bucket.
	stats, err := index.QueryTimeStats(TimeStatsQuery{
		TxIDs: []string{"zzz"}, Start: 0, End: 110, StepSeconds: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stats.MissingTimeBlocks != 1 {
		t.Fatalf("missing time blocks=%d, want 1", stats.MissingTimeBlocks)
	}
	if stats.Totals != (TimeTotals{}) {
		t.Fatalf("totals=%+v, want all zero", stats.Totals)
	}

	// Restricting the height range to 4..6 excludes the missing-time
	// block from the count as well.
	sub, err := index.QueryTimeStats(TimeStatsQuery{From: 4, To: 6, Start: 0, End: 110, StepSeconds: 50})
	if err != nil {
		t.Fatal(err)
	}
	if sub.MissingTimeBlocks != 0 || sub.FromHeight != 4 || sub.ToHeight != 6 {
		t.Fatalf("sub-range stats=%+v, want no missing-time blocks over heights 4..6", sub)
	}

	// Without a time window the missing-time block's transactions are
	// listed (and the t=110 block, excluded only by the window, joins in);
	// with a window the missing-time block never matches.
	noWindow := flattenHits(collectPages(t, index, TxQuery{TxIDs: []string{"b"}}))
	if len(noWindow) != 3 || noWindow[0].Height != 1 || noWindow[1].Height != 3 || noWindow[2].Height != 6 {
		t.Fatalf("unwindowed b hits=%v, want heights 1, 3 and 6", noWindow)
	}
	windowed := flattenHits(collectPages(t, index, TxQuery{
		TxIDs: []string{"b"}, TimeStart: intptr(0), TimeEnd: intptr(110),
	}))
	if len(windowed) != 1 || windowed[0].Height != 1 {
		t.Fatalf("windowed b hits=%v, want only height 1", windowed)
	}

	// A real zero timestamp participates in a window containing zero.
	zero := flattenHits(collectPages(t, index, TxQuery{TimeStart: intptr(0), TimeEnd: intptr(1)}))
	if len(zero) != 2 || zero[0].Height != 4 || zero[1].Height != 4 {
		t.Fatalf("zero-window hits=%v, want the two occurrences at height 4", zero)
	}
}
