package indexroom

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// This file guards an OrderDesc QueryTxs pagination across a snapshot Restore
// that lands while the walk is mid-pagination. The cursor binds the complete
// height range fixed by the first page and the block contents inside it — not
// the snapshot text those blocks arrived in, and not the transactions still
// unread. Restore replaces the whole main chain; a continuation therefore:
//
//   - keeps working when every block in the pinned range is data-identical
//     and the tip still covers the range, even when the restored document
//     reordered object fields, added layout whitespace, or spelled strings as
//     equivalent \uXXXX escapes;
//   - fails with ErrQueryChanged (never ErrInvalidArgument) and returns no
//     page at all when any in-range block content changed — including a block
//     the first page already read whose ONLY change is a missing timestamp
//     becoming a real zero, with hash, parent, and transaction list untouched;
//   - keeps working when the restore changes only block times OUTSIDE the
//     pinned range while the in-range contents stay byte-identical and the tip
//     still covers the range.
//
// The fixture is the product-brief example: heights 1..3 hold [a,b,a],
// [a,c], [b,a]; the query filters "a" over the explicit range [1,3] with
// OrderDesc and page size 2:
//
//	page 1: (height 3, position 1), (height 2, position 0)
//	page 2: (height 1, position 2), (height 1, position 0) — last page
//
// Both pages report TotalMatches 4, MatchedBlocks 3, ToHeight 3. Page 1 has
// already read height 2, which is what lets the missing-time-to-zero case
// exercise a change hidden behind unchanged query results: the unread
// occurrences on page 2 are byte-identical and must nevertheless be refused
// rather than stitched onto the old page.

var descRestoreQuery = TxQuery{
	From:     1,
	To:       3,
	TxIDs:    []string{"a"},
	PageSize: 2,
	Order:    OrderDesc,
}

var (
	descSpecPage1Hits = []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
	}
	descSpecPage2Hits = []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 2},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
	}
	descSpecAllHits = append(append([]TxHit{}, descSpecPage1Hits...), descSpecPage2Hits...)
)

// descRestoreSpecChain builds the three-block fixture chain through the
// ordinary Append API; none of its blocks carry a timestamp.
func descRestoreSpecChain(t *testing.T) *Index {
	t.Helper()
	return txChain(t,
		[]string{"a", "b", "a"},
		[]string{"a", "c"},
		[]string{"b", "a"},
	)
}

// descRestoreLongChain extends the fixture with heights 4..5 carrying real
// timestamps, so a query pinned to [1,3] has blocks above its range whose
// times a restore may change without touching the pinned contents.
func descRestoreLongChain(t *testing.T) *Index {
	t.Helper()
	index := descRestoreSpecChain(t)
	for _, block := range []Block{
		{Height: 4, Hash: "h4", Parent: "h3", Txs: []string{"z"}, Time: intptr(400)},
		{Height: 5, Hash: "h5", Parent: "h4", Txs: []string{"z"}, Time: intptr(500)},
	} {
		if err := index.Append(block); err != nil {
			t.Fatalf("setup append at %d: %v", block.Height, err)
		}
	}
	return index
}

// descRestoreReshapedV1Snapshot is a hand-written version-1 snapshot of the
// fixture chain whose text departs from a canonical export in every tolerated
// way at once: top-level and block field orders are shuffled, layout
// whitespace is added, and hashes/identifiers are partly spelled as
// equivalent \uXXXX escapes (a='a', g='g', h='h',
// 3='3'). Decoded, it is exactly the fixture chain with no timestamps.
func descRestoreReshapedV1Snapshot() string {
	a, g, h, three := uescape('a'), uescape('g'), uescape('h'), uescape('3')
	return "{\n  \"blocks\" : [\n" +
		"    { \"txs\" : [ \"" + a + "\", \"b\", \"" + a + "\" ], \"parent\" : \"" + g + "enesis\", \"hash\" : \"h1\", \"height\" : 1 },\n" +
		"    { \"height\" : 2, \"hash\" : \"" + h + "2\", \"parent\" : \"h1\", \"txs\" : [ \"" + a + "\", \"c\" ] },\n" +
		"    { \"parent\" : \"h2\", \"height\" : 3, \"hash\" : \"h" + three + "\", \"txs\" : [ \"b\", \"" + a + "\" ] }\n" +
		"  ],\n  \"tip\" : 3 ,\n  \"version\" : 1\n}\n"
}

// descRestoreZeroTimeV2Snapshot is a version-2 snapshot of the same three
// blocks with one in-range change: height 2's timestamp goes from missing
// (null) to a real zero (0). Its hash (spelled "h2"), parent, and
// transaction list are unchanged, as are heights 1 and 3. Height 2 is exactly
// the block page 1 has already read.
func descRestoreZeroTimeV2Snapshot() string {
	h, two := uescape('h'), uescape('2')
	return "{\n  \"tip\" : 3,\n  \"version\" : 2,\n  \"blocks\" : [\n" +
		"    { \"timestamp\" : null, \"txs\" : [ \"a\", \"b\", \"a\" ], \"parent\" : \"genesis\", \"hash\" : \"h1\", \"height\" : 1 },\n" +
		"    { \"height\" : 2, \"hash\" : \"" + h + "2\", \"parent\" : \"h1\", \"txs\" : [ \"a\", \"c\" ], \"timestamp\" : 0 },\n" +
		"    { \"timestamp\" : null, \"height\" : 3, \"hash\" : \"h3\", \"parent\" : \"h" + two + "\", \"txs\" : [ \"b\", \"a\" ] }\n" +
		"  ]\n}\n"
}

// descRestoreOutsideTimeV2Snapshot keeps heights 1..3 exactly as they are
// (timestamp null) and changes only the times of blocks above the pinned
// range: height 4 goes 400->999 and height 5 goes 500->501, hashes, parents,
// and transaction lists unchanged. The tip stays at 5, still covering [1,3].
func descRestoreOutsideTimeV2Snapshot() string {
	return "{\n  \"version\" : 2,\n  \"tip\" : 5,\n  \"blocks\" : [\n" +
		"    { \"timestamp\" : null, \"height\" : 1, \"hash\" : \"h1\", \"parent\" : \"genesis\", \"txs\" : [ \"a\", \"b\", \"a\" ] },\n" +
		"    { \"txs\" : [ \"a\", \"c\" ], \"timestamp\" : null, \"parent\" : \"h1\", \"hash\" : \"h2\", \"height\" : 2 },\n" +
		"    { \"height\" : 3, \"hash\" : \"h3\", \"parent\" : \"h2\", \"txs\" : [ \"b\", \"a\" ], \"timestamp\" : null },\n" +
		"    { \"height\" : 4, \"timestamp\" : 999, \"txs\" : [ \"z\" ], \"parent\" : \"h3\", \"hash\" : \"h" + uescape('4') + "\" },\n" +
		"    { \"height\" : 5, \"hash\" : \"h5\", \"parent\" : \"h4\", \"txs\" : [ \"z\" ], \"timestamp\" : 501 }\n" +
		"  ]\n}\n"
}

func requireDescSpecFirstPage(t *testing.T, page TxPage) string {
	t.Helper()
	if !reflect.DeepEqual(page.Hits, descSpecPage1Hits) {
		t.Fatalf("first desc page hits=%v, want %v", page.Hits, descSpecPage1Hits)
	}
	if page.TotalMatches != 4 || page.MatchedBlocks != 3 || page.ToHeight != 3 {
		t.Fatalf("first desc page stats=%+v, want total=4 blocks=3 to=3", page)
	}
	if page.NextCursor == "" {
		t.Fatal("first desc page over 4 matches with page size 2 must carry a cursor")
	}
	return page.NextCursor
}

func requireDescSpecFinalPage(t *testing.T, page TxPage) {
	t.Helper()
	if !reflect.DeepEqual(page.Hits, descSpecPage2Hits) {
		t.Fatalf("final desc page hits=%v, want %v", page.Hits, descSpecPage2Hits)
	}
	if page.TotalMatches != 4 || page.MatchedBlocks != 3 || page.ToHeight != 3 {
		t.Fatalf("final desc page stats=%+v, want total=4 blocks=3 to=3", page)
	}
	if page.NextCursor != "" {
		t.Fatalf("final desc page must carry no cursor, got %q", page.NextCursor)
	}
}

// Fixture self-check: the hand-written snapshots must decode to exactly the
// chains the cursor tests assume, and the "reshaped" text must genuinely
// differ from a canonical export while normalizing back to it. Without this,
// a typo in a fixture string could let the cursor tests pass for the wrong
// reason.
func TestDescRestoreSnapshotFixturesDecodeAsIntended(t *testing.T) {
	reference := descRestoreSpecChain(t)
	canonical := exportString(t, reference)
	reshaped := descRestoreReshapedV1Snapshot()
	pretty := prettyPrint(t, canonical)
	if reshaped == canonical {
		t.Fatal("test setup: reshaped snapshot is byte-identical to the canonical export")
	}
	if pretty == canonical {
		t.Fatal("test setup: pretty snapshot is byte-identical to the canonical export")
	}
	if !strings.Contains(reshaped, `\u`) || !strings.ContainsAny(reshaped, "\n") {
		t.Fatalf("test setup: reshaped snapshot must carry escapes and whitespace:\n%s", reshaped)
	}

	for name, input := range map[string]string{
		"canonical export":   canonical,
		"whitespace variant": pretty,
		"reshaped variant":   reshaped,
	} {
		index := New()
		if err := index.Restore(strings.NewReader(input)); err != nil {
			t.Fatalf("%s restore refused: %v", name, err)
		}
		requireSameChain(t, index, reference)
		// Same data normalizes to the same canonical bytes regardless of the
		// input field order, whitespace, or escape spelling.
		if got := exportString(t, index); got != canonical {
			t.Fatalf("%s re-export=%s, want %s", name, got, canonical)
		}
		for h := int64(1); h <= 3; h++ {
			if index.Blocks[h].Time != nil {
				t.Fatalf("%s: height %d gained a timestamp, want missing", name, h)
			}
		}
	}

	// The zero-time version-2 snapshot changes exactly one thing: height 2's
	// missing time becomes a real zero.
	zeroed := New()
	if err := zeroed.Restore(strings.NewReader(descRestoreZeroTimeV2Snapshot())); err != nil {
		t.Fatalf("zero-time snapshot refused: %v", err)
	}
	if got := zeroed.Blocks[2].Time; got == nil || *got != 0 {
		t.Fatalf("height 2 time=%v, want pointer to 0", got)
	}
	want2 := Block{Height: 2, Hash: "h2", Parent: "h1", Txs: []string{"a", "c"}, Time: intptr(0)}
	if !reflect.DeepEqual(zeroed.Blocks[2], want2) {
		t.Fatalf("height 2=%+v, want %+v", zeroed.Blocks[2], want2)
	}
	for _, h := range []int64{1, 3} {
		if zeroed.Blocks[h].Time != nil {
			t.Fatalf("height %d time=%v, want still missing", h, zeroed.Blocks[h].Time)
		}
	}
	if zeroed.Blocks[1].Hash != "h1" || zeroed.Blocks[3].Parent != "h2" {
		t.Fatalf("zero-time snapshot disturbed hashes or parents: %+v %+v", zeroed.Blocks[1], zeroed.Blocks[3])
	}

	// The out-of-range snapshot changes only heights 4..5 times; the pinned
	// blocks 1..3 stay data-identical (still missing their times).
	outside := New()
	if err := outside.Restore(strings.NewReader(descRestoreOutsideTimeV2Snapshot())); err != nil {
		t.Fatalf("out-of-range snapshot refused: %v", err)
	}
	if outside.Tip != 5 {
		t.Fatalf("tip=%d, want 5", outside.Tip)
	}
	for h := int64(1); h <= 3; h++ {
		if !reflect.DeepEqual(outside.Blocks[h], reference.Blocks[h]) {
			t.Fatalf("pinned height %d changed: got %+v want %+v", h, outside.Blocks[h], reference.Blocks[h])
		}
	}
	if got := *outside.Blocks[4].Time; got != 999 || outside.Blocks[4].Hash != "h4" {
		t.Fatalf("height 4=%+v, want time 999 with hash h4", outside.Blocks[4])
	}
	if got := *outside.Blocks[5].Time; got != 501 {
		t.Fatalf("height 5 time=%d, want 501", got)
	}
}

// After the first descending page, restoring a snapshot that is data-
// identical but textually different — canonical export, whitespace-only
// reformatting, or a reshaped document with reordered fields and equivalent
// Unicode escapes — leaves the pinned range and its contents untouched. The
// old cursor continues to the same final page with the same whole-range
// statistics: original block hashes and in-block positions are preserved,
// the repeated "a" at height 1 is not merged, and the already-read page-1
// occurrences do not reappear.
func TestQueryTxsDescCursorSurvivesDataEquivalentRestore(t *testing.T) {
	canonical := exportString(t, descRestoreSpecChain(t))
	variants := []struct {
		name     string
		snapshot string
	}{
		{"canonical export", canonical},
		{"whitespace only", prettyPrint(t, canonical)},
		{"reordered fields escapes and whitespace", descRestoreReshapedV1Snapshot()},
	}
	for _, variant := range variants {
		t.Run(variant.name, func(t *testing.T) {
			index := descRestoreSpecChain(t)
			first, err := index.QueryTxs(descRestoreQuery)
			if err != nil {
				t.Fatal(err)
			}
			cursor := requireDescSpecFirstPage(t, first)

			if err := index.Restore(strings.NewReader(variant.snapshot)); err != nil {
				t.Fatalf("restore refused: %v", err)
			}
			// The restored chain is the same data as a freshly built one,
			// including timestamp absence on every block.
			requireSameChain(t, index, descRestoreSpecChain(t))
			for h := int64(1); h <= 3; h++ {
				if index.Blocks[h].Time != nil {
					t.Fatalf("height %d time presence changed to %v, want still missing", h, index.Blocks[h].Time)
				}
			}

			continuation := descRestoreQuery
			continuation.Cursor = cursor
			second, err := index.QueryTxs(continuation)
			if err != nil {
				t.Fatalf("old cursor refused across a data-equivalent restore: %v", err)
			}
			requireDescSpecFinalPage(t, second)

			// The two pages concatenate to the full descending answer: each
			// occurrence exactly once, in its block with its original hash and
			// position, no page-1 record repeated and no occurrence skipped.
			all := append(append([]TxHit{}, first.Hits...), second.Hits...)
			if !reflect.DeepEqual(all, descSpecAllHits) {
				t.Fatalf("paginated desc hits=%v, want %v", all, descSpecAllHits)
			}
			// Statistics stayed pinned to the first page's fixed range.
			if first.TotalMatches != second.TotalMatches ||
				first.MatchedBlocks != second.MatchedBlocks ||
				first.ToHeight != second.ToHeight {
				t.Fatalf("stats drifted across restore: page1=%+v page2=%+v", first, second)
			}
		})
	}
}

// A change easily masked by identical query results: the height-2 block the
// first descending page has already read keeps its hash, parent, and
// transaction list, but its missing timestamp is restored as a real zero.
// The unread page-2 occurrences are byte-identical, yet the continuation
// must fail with ErrQueryChanged and a fully zeroed page — no hits, no
// cursor, no statistics — and must not be misreported as ErrInvalidArgument.
// The only recovery is a fresh first-page query; new results are never
// stitched onto the old page.
func TestQueryTxsDescCursorFailsWhenRestoreMakesReadBlockTimeZero(t *testing.T) {
	index := descRestoreSpecChain(t)
	first, err := index.QueryTxs(descRestoreQuery)
	if err != nil {
		t.Fatal(err)
	}
	cursor := requireDescSpecFirstPage(t, first)
	if index.Blocks[2].Time != nil {
		t.Fatalf("test setup: height 2 must start with a missing time")
	}

	if err := index.Restore(strings.NewReader(descRestoreZeroTimeV2Snapshot())); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	// Exactly one in-range content change: missing time -> real zero.
	block2 := index.Blocks[2]
	if block2.Time == nil || *block2.Time != 0 {
		t.Fatalf("height 2 time=%v, want real zero", block2.Time)
	}
	if block2.Hash != "h2" || block2.Parent != "h1" || !reflect.DeepEqual(block2.Txs, []string{"a", "c"}) {
		t.Fatalf("height 2 changed beyond its timestamp: %+v", block2)
	}
	for _, h := range []int64{1, 3} {
		if index.Blocks[h].Time != nil {
			t.Fatalf("height %d time=%v, want still missing", h, index.Blocks[h].Time)
		}
	}

	continuation := descRestoreQuery
	continuation.Cursor = cursor
	page, err := index.QueryTxs(continuation)
	if !errors.Is(err, ErrQueryChanged) {
		t.Fatalf("continuation err=%v, want ErrQueryChanged", err)
	}
	if errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("in-range data change reported as ErrInvalidArgument: %v", err)
	}
	if !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("changed continuation must return no usable page, got %+v", page)
	}

	// The stale cursor stays dead deterministically: the identical unread
	// transactions do not make a second attempt succeed.
	pageAgain, errAgain := index.QueryTxs(continuation)
	if !errors.Is(errAgain, ErrQueryChanged) || !reflect.DeepEqual(pageAgain, TxPage{}) {
		t.Fatalf("repeated stale cursor: err=%v page=%+v, want ErrQueryChanged and a zeroed page", errAgain, pageAgain)
	}

	// The only valid recovery is restarting from an empty first page, which
	// re-reads the whole range on the new chain and paginates to the end;
	// the remaining occurrences are never appended to the old page.
	fresh, err := index.QueryTxs(descRestoreQuery)
	if err != nil {
		t.Fatalf("fresh first page after change failed: %v", err)
	}
	requireDescSpecFirstPage(t, fresh)
	restart := descRestoreQuery
	restart.Cursor = fresh.NextCursor
	second, err := index.QueryTxs(restart)
	if err != nil {
		t.Fatalf("fresh pagination failed: %v", err)
	}
	requireDescSpecFinalPage(t, second)
}

// When the main chain extends above the pinned range and a restore changes
// only block times outside that range — in-range contents preserved and the
// tip still covering height 3 — the old descending cursor continues
// successfully, returning the same remaining records and the fixed-range
// statistics.
func TestQueryTxsDescCursorSurvivesRestoreChangingTimeOutsidePinnedRange(t *testing.T) {
	index := descRestoreLongChain(t)
	first, err := index.QueryTxs(descRestoreQuery)
	if err != nil {
		t.Fatal(err)
	}
	cursor := requireDescSpecFirstPage(t, first)

	// Snapshot the pinned blocks before the restore to compare afterwards.
	before := map[int64]Block{}
	for h := int64(1); h <= 3; h++ {
		before[h] = index.Blocks[h]
	}

	if err := index.Restore(strings.NewReader(descRestoreOutsideTimeV2Snapshot())); err != nil {
		t.Fatalf("restore refused: %v", err)
	}
	if index.Tip < 3 {
		t.Fatalf("tip=%d no longer covers the pinned upper bound 3", index.Tip)
	}
	for h := int64(1); h <= 3; h++ {
		if !reflect.DeepEqual(index.Blocks[h], before[h]) {
			t.Fatalf("pinned height %d changed: got %+v want %+v", h, index.Blocks[h], before[h])
		}
		if index.Blocks[h].Time != nil {
			t.Fatalf("pinned height %d gained a timestamp", h)
		}
	}
	// The out-of-range times did change; hashes, parents, and txs did not.
	if got := index.Blocks[4]; got.Time == nil || *got.Time != 999 || got.Hash != "h4" ||
		got.Parent != "h3" || !reflect.DeepEqual(got.Txs, []string{"z"}) {
		t.Fatalf("height 4=%+v, want only its time changed to 999", got)
	}
	if got := index.Blocks[5]; got.Time == nil || *got.Time != 501 {
		t.Fatalf("height 5=%+v, want time 501", got)
	}

	continuation := descRestoreQuery
	continuation.Cursor = cursor
	second, err := index.QueryTxs(continuation)
	if err != nil {
		t.Fatalf("out-of-range time change killed an in-range cursor: %v", err)
	}
	requireDescSpecFinalPage(t, second)

	all := append(append([]TxHit{}, first.Hits...), second.Hits...)
	if !reflect.DeepEqual(all, descSpecAllHits) {
		t.Fatalf("paginated desc hits=%v, want %v", all, descSpecAllHits)
	}
	if second.TotalMatches != 4 || second.MatchedBlocks != 3 || second.ToHeight != 3 {
		t.Fatalf("final page lost the pinned-range statistics: %+v", second)
	}
}
