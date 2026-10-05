package indexroom

import (
	"errors"
	"reflect"
	"testing"
)

// requireInvalidContinuation pins the public contract for a continuation
// whose filter no longer matches the first page: it is an argument problem,
// never a chain-data problem, and no page results are observable.
func requireInvalidContinuation(t *testing.T, index *Index, query TxQuery) {
	t.Helper()
	page, err := index.QueryTxs(query)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("continuation err=%v, want ErrInvalidArgument", err)
	}
	if errors.Is(err, ErrQueryChanged) {
		t.Fatalf("changed filter spelling reported as ErrQueryChanged: %v", err)
	}
	if !reflect.DeepEqual(page, TxPage{}) {
		t.Fatalf("rejected continuation returned page results: %+v", page)
	}
}

// TestQueryTxsFilterSetSpellingAcrossPages protects the rule that only the
// set of identifiers matters: reordered, deduplicated or re-duplicated
// spellings describe the same filter and must keep working with the original
// cursor. Every occurrence survives (including repeats inside one block), in
// height/position order, and whole-range statistics stay fixed.
func TestQueryTxsFilterSetSpellingAcrossPages(t *testing.T) {
	index := txChain(t,
		[]string{"a", "a", "b"},
		[]string{"a", "", ""},
		[]string{},
		[]string{"c", "a", "c", "a"},
		[]string{"", "a"},
	)
	want := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
		{Height: 4, BlockHash: "h4", TxID: "c", Position: 0},
		{Height: 4, BlockHash: "h4", TxID: "a", Position: 1},
		{Height: 4, BlockHash: "h4", TxID: "c", Position: 2},
		{Height: 4, BlockHash: "h4", TxID: "a", Position: 3},
		{Height: 5, BlockHash: "h5", TxID: "a", Position: 1},
	}

	// First page with a duplicated spelling and a page size of 3.
	first, err := index.QueryTxs(TxQuery{TxIDs: []string{"a", "c", "a"}, PageSize: 3})
	if err != nil {
		t.Fatal(err)
	}
	if first.NextCursor == "" {
		t.Fatal("expected a continuation cursor")
	}
	if !reflect.DeepEqual(first.Hits, want[:3]) {
		t.Fatalf("first page hits=%v, want %v", first.Hits, want[:3])
	}

	// Second page: same set reordered and deduplicated, resized to 2.
	second, err := index.QueryTxs(TxQuery{TxIDs: []string{"c", "a"}, PageSize: 2, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("reordered, deduplicated set refused: %v", err)
	}
	if !reflect.DeepEqual(second.Hits, want[3:5]) {
		t.Fatalf("second page hits=%v, want %v", second.Hits, want[3:5])
	}
	if second.NextCursor == "" {
		t.Fatal("expected another continuation cursor")
	}

	// Third page: same set with extra duplicates, resized to 3; it is last.
	third, err := index.QueryTxs(TxQuery{TxIDs: []string{"c", "c", "a", "a"}, PageSize: 3, Cursor: second.NextCursor})
	if err != nil {
		t.Fatalf("re-duplicated set refused: %v", err)
	}
	if !reflect.DeepEqual(third.Hits, want[5:]) {
		t.Fatalf("third page hits=%v, want %v", third.Hits, want[5:])
	}
	if third.NextCursor != "" {
		t.Fatalf("last page offered a cursor %q", third.NextCursor)
	}

	for i, page := range []TxPage{first, second, third} {
		if page.TotalMatches != 8 || page.MatchedBlocks != 4 || page.ToHeight != 5 {
			t.Fatalf("page %d stats=%+v, want total=8 blocks=4 to=5", i, page)
		}
	}

	got := append(append(append([]TxHit{}, first.Hits...), second.Hits...), third.Hits...)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stitched pages=%v, want %v", got, want)
	}

	// Cross-check: a single page over the same range must list exactly the
	// same occurrences, proving repeats within a block were not merged and
	// positions follow the original transaction list.
	single, err := index.QueryTxs(TxQuery{TxIDs: []string{"c", "a", "c"}, PageSize: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(single.Hits, want) {
		t.Fatalf("single-page hits=%v, want %v", single.Hits, want)
	}
}

// TestQueryTxsEmptySetAndEmptyIDDiffer protects three distinct meanings:
// no TxIDs field, an explicitly empty slice (both mean "no filter") and a
// set containing the empty string (only empty-identifier occurrences).
func TestQueryTxsEmptySetAndEmptyIDDiffer(t *testing.T) {
	index := txChain(t,
		[]string{"a", ""},
		[]string{""},
		[]string{"b"},
	)
	unfiltered := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "b", Position: 0},
	}
	emptyIDOnly := unfiltered[1:3]

	// No filter on the first page, an explicitly empty slice on continuation.
	first, err := index.QueryTxs(TxQuery{PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Hits, unfiltered[:1]) || first.TotalMatches != 4 || first.MatchedBlocks != 3 {
		t.Fatalf("unexpected unfiltered first page: %+v", first)
	}
	rest, err := index.QueryTxs(TxQuery{TxIDs: []string{}, PageSize: 5, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("empty slice must mean the same no-filter set: %v", err)
	}
	if !reflect.DeepEqual(rest.Hits, unfiltered[1:]) || rest.NextCursor != "" || rest.TotalMatches != 4 {
		t.Fatalf("empty-slice continuation mismatch: %+v", rest)
	}

	// Reverse spelling: an empty slice first, a nil field on continuation.
	firstEmpty, err := index.QueryTxs(TxQuery{TxIDs: []string{}, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	restNil, err := index.QueryTxs(TxQuery{PageSize: 2, Cursor: firstEmpty.NextCursor})
	if err != nil {
		t.Fatalf("nil TxIDs must mean the same no-filter set: %v", err)
	}
	if !reflect.DeepEqual(restNil.Hits, unfiltered[2:]) || restNil.NextCursor != "" {
		t.Fatalf("nil continuation mismatch: %+v", restNil)
	}

	// Filtering on the empty string returns only empty-identifier occurrences.
	emptyFirst, err := index.QueryTxs(TxQuery{TxIDs: []string{""}, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(emptyFirst.Hits, emptyIDOnly[:1]) || emptyFirst.TotalMatches != 2 || emptyFirst.MatchedBlocks != 2 {
		t.Fatalf("unexpected empty-id first page: %+v", emptyFirst)
	}
	// It must not be accepted as the no-filter cursor, in either direction.
	requireInvalidContinuation(t, index, TxQuery{PageSize: 1, Cursor: emptyFirst.NextCursor})
	requireInvalidContinuation(t, index, TxQuery{TxIDs: []string{}, PageSize: 1, Cursor: emptyFirst.NextCursor})
	requireInvalidContinuation(t, index, TxQuery{TxIDs: []string{""}, PageSize: 1, Cursor: first.NextCursor})
	// A duplicated empty string is the same one-element set and still works.
	emptyRest, err := index.QueryTxs(TxQuery{TxIDs: []string{"", ""}, PageSize: 5, Cursor: emptyFirst.NextCursor})
	if err != nil {
		t.Fatalf("duplicated empty-id spelling refused: %v", err)
	}
	if !reflect.DeepEqual(emptyRest.Hits, emptyIDOnly[1:]) || emptyRest.NextCursor != "" {
		t.Fatalf("empty-id continuation mismatch: %+v", emptyRest)
	}
}

// TestQueryTxsFilterBoundariesAreSignificant protects exact string matching:
// ["ab","c"] and ["a","bc"] are different filters even though their text
// concatenates identically, and a cursor must not cross between them.
func TestQueryTxsFilterBoundariesAreSignificant(t *testing.T) {
	index := txChain(t,
		[]string{"ab", "a"},
		[]string{"c", "bc"},
	)
	wantSplit := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "ab", Position: 0},
		{Height: 2, BlockHash: "h2", TxID: "c", Position: 0},
	}
	wantJoined := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "bc", Position: 1},
	}

	split, err := index.QueryTxs(TxQuery{TxIDs: []string{"ab", "c"}, PageSize: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	joined, err := index.QueryTxs(TxQuery{TxIDs: []string{"a", "bc"}, PageSize: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(split.Hits, wantSplit) || !reflect.DeepEqual(joined.Hits, wantJoined) {
		t.Fatalf("boundary-insensitive matching:\nsplit=%v\njoined=%v", split.Hits, joined.Hits)
	}

	first, err := index.QueryTxs(TxQuery{TxIDs: []string{"ab", "c"}, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Hits, wantSplit[:1]) || first.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", first)
	}
	// Same concatenated text, different string boundaries: a different filter.
	requireInvalidContinuation(t, index, TxQuery{TxIDs: []string{"a", "bc"}, PageSize: 1, Cursor: first.NextCursor})
	// Same set spelled in another order still drives the original cursor.
	rest, err := index.QueryTxs(TxQuery{TxIDs: []string{"c", "ab"}, PageSize: 1, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("equivalent boundary spelling refused: %v", err)
	}
	if !reflect.DeepEqual(rest.Hits, wantSplit[1:]) || rest.NextCursor != "" || rest.TotalMatches != 2 {
		t.Fatalf("continuation mismatch: %+v", rest)
	}
}

// TestQueryTxsChangedFilterSetIsArgumentError protects add/remove/replace of
// an identifier across continuation. The chain never changes, so the failure
// is ErrInvalidArgument rather than ErrQueryChanged; even when the added
// identifier matches nothing and both filters hit the same transactions, the
// request still describes different conditions and yields no page. The
// original cursor and its statistics keep working afterwards.
func TestQueryTxsChangedFilterSetIsArgumentError(t *testing.T) {
	index := txChain(t,
		[]string{"x", "a", "x"},
		[]string{"b"},
		[]string{"a", "b", "a"},
		[]string{"a"},
	)
	original := []string{"a", "b"}

	first, err := index.QueryTxs(TxQuery{TxIDs: original, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 1},
		{Height: 2, BlockHash: "h2", TxID: "b", Position: 0},
	}
	if !reflect.DeepEqual(first.Hits, wantFirst) || first.TotalMatches != 6 ||
		first.MatchedBlocks != 4 || first.ToHeight != 4 || first.NextCursor == "" {
		t.Fatalf("unexpected first page: %+v", first)
	}

	// The added identifier is absent from the whole pinned range, so a fresh
	// query with the wider set matches exactly the same transactions; the
	// continuation must still be rejected as a condition change.
	withAbsent, err := index.QueryTxs(TxQuery{TxIDs: []string{"a", "b", "z"}, PageSize: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	originalFull, err := index.QueryTxs(TxQuery{TxIDs: original, PageSize: MaxPageSize})
	if err != nil {
		t.Fatal(err)
	}
	if withAbsent.TotalMatches != originalFull.TotalMatches ||
		!reflect.DeepEqual(withAbsent.Hits, originalFull.Hits) {
		t.Fatalf("absent identifier changed matches:\n%v\n%v", withAbsent.Hits, originalFull.Hits)
	}

	blocks, byHash, tip := snapshot(index)
	for name, txIDs := range map[string][]string{
		"added identifier":    {"a", "b", "z"},
		"removed identifier":  {"a"},
		"replaced identifier": {"a", "z"},
	} {
		t.Run(name, func(t *testing.T) {
			requireInvalidContinuation(t, index, TxQuery{TxIDs: txIDs, PageSize: 2, Cursor: first.NextCursor})
		})
	}
	// Rejected continuations are read-only; the main chain is untouched.
	requireUnchanged(t, index, blocks, byHash, tip)

	// The original set, reordered, still reads the original next page.
	second, err := index.QueryTxs(TxQuery{TxIDs: []string{"b", "a"}, PageSize: 3, Cursor: first.NextCursor})
	if err != nil {
		t.Fatalf("original cursor refused after rejected continuations: %v", err)
	}
	wantSecond := []TxHit{
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
		{Height: 3, BlockHash: "h3", TxID: "b", Position: 1},
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 2},
	}
	if !reflect.DeepEqual(second.Hits, wantSecond) || second.NextCursor == "" {
		t.Fatalf("unexpected second page: %+v", second)
	}
	if second.TotalMatches != 6 || second.MatchedBlocks != 4 || second.ToHeight != 4 {
		t.Fatalf("statistics changed after rejected continuations: %+v", second)
	}

	// Finish the query with a re-duplicated spelling of the same set.
	third, err := index.QueryTxs(TxQuery{TxIDs: []string{"a", "b", "a"}, PageSize: 10, Cursor: second.NextCursor})
	if err != nil {
		t.Fatal(err)
	}
	wantThird := []TxHit{{Height: 4, BlockHash: "h4", TxID: "a", Position: 0}}
	if !reflect.DeepEqual(third.Hits, wantThird) || third.NextCursor != "" {
		t.Fatalf("unexpected final page: %+v", third)
	}
	if third.TotalMatches != 6 || third.MatchedBlocks != 4 || third.ToHeight != 4 {
		t.Fatalf("final page statistics changed: %+v", third)
	}
}
