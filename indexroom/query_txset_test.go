package indexroom

import (
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// TestQueryTxsEquivalentSetPaginationsAgree protects the public rule for
// continuing a filtered query: only a filter set equivalent to the first
// request may reuse its cursor. Equivalence is set equality of the exact
// identifiers — reordering, dropping duplicates, or adding repetitions must
// describe the same filter, while every continuation may also pick a new page
// size. Whichever spelling is used, the stitched pages must equal one
// single-page filtered answer occurrence for occurrence: repeats of an
// identifier, including several copies inside one block, are never merged,
// positions index the original block transaction list, and every page reports
// the same whole-range totals and pinned upper bound.
func TestQueryTxsEquivalentSetPaginationsAgree(t *testing.T) {
	// Four blocks where the filtered identifiers recur, including three
	// occurrences of "a" inside the first block; "z" below never matches.
	index := txChain(t,
		[]string{"a", "b", "a", "z", "a", "c"}, // positions 0,2,4 for a; 1 b; 5 c
		[]string{"z", "b", "b"},                // b twice in one block, both kept
		[]string{},
		[]string{"c", "a", "z"},
	)
	const setA, setB = "a", "b"
	filter := []string{setA, setB}

	// Oracle: the whole filtered occurrence list in height/position order.
	want, wantBlocks := referenceScan(index, 1, index.Tip, filter)
	if len(want) != 7 || wantBlocks != 3 {
		t.Fatalf("oracle setup: hits=%d blocks=%d, want 7/3", len(want), wantBlocks)
	}
	wantPositions := []struct {
		height   int64
		position int
		tx       string
	}{
		{1, 0, "a"}, {1, 1, "b"}, {1, 2, "a"}, {1, 4, "a"},
		{2, 1, "b"}, {2, 2, "b"},
		{4, 1, "a"},
	}
	if len(want) != len(wantPositions) {
		t.Fatalf("oracle hit count %d, want %d", len(want), len(wantPositions))
	}
	for i, pos := range wantPositions {
		if want[i].Height != pos.height || want[i].Position != pos.position || want[i].TxID != pos.tx {
			t.Fatalf("oracle hit %d = %+v, want height %d position %d tx %q",
				i, want[i], pos.height, pos.position, pos.tx)
		}
	}

	// Equivalent spellings exercised as continuation inputs. The first page
	// starts from one spelling; later pages alternate through the others.
	spellings := [][]string{
		{"a", "b"},           // canonical order
		{"b", "a"},           // reordered
		{"a", "b", "a"},      // an extra repetition
		{"b", "a", "b", "a"}, // reordered and repeated
		{"a", "a", "b", "b"}, // fully duplicated
	}

	for _, pageSize := range []int{1, 2, 3, 7, MaxPageSize} {
		t.Run(fmt.Sprintf("pagesize=%d", pageSize), func(t *testing.T) {
			first, err := index.QueryTxs(TxQuery{TxIDs: []string{"a", "b", "b"}, PageSize: pageSize})
			if err != nil {
				t.Fatalf("first page: %v", err)
			}
			if first.TotalMatches != int64(len(want)) || first.MatchedBlocks != wantBlocks || first.ToHeight != 4 {
				t.Fatalf("first page stats=%+v, want total %d blocks %d to 4",
					first, len(want), wantBlocks)
			}
			if (first.NextCursor == "") != (pageSize >= len(want)) {
				t.Fatalf("first page cursor=%q with %d hits, want a cursor exactly while matches remain",
					first.NextCursor, len(first.Hits))
			}

			stitched := append([]TxHit{}, first.Hits...)
			query := TxQuery{PageSize: pageSize, Cursor: first.NextCursor}
			consumed := int64(len(first.Hits))
			for pageIdx := 1; query.Cursor != ""; pageIdx++ {
				// Same set under a different spelling, and a possibly
				// different page size on each continuation.
				query.TxIDs = spellings[pageIdx%len(spellings)]
				if pageIdx%2 == 1 {
					query.PageSize = pageSize + 1
					if query.PageSize > MaxPageSize {
						query.PageSize = pageSize
					}
				} else {
					query.PageSize = pageSize
				}
				page, err := index.QueryTxs(query)
				if err != nil {
					t.Fatalf("continuation %d with equivalent set %v refused: %v",
						pageIdx, query.TxIDs, err)
				}
				if page.TotalMatches != int64(len(want)) || page.MatchedBlocks != wantBlocks || page.ToHeight != 4 {
					t.Fatalf("continuation %d stats=%+v, want total %d blocks %d to 4",
						pageIdx, page, len(want), wantBlocks)
				}
				if len(page.Hits) > query.PageSize {
					t.Fatalf("continuation %d holds %d hits, page size %d",
						pageIdx, len(page.Hits), query.PageSize)
				}
				consumed += int64(len(page.Hits))
				// A non-final page must be exactly full; only the final page
				// may be short and must then carry no further cursor.
				if page.NextCursor != "" && len(page.Hits) != query.PageSize {
					t.Fatalf("continuation %d not full (%d/%d hits) yet returned a cursor",
						pageIdx, len(page.Hits), query.PageSize)
				}
				if (page.NextCursor == "") != (consumed == int64(len(want))) {
					t.Fatalf("continuation %d cursor=%q after %d/%d hits",
						pageIdx, page.NextCursor, consumed, len(want))
				}
				stitched = append(stitched, page.Hits...)
				query.Cursor = page.NextCursor
			}
			if consumed != int64(len(want)) {
				t.Fatalf("consumed %d hits, want %d", consumed, len(want))
			}
			if !reflect.DeepEqual(stitched, want) {
				t.Fatalf("equivalent-set pagination lost/reordered occurrences:\nstitched=%v\nwant=%v",
					stitched, want)
			}
		})
	}
}

// TestQueryTxsEmptySetAndEmptyIDAreDifferentFilters protects the distinction
// between "no filter" and "only the empty-string identifier": omitting TxIDs
// and supplying an empty (possibly nil) slice are the same unfiltered query
// and may share one cursor; supplying [""] is a real filter that returns only
// empty-identifier occurrences and must never be accepted as unfiltered.
func TestQueryTxsEmptySetAndEmptyIDAreDifferentFilters(t *testing.T) {
	index := txChain(t,
		[]string{"", "a", ""}, // two empty-identifier occurrences, positions 0 and 2
		[]string{"b"},
		[]string{""},
	)

	// First page with the filter omitted entirely.
	omitted, err := index.QueryTxs(TxQuery{PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	unfiltered, unfilteredBlocks := referenceScan(index, 1, index.Tip, nil)
	if omitted.TotalMatches != int64(len(unfiltered)) || omitted.MatchedBlocks != unfilteredBlocks {
		t.Fatalf("omitted filter page=%+v, want oracle total %d blocks %d",
			omitted, len(unfiltered), unfilteredBlocks)
	}
	if omitted.TotalMatches != 5 || omitted.MatchedBlocks != 3 {
		t.Fatalf("unfiltered totals=%d/%d, want 5/3", omitted.TotalMatches, omitted.MatchedBlocks)
	}

	// A nil slice and a non-nil empty slice are the same unfiltered filter and
	// must reuse the cursor minted for the omitted-TxIDs first page.
	for name, txIDs := range map[string][]string{
		"nil":         nil,
		"empty slice": {},
	} {
		page, err := index.QueryTxs(TxQuery{TxIDs: txIDs, PageSize: 2, Cursor: omitted.NextCursor})
		if err != nil {
			t.Fatalf("%s: empty set refused as continuation of omitted filter: %v", name, err)
		}
		if page.TotalMatches != 5 || page.MatchedBlocks != 3 {
			t.Fatalf("%s: continuation totals=%+v, want unfiltered 5/3", name, page)
		}
	}

	// Going the other direction: a first page started with an explicit empty
	// slice accepts a nil/omitted TxIDs continuation.
	emptyStart, err := index.QueryTxs(TxQuery{TxIDs: []string{}, PageSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if emptyStart.TotalMatches != 5 {
		t.Fatalf("empty-slice first page total=%d, want 5", emptyStart.TotalMatches)
	}
	if _, err := index.QueryTxs(TxQuery{PageSize: 2, Cursor: emptyStart.NextCursor}); err != nil {
		t.Fatalf("omitted-TxIDs continuation of empty-slice cursor refused: %v", err)
	}

	// Filtering on the empty string returns only "" occurrences: three of
	// them, in two blocks, at their real block positions.
	emptyID, err := index.QueryTxs(TxQuery{TxIDs: []string{""}})
	if err != nil {
		t.Fatal(err)
	}
	wantEmpty := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "", Position: 2},
		{Height: 3, BlockHash: "h3", TxID: "", Position: 0},
	}
	if !reflect.DeepEqual(emptyID.Hits, wantEmpty) {
		t.Fatalf("empty-id hits=%v, want %v", emptyID.Hits, wantEmpty)
	}
	if emptyID.TotalMatches != 3 || emptyID.MatchedBlocks != 2 || emptyID.NextCursor != "" {
		t.Fatalf("empty-id stats=%+v, want 3 matches/2 blocks/no cursor", emptyID)
	}

	// The empty-ID filter is not interchangeable with the unfiltered cursor,
	// in either direction.
	emptyIDFirst, err := index.QueryTxs(TxQuery{TxIDs: []string{""}, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	for name, query := range map[string]TxQuery{
		"unfiltered cursor, empty id given": {PageSize: 1, TxIDs: []string{""}, Cursor: omitted.NextCursor},
		"empty id cursor, no filter given":  {PageSize: 1, Cursor: emptyIDFirst.NextCursor},
	} {
		if _, err := index.QueryTxs(query); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: err=%v, want ErrInvalidArgument", name, err)
		}
	}
}

// TestQueryTxsStringBoundariesArePartOfTheFilter protects that identifiers are
// matched as whole strings: ["ab","c"] and ["a","bc"] are different filters
// even though their concatenation is identical, and neither spelling may
// continue the other's cursor.
func TestQueryTxsStringBoundariesArePartOfTheFilter(t *testing.T) {
	index := txChain(t,
		[]string{"ab", "c", "a", "bc"},
		[]string{"abc"},
		[]string{"a", "c"},
	)

	abC := []string{"ab", "c"}
	aBC := []string{"a", "bc"}

	pageABC, err := index.QueryTxs(TxQuery{TxIDs: abC})
	if err != nil {
		t.Fatal(err)
	}
	wantABC := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "ab", Position: 0},
		{Height: 1, BlockHash: "h1", TxID: "c", Position: 1},
		{Height: 3, BlockHash: "h3", TxID: "c", Position: 1},
	}
	if !reflect.DeepEqual(pageABC.Hits, wantABC) {
		t.Fatalf("[ab c] hits=%v, want %v", pageABC.Hits, wantABC)
	}
	if pageABC.TotalMatches != 3 || pageABC.MatchedBlocks != 2 {
		t.Fatalf("[ab c] stats=%+v, want 3/2", pageABC)
	}

	pageABitC, err := index.QueryTxs(TxQuery{TxIDs: aBC})
	if err != nil {
		t.Fatal(err)
	}
	wantABitC := []TxHit{
		{Height: 1, BlockHash: "h1", TxID: "a", Position: 2},
		{Height: 1, BlockHash: "h1", TxID: "bc", Position: 3},
		{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
	}
	if !reflect.DeepEqual(pageABitC.Hits, wantABitC) {
		t.Fatalf("[a bc] hits=%v, want %v", pageABitC.Hits, wantABitC)
	}
	if pageABitC.TotalMatches != 3 || pageABitC.MatchedBlocks != 2 {
		t.Fatalf("[a bc] stats=%+v, want 3/2", pageABitC)
	}
	// "abc" matches neither filter: concatenation is not containment.

	firstABC, err := index.QueryTxs(TxQuery{TxIDs: abC, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	firstABitC, err := index.QueryTxs(TxQuery{TxIDs: aBC, PageSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	for name, query := range map[string]TxQuery{
		"boundary moved forward": {TxIDs: aBC, PageSize: 1, Cursor: firstABC.NextCursor},
		"boundary moved back":    {TxIDs: abC, PageSize: 1, Cursor: firstABitC.NextCursor},
	} {
		page, err := index.QueryTxs(query)
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("%s: err=%v page=%+v, want ErrInvalidArgument with no page", name, err, page)
		}
	}

	// Reordered/duplicated spellings of each set stay on their own side of
	// the boundary and continue their own cursor.
	cont, err := index.QueryTxs(TxQuery{TxIDs: []string{"c", "ab", "ab"}, PageSize: 1, Cursor: firstABC.NextCursor})
	if err != nil {
		t.Fatalf("equivalent spelling of [ab c] refused: %v", err)
	}
	if cont.Hits[0] != wantABC[1] {
		t.Fatalf("continued [ab c] hit=%+v, want %+v", cont.Hits[0], wantABC[1])
	}
}

// TestQueryTxsContinuationRejectsChangedIdentifierSet protects that a
// continuation which genuinely adds, removes, or substitutes an identifier
// fails with ErrInvalidArgument (never ErrQueryChanged — the main chain is
// untouched) and returns no usable page. This holds even when the changed
// identifier has no occurrence in the pinned range, so both filters happen to
// hit the same transactions: the request conditions still changed. After the
// rejection, the original set and cursor must return the original next page,
// with chain content and query statistics exactly as before.
func TestQueryTxsContinuationRejectsChangedIdentifierSet(t *testing.T) {
	t.Run("add remove replace", func(t *testing.T) {
		index := txChain(t,
			[]string{"a", "b", "a"},
			[]string{"a"},
			[]string{"b", "a"},
		)
		// B1 a,b,a; B2 a; B3 b,a -> six occurrences across three blocks.
		first, err := index.QueryTxs(TxQuery{TxIDs: []string{"a", "b"}, PageSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		if first.NextCursor == "" || first.TotalMatches != 6 || first.MatchedBlocks != 3 {
			t.Fatalf("unexpected first page: %+v", first)
		}
		expectedNext := []TxHit{
			{Height: 1, BlockHash: "h1", TxID: "a", Position: 2},
			{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
		}

		for name, txIDs := range map[string][]string{
			"added identifier":    {"a", "b", "c"},
			"removed identifier":  {"a"},
			"replaced identifier": {"a", "x"},
		} {
			page, err := index.QueryTxs(TxQuery{TxIDs: txIDs, PageSize: 2, Cursor: first.NextCursor})
			if !errors.Is(err, ErrInvalidArgument) {
				t.Errorf("%s: err=%v, want ErrInvalidArgument", name, err)
			}
			if errors.Is(err, ErrQueryChanged) {
				t.Errorf("%s: condition change misreported as ErrQueryChanged", name)
			}
			if len(page.Hits) != 0 || page.NextCursor != "" {
				t.Errorf("%s: rejected call returned usable page data: %+v", name, page)
			}
		}

		// The original set on the original cursor still yields the next page,
		// unchanged, after every rejection.
		next, err := index.QueryTxs(TxQuery{TxIDs: []string{"b", "a", "a"}, PageSize: 2, Cursor: first.NextCursor})
		if err != nil {
			t.Fatalf("original set continuation refused after rejections: %v", err)
		}
		if !reflect.DeepEqual(next.Hits, expectedNext) {
			t.Fatalf("next page hits=%v, want %v", next.Hits, expectedNext)
		}
		if next.TotalMatches != 6 || next.MatchedBlocks != 3 || next.ToHeight != 3 {
			t.Fatalf("next page stats=%+v, want 6/3/to 3", next)
		}
	})

	t.Run("changed identifier absent from range", func(t *testing.T) {
		// Only "a" occurs anywhere in the pinned range, so filtering on
		// {"a"} and {"a","absent"} provably hits identical transactions.
		index := txChain(t,
			[]string{"a", "a"},
			[]string{"a"},
			[]string{"a", "a"},
		)
		plain := []string{"a"}
		withGhost := []string{"a", "ghost"}

		// Full single-page answers under both filters.
		fullPlain, err := index.QueryTxs(TxQuery{TxIDs: plain})
		if err != nil {
			t.Fatal(err)
		}
		fullGhost, err := index.QueryTxs(TxQuery{TxIDs: withGhost})
		if err != nil {
			t.Fatal(err)
		}
		// Sanity: the two answers really are the same hits and totals — so
		// accepting a swapped continuation would be a silent condition change.
		if !reflect.DeepEqual(fullGhost, fullPlain) {
			t.Fatalf("setup: filters hit different results despite absent id:\n%+v\n%+v",
				fullGhost, fullPlain)
		}

		first, err := index.QueryTxs(TxQuery{TxIDs: plain, PageSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		if first.NextCursor == "" {
			t.Fatal("expected a continuation cursor")
		}

		blocks, byHash, tip := snapshot(index)

		// Adding the absent identifier is still an argument error.
		page, err := index.QueryTxs(TxQuery{TxIDs: withGhost, PageSize: 2, Cursor: first.NextCursor})
		if !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("err=%v, want ErrInvalidArgument even though the added id never occurs", err)
		}
		if errors.Is(err, ErrQueryChanged) {
			t.Fatal("unchanged chain must not report ErrQueryChanged")
		}
		if len(page.Hits) != 0 || page.NextCursor != "" {
			t.Fatalf("rejected continuation leaked page data: %+v", page)
		}
		// Removing it back off a {"a","ghost"} cursor is equally invalid.
		ghostFirst, err := index.QueryTxs(TxQuery{TxIDs: withGhost, PageSize: 2})
		if err != nil {
			t.Fatal(err)
		}
		if page, err := index.QueryTxs(TxQuery{TxIDs: plain, PageSize: 2, Cursor: ghostFirst.NextCursor}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("dropping identifier: err=%v page=%+v, want ErrInvalidArgument", err, page)
		}

		// The failed continuation changed nothing: same chain, same next page.
		requireUnchanged(t, index, blocks, byHash, tip)
		next, err := index.QueryTxs(TxQuery{TxIDs: plain, PageSize: 2, Cursor: first.NextCursor})
		if err != nil {
			t.Fatalf("original continuation refused after condition change: %v", err)
		}
		wantNext := []TxHit{
			{Height: 2, BlockHash: "h2", TxID: "a", Position: 0},
			{Height: 3, BlockHash: "h3", TxID: "a", Position: 0},
		}
		if !reflect.DeepEqual(next.Hits, wantNext) {
			t.Fatalf("resumed hits=%v, want %v", next.Hits, wantNext)
		}
		if next.TotalMatches != 5 || next.MatchedBlocks != 3 || next.ToHeight != 3 {
			t.Fatalf("resumed stats=%+v, want 5/3/to 3", next)
		}

		// Walking the rest of the original cursor chain still stitches
		// together exactly the original single-page answer.
		full, err := index.QueryTxs(TxQuery{TxIDs: plain})
		if err != nil {
			t.Fatal(err)
		}
		rest := collectPages(t, index, TxQuery{TxIDs: plain, PageSize: 2, Cursor: next.NextCursor})
		stitched := append(append([]TxHit{}, first.Hits...), next.Hits...)
		for _, p := range rest {
			stitched = append(stitched, p.Hits...)
		}
		if !reflect.DeepEqual(stitched, full.Hits) {
			t.Fatalf("cursor chain after rejection differs from fresh filtered answer")
		}
	})
}

// TestHashTxSetEquivalenceClasses is a white-box guard over the cursor
// validation digest: equivalent spellings of one identifier set must digest
// identically, and distinct conditions — no filter, the empty identifier,
// boundary shifts, additions — must never collide.
func TestHashTxSetEquivalenceClasses(t *testing.T) {
	equivalent := [][]string{
		{"a", "b", "c"},
		{"c", "b", "a"},
		{"a", "a", "b", "c"},
		{"b", "c", "a", "c", "b", "a"},
	}
	first := hashTxSet(equivalent[0])
	for i, spelling := range equivalent[1:] {
		if got := hashTxSet(spelling); !reflect.DeepEqual(got, first) {
			t.Errorf("spelling %d (%v) digested differently from %v", i+1, spelling, equivalent[0])
		}
	}

	// nil and the empty slice both encode "no filter" and must agree; every
	// genuinely different condition must produce a distinct digest.
	emptyNil := hashTxSet(nil)
	if got := hashTxSet([]string{}); !reflect.DeepEqual(got, emptyNil) {
		t.Error("nil and []string{} describe different filters")
	}
	distinct := [][]string{
		{""},
		{" "},
		{"a"},
		{"ab", "c"},
		{"a", "bc"},
		{"a", "b"},
		{"a", "b", "c", ""},
		{"a", "b", "cc"},
		{"A", "b", "c"}, // case is part of the identifier
	}
	seen := map[string]string{string(emptyNil): "nil"}
	for _, ids := range distinct {
		digest := string(hashTxSet(ids))
		if digest == string(emptyNil) {
			t.Errorf("set %v collided with the no-filter digest", ids)
		}
		if other, dup := seen[digest]; dup {
			t.Errorf("sets %v and %v share a digest", other, ids)
		}
		seen[digest] = fmt.Sprint(ids)
	}
}
