package indexroom

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// This file is the automated regression for continuing an OrderDesc query
// across a snapshot Restore. A Restore replaces the whole main chain at once,
// so a cursor minted on the first descending page must be judged purely by the
// content of the range that first page pinned:
//
//   - the cursor binds the full height range fixed on page 1 together with the
//     block contents inside it (height, hash, parent, the ordered transaction
//     list, and the timestamp's presence and value) — not the snapshot's text
//     and not the occurrences the pages have yet to read;
//   - a snapshot whose data is identical — object fields reordered, layout
//     whitespace added, strings written as equivalent \uXXXX escapes — must
//     leave the old cursor usable, and the remaining pages must keep the
//     original block hashes and in-block positions, never repeat a record
//     already read, never merge duplicate in-block identifiers, and report the
//     same fixed-range statistics and timestamp presence/values;
//   - a snapshot that changes anything inside the pinned range — including
//     only turning an already-read block's missing timestamp into a real zero,
//     while its hash, parent, and transactions stay identical and the unread
//     transactions are unchanged — must fail the continuation with
//     ErrQueryChanged (never ErrInvalidArgument), returning no hits, no cursor,
//     and no statistics;
//   - a snapshot that changes only block timestamps above the pinned range,
//     while the in-range content is byte-identical and the tip still covers
//     the range, must leave the old cursor usable with the same remaining
//     records and fixed-range statistics.
//
// The shared fixture is the product-brief descending example: heights 1..3
// carry [a,b,a], [a,c], [b,a], the filter is {"a"}, the range is [1,3], and
// the page size is 2. Page 1 reads (height 3, position 1) and (height 2,
// position 0); page 2 must read (height 1, position 2) and (height 1,
// position 0) and then end. Every page reports TotalMatches 4,
// MatchedBlocks 3, ToHeight 3.

// descRestoreSpecBlocks builds the three-block spec fixture, each block given
// the timestamp (nil for a missing time) its caller wants.
func descRestoreSpecBlocks(t1, t2, t3 *int64) []Block {
	return []Block{
		{Height: 1, Hash: "g1", Parent: "genesis", Txs: []string{"a", "b", "a"}, Time: t1},
		{Height: 2, Hash: "g2", Parent: "g1", Txs: []string{"a", "c"}, Time: t2},
		{Height: 3, Hash: "g3", Parent: "g2", Txs: []string{"b", "a"}, Time: t3},
	}
}

func buildDescRestoreChain(t *testing.T, blocks []Block) *Index {
	t.Helper()
	idx := New()
	for _, block := range blocks {
		if err := idx.Append(block); err != nil {
			t.Fatalf("setup append at height %d: %v", block.Height, err)
		}
	}
	return idx
}

// descRestoreQuery is the fixed first-page request of the regression.
func descRestoreQuery() TxQuery {
	return TxQuery{From: 1, To: 3, TxIDs: []string{"a"}, PageSize: 2, Order: OrderDesc}
}

var (
	descRestoreWantPage1 = []TxHit{
		{Height: 3, BlockHash: "g3", TxID: "a", Position: 1},
		{Height: 2, BlockHash: "g2", TxID: "a", Position: 0},
	}
	descRestoreWantPage2 = []TxHit{
		{Height: 1, BlockHash: "g1", TxID: "a", Position: 2},
		{Height: 1, BlockHash: "g1", TxID: "a", Position: 0},
	}
)

func requireDescRestoreRangeStats(t *testing.T, page TxPage) {
	t.Helper()
	if page.TotalMatches != 4 || page.MatchedBlocks != 3 || page.ToHeight != 3 {
		t.Fatalf("fixed-range stats=%+v, want TotalMatches=4 MatchedBlocks=3 ToHeight=3", page)
	}
}

// requireDescRestoreTimes pins timestamp presence and exact value for the
// spec fixture: blocks 1 and 2 carry no time, block 3 carries Unix seconds
// want3 (nil means the block must have no time at all).
func requireDescRestoreTimes(t *testing.T, idx *Index, want3 int64) {
	t.Helper()
	if idx.Blocks[1].Time != nil {
		t.Fatalf("height 1 time=%v, want missing (nil)", idx.Blocks[1].Time)
	}
	if idx.Blocks[2].Time != nil {
		t.Fatalf("height 2 time=%v, want missing (nil)", idx.Blocks[2].Time)
	}
	if got := idx.Blocks[3].Time; got == nil || *got != want3 {
		t.Fatalf("height 3 time=%v, want pointer to %d", got, want3)
	}
}

// descRestoreEquivalentSnapshot is the spec chain as a version-2 snapshot
// whose TEXT departs from a canonical export in every tolerated way at once:
//   - top-level fields are reordered (tip/version before blocks, tip first);
//   - block-object fields are reordered (timestamp first; hash/parent mixed);
//   - whitespace is added throughout (newlines, spaces around ':' and ',',
//     spaces inside the arrays);
//   - the matching identifier "a" is written throughout with the equivalent
//     \uXXXX escape, and hashes/parents mix escapes into their digits
//     ("g2", "g3") and into "genesis" — all decode back to the
//     literal values;
//
// Decoded, it is exactly the three spec blocks: g1/genesis [a,b,a] with no
// time, g2/g1 [a,c] with no time, and g3/g2 [b,a] at Unix second 500. The
// escapes are produced with uescape (the same helper as the snapshot text
// tests) so the backslashes are real wire bytes, not Go source escapes.
func descRestoreEquivalentSnapshot() string {
	a := uescape('a') // "a" decodes to "a"
	g2 := "g" + uescape('2')
	g3 := "g" + uescape('3')
	genesis := uescape('g') + "enesis"
	return "{\n" +
		"  \"tip\" : 3 , \"version\" : 2,\n" +
		"  \"blocks\" :[\n" +
		"    { \"timestamp\" : null, \"hash\" : \"g1\", \"parent\" : \"" + genesis + "\", \"height\" : 1,\n" +
		"      \"txs\" : [ \"" + a + "\", \"b\", \"" + a + "\" ] },\n" +
		"    { \"height\" : 2, \"txs\" : [ \"" + a + "\", \"c\" ], \"parent\" : \"g1\", \"hash\" : \"" + g2 + "\", \"timestamp\" : null },\n" +
		"    { \"timestamp\" : 500, \"txs\" : [ \"b\" , \"" + a + "\" ], \"parent\" : \"" + g2 + "\",\n" +
		"      \"height\" : 3, \"hash\" : \"" + g3 + "\" }\n" +
		"  ]\n" +
		"}\n"
}

// The descending cursor stays usable after restoring an equivalent chain,
// however differently the snapshot text is laid out.
func TestQueryTxsDescCursorSurvivesEquivalentRestore(t *testing.T) {
	t3 := int64(500)
	specBlocks := descRestoreSpecBlocks(nil, nil, &t3)
	wantAll := append(append([]TxHit{}, descRestoreWantPage1...), descRestoreWantPage2...)

	// continueAfterEquivalentRestore takes the first descending page, restores
	// the given equivalent snapshot text, and walks the cursor to its end.
	continueAfterEquivalentRestore := func(t *testing.T, snapshotText string) *Index {
		t.Helper()
		idx := buildDescRestoreChain(t, specBlocks)
		query := descRestoreQuery()
		page1, err := idx.QueryTxs(query)
		if err != nil {
			t.Fatalf("first page failed: %v", err)
		}
		if !reflect.DeepEqual(page1.Hits, descRestoreWantPage1) {
			t.Fatalf("page1 hits=%v, want %v", page1.Hits, descRestoreWantPage1)
		}
		requireDescRestoreRangeStats(t, page1)
		if page1.NextCursor == "" {
			t.Fatal("page1 must carry a continuation cursor")
		}

		if err := idx.Restore(strings.NewReader(snapshotText)); err != nil {
			t.Fatalf("equivalent snapshot rejected: %v", err)
		}
		if idx.Tip != 3 {
			t.Fatalf("tip after equivalent restore=%d, want 3", idx.Tip)
		}
		// Presence and exact timestamp values survive the text reshape.
		requireDescRestoreTimes(t, idx, 500)

		query.Cursor = page1.NextCursor
		page2, err := idx.QueryTxs(query)
		if err != nil {
			t.Fatalf("continuation after equivalent restore failed: %v", err)
		}
		if !reflect.DeepEqual(page2.Hits, descRestoreWantPage2) {
			t.Fatalf("page2 hits=%v, want %v", page2.Hits, descRestoreWantPage2)
		}
		requireDescRestoreRangeStats(t, page2)
		if page2.NextCursor != "" {
			t.Fatalf("page2 must end the walk, got cursor %q", page2.NextCursor)
		}

		// The two pages partition the four occurrences: original hashes and
		// positions throughout, no occurrence read twice, duplicate "a"
		// occurrences inside block 1 kept distinct.
		seen := map[TxHit]bool{}
		for _, hit := range append(append([]TxHit{}, page1.Hits...), page2.Hits...) {
			if seen[hit] {
				t.Fatalf("occurrence %+v was returned more than once across the restore", hit)
			}
			seen[hit] = true
		}
		joined := append(append([]TxHit{}, page1.Hits...), page2.Hits...)
		if !reflect.DeepEqual(joined, wantAll) {
			t.Fatalf("paginated occurrences=%v, want %v", joined, wantAll)
		}
		return idx
	}

	t.Run("reordered fields added whitespace and unicode escapes", func(t *testing.T) {
		idx := continueAfterEquivalentRestore(t, descRestoreEquivalentSnapshot())
		// Same data normalizes to the same canonical bytes as an independently
		// built reference chain; the hand-written text shape is not retained.
		reference := buildDescRestoreChain(t, specBlocks)
		if got, want := exportString(t, idx), exportString(t, reference); got != want {
			t.Fatalf("re-export after reshaped restore differs:\n got %s\nwant %s", got, want)
		}
	})

	t.Run("pretty printed canonical export", func(t *testing.T) {
		idx := buildDescRestoreChain(t, specBlocks)
		canonical := exportString(t, idx)
		pretty := prettyPrint(t, canonical)
		if pretty == canonical {
			t.Fatal("setup: pretty snapshot must differ textually from the canonical export")
		}
		continueAfterEquivalentRestore(t, pretty)
	})

	t.Run("cursor survives several equivalent restores in a row", func(t *testing.T) {
		idx := buildDescRestoreChain(t, specBlocks)
		query := descRestoreQuery()
		page1, err := idx.QueryTxs(query)
		if err != nil {
			t.Fatal(err)
		}
		canonical := exportString(t, idx)
		// Three text shapes of the same data, applied one after another before
		// the single continuation is ever sent.
		texts := []string{
			descRestoreEquivalentSnapshot(), // reordered fields, whitespace, escapes
			prettyPrint(t, canonical),       // canonical layout, indented
			canonical,                       // byte-for-byte canonical
		}
		for i, text := range texts {
			if err := idx.Restore(strings.NewReader(text)); err != nil {
				t.Fatalf("equivalent restore %d rejected: %v", i, err)
			}
			requireDescRestoreTimes(t, idx, 500)
		}
		query.Cursor = page1.NextCursor
		page2, err := idx.QueryTxs(query)
		if err != nil {
			t.Fatalf("continuation after repeated equivalent restores failed: %v", err)
		}
		if !reflect.DeepEqual(page2.Hits, descRestoreWantPage2) {
			t.Fatalf("page2 hits=%v, want %v", page2.Hits, descRestoreWantPage2)
		}
		requireDescRestoreRangeStats(t, page2)
		if page2.NextCursor != "" {
			t.Fatalf("page2 must end the walk, got cursor %q", page2.NextCursor)
		}
	})
}

// A change the transaction results cannot mask: the first descending page
// already read height 2's match while height 2 had no timestamp. Restoring a
// chain that only turns that missing timestamp into a real zero — same hash,
// same parent, same transaction list, same tip, identical unread transactions
// — is chain data changing inside the pinned range, so the old cursor must
// fail with ErrQueryChanged and return nothing usable.
func TestQueryTxsDescCursorDiesWhenReadBlockMissingTimeBecomesZero(t *testing.T) {
	idx := buildDescRestoreChain(t, descRestoreSpecBlocks(nil, nil, nil))
	if idx.Blocks[2].Time != nil {
		t.Fatalf("setup: height 2 must start without a timestamp, got %v", idx.Blocks[2].Time)
	}

	query := descRestoreQuery()
	page1, err := idx.QueryTxs(query)
	if err != nil {
		t.Fatalf("first page failed: %v", err)
	}
	if !reflect.DeepEqual(page1.Hits, descRestoreWantPage1) {
		t.Fatalf("page1 hits=%v, want %v", page1.Hits, descRestoreWantPage1)
	}
	requireDescRestoreRangeStats(t, page1)
	if page1.NextCursor == "" {
		t.Fatal("page1 must carry a continuation cursor")
	}

	// Version-2 snapshot identical to the current chain except height 2's
	// timestamp: null (missing) becomes 0 (a real Unix-seconds zero). The
	// unread height-1 occurrences and every hash/parent/tx list are unchanged.
	const onlyHeight2Zero = `{"version":2,"tip":3,"blocks":[
		{"height":1,"hash":"g1","parent":"genesis","txs":["a","b","a"],"timestamp":null},
		{"height":2,"hash":"g2","parent":"g1","txs":["a","c"],"timestamp":0},
		{"height":3,"hash":"g3","parent":"g2","txs":["b","a"],"timestamp":null}
	]}`
	if err := idx.Restore(strings.NewReader(onlyHeight2Zero)); err != nil {
		t.Fatalf("restore with a real zero at height 2 rejected: %v", err)
	}
	if idx.Tip != 3 {
		t.Fatalf("tip=%d, want 3 (the restored chain still covers the range)", idx.Tip)
	}
	if got := idx.Blocks[2].Time; got == nil || *got != 0 {
		t.Fatalf("height 2 time=%v, want a pointer to a real zero", got)
	}
	if idx.Blocks[1].Time != nil || idx.Blocks[3].Time != nil {
		t.Fatalf("heights 1 and 3 must still lack timestamps: %v %v",
			idx.Blocks[1].Time, idx.Blocks[3].Time)
	}

	query.Cursor = page1.NextCursor
	page, err := idx.QueryTxs(query)
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("continuation err=%v, want ErrQueryChanged", err)
	}
	if errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("in-range data change reported as ErrInvalidArgument: %v", err)
	}
	if !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("changed continuation must return no hits, cursor, or stats, got %+v", page)
	}

	// Repeating the stale cursor fails identically: the height-1 occurrences
	// the old page 2 would have returned are byte-identical data, but they
	// must never be stitched onto the old first page.
	page, err = idx.QueryTxs(query)
	if !errors.Is(err, ErrQueryChanged) || !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("repeated stale cursor: err=%v page=%+v, want ErrQueryChanged and a zeroed page", err, page)
	}

	// The index itself is healthy: an empty-cursor first page on the restored
	// chain succeeds, sees the unchanged transaction occurrences, and still
	// reports the fixed-range totals over data that did change.
	fresh, err := idx.QueryTxs(descRestoreQuery())
	if err != nil {
		t.Fatalf("fresh first page after restore failed: %v", err)
	}
	if !reflect.DeepEqual(fresh.Hits, descRestoreWantPage1) {
		t.Fatalf("fresh page1 hits=%v, want %v", fresh.Hits, descRestoreWantPage1)
	}
	requireDescRestoreRangeStats(t, fresh)
	if fresh.NextCursor == "" {
		t.Fatal("fresh first page must carry a continuation cursor")
	}
}

// Timestamps changing only ABOVE the pinned range are invisible to the old
// cursor: the in-range content stays identical and the restored tip still
// covers height 3, so the descending walk continues with the same remaining
// records and fixed-range statistics. A matching transaction sitting in an
// out-of-range block must not leak in.
func TestQueryTxsDescCursorSurvivesOutOfRangeTimestampRestore(t *testing.T) {
	t4, t5 := int64(100), int64(200)
	blocks := append(descRestoreSpecBlocks(nil, nil, nil),
		// Height 4 even carries a matching "a" outside the pinned [1,3] range.
		Block{Height: 4, Hash: "g4", Parent: "g3", Txs: []string{"a"}, Time: &t4},
		Block{Height: 5, Hash: "g5", Parent: "g4", Txs: []string{"z"}, Time: &t5},
	)
	idx := buildDescRestoreChain(t, blocks)

	query := descRestoreQuery() // explicitly pins [1,3] despite the tip being 5
	page1, err := idx.QueryTxs(query)
	if err != nil {
		t.Fatalf("first page failed: %v", err)
	}
	if !reflect.DeepEqual(page1.Hits, descRestoreWantPage1) {
		t.Fatalf("page1 hits=%v, want %v", page1.Hits, descRestoreWantPage1)
	}
	requireDescRestoreRangeStats(t, page1)
	if page1.NextCursor == "" {
		t.Fatal("page1 must carry a continuation cursor")
	}

	// Restore keeps heights 1..3 byte-identical (still missing timestamps) and
	// changes only the timestamps of heights 4 and 5; the tip stays at 5 and
	// still covers the pinned range.
	const aboveRangeTimesChanged = `{"version":2,"tip":5,"blocks":[
		{"height":1,"hash":"g1","parent":"genesis","txs":["a","b","a"],"timestamp":null},
		{"height":2,"hash":"g2","parent":"g1","txs":["a","c"],"timestamp":null},
		{"height":3,"hash":"g3","parent":"g2","txs":["b","a"],"timestamp":null},
		{"height":4,"hash":"g4","parent":"g3","txs":["a"],"timestamp":777},
		{"height":5,"hash":"g5","parent":"g4","txs":["z"],"timestamp":888}
	]}`
	if err := idx.Restore(strings.NewReader(aboveRangeTimesChanged)); err != nil {
		t.Fatalf("out-of-range timestamp restore rejected: %v", err)
	}
	if idx.Tip != 5 {
		t.Fatalf("tip=%d, want 5 so the pinned range [1,3] stays covered", idx.Tip)
	}
	if got := idx.Blocks[4].Time; got == nil || *got != 777 {
		t.Fatalf("height 4 time=%v, want 777", got)
	}
	if got := idx.Blocks[5].Time; got == nil || *got != 888 {
		t.Fatalf("height 5 time=%v, want 888", got)
	}
	if idx.Blocks[1].Time != nil || idx.Blocks[2].Time != nil || idx.Blocks[3].Time != nil {
		t.Fatal("in-range timestamps must remain missing after the restore")
	}

	query.Cursor = page1.NextCursor
	page2, err := idx.QueryTxs(query)
	if err != nil {
		t.Fatalf("continuation after out-of-range time change failed: %v", err)
	}
	if !reflect.DeepEqual(page2.Hits, descRestoreWantPage2) {
		t.Fatalf("page2 hits=%v, want %v", page2.Hits, descRestoreWantPage2)
	}
	requireDescRestoreRangeStats(t, page2)
	if page2.NextCursor != "" {
		t.Fatalf("page2 must end the walk, got cursor %q", page2.NextCursor)
	}
	for _, hit := range page2.Hits {
		if hit.Height > 3 {
			t.Fatalf("out-of-range occurrence leaked into the continued page: %+v", hit)
		}
	}

	// The full walk still covers exactly the four pinned occurrences; the
	// matching "a" at height 4 never belonged to this query.
	joined := append(append([]TxHit{}, page1.Hits...), page2.Hits...)
	if len(joined) != 4 || int64(len(joined)) != page2.TotalMatches {
		t.Fatalf("continued walk yielded %d occurrences, want 4 pinned matches", len(joined))
	}
}
